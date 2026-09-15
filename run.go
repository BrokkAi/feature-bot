package featurebot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BrokkAi/acp-go/runner"
	"github.com/BrokkAi/feature-bot/internal/osrun"
)

type engine struct {
	config  Config
	source  issueSource
	log     *slog.Logger
	agent   func(Config) Agent
	now     func() time.Time
	observe func(Progress)
}

func Run(ctx context.Context, cfg Config, log *slog.Logger, once bool) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.GitHubRepo() == "" {
		return errors.New("GitHub repository required; set github.repo for a local mirror")
	}
	if log == nil {
		log = slog.Default()
	}
	unlock, err := lockConfig(cfg)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(cfg)
	if err != nil {
		return err
	}
	if s == nil {
		s = newState(cfg)
	}
	observe, _ := ctx.Value(progressKey{}).(func(Progress))
	e := engine{config: cfg, source: githubClient{cfg}, log: log, agent: func(c Config) Agent { return agentProcess{c, log} }, now: time.Now, observe: observe}
	e.report(s, "starting", "Loading saved scan")
	for {
		err := e.step(ctx, s, once)
		var setup *runner.SetupError
		if once || errors.As(err, &setup) || ctx.Err() != nil {
			return err
		}
		if err != nil {
			log.Error("Feature research paused", "error", err)
			phase := "paused"
			if s.Scan != nil {
				if s.Scan.Tries >= cfg.Attempts {
					phase = "blocked"
				}
				for _, c := range s.Scan.Candidates {
					if c.Status == "posting" {
						phase = "blocked"
					}
				}
			}
			e.report(s, phase, err.Error())
		} else if s.Scan != nil && s.Scan.Failure != "" {
			e.report(s, "paused", s.Scan.Failure)
		} else {
			e.report(s, "waiting", "Next scan")
		}
		if err := pause(ctx, time.Duration(cfg.Poll)); err != nil {
			return err
		}
	}
}
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (e engine) save(s *State) error {
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.report(s, "", "")
	return nil
}
func (e engine) step(ctx context.Context, s *State, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Reconcile unknown POST outcomes even when the scan attempt budget is exhausted.
	if s.Scan != nil {
		for _, c := range s.Scan.Candidates {
			if c.Status == "posting" {
				e.report(s, "reconciling", "Checking an interrupted issue publication")
				issues, err := e.source.issues(ctx)
				if err != nil {
					return err
				}
				found := false
				for _, i := range issues {
					if containsMarker(i, c.RequestID) {
						if err := validateCreated(e.config, c, &i); err != nil {
							return err
						}
						c.Status = "submitted"
						c.URL = i.URL
						found = true
						break
					}
				}
				if !found {
					return errors.New("issue creation outcome is unknown; no marker visible yet, refusing to repost (inspect saved state and GitHub)")
				}
				if err := e.save(s); err != nil {
					return err
				}
			}
		}
		if s.Scan.Discovered {
			pending := false
			for _, c := range s.Scan.Candidates {
				if c.Status == "pending" {
					pending = true
				}
			}
			if !pending {
				return e.finish(s)
			}
		}
		if s.Scan.Tries >= e.config.Attempts {
			return errors.New("scan attempt budget exhausted; inspect status and use bfb retry")
		}
		if !force && s.Scan.RetryAt.After(e.now()) {
			return nil
		}
	} else if !force && s.NextScan.After(e.now()) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(e.config.Timeout))
	defer cancel()
	e.report(s, "fetching", "Fetching "+e.config.Branch)
	g := checkout{e.config}
	if err := g.open(ctx); err != nil {
		return err
	}
	head, err := g.head(ctx)
	if err != nil {
		return err
	}
	if s.Scan != nil && s.Scan.Commit != head {
		for _, c := range s.Scan.Candidates {
			if c.Status == "pending" {
				c.Status = "stale"
			}
		}
		if err := e.finish(s); err != nil {
			return err
		}
	}
	if s.Scan == nil {
		var id [12]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		s.Scan = &Scan{Commit: head, Directory: filepath.Join(e.config.Directory+"-scans", fmt.Sprintf("scan-%x", id))}
		if err := e.save(s); err != nil {
			return err
		}
	}
	e.report(s, "preparing", "Preparing isolated scan workspace")
	w, err := g.prepare(ctx, s.Scan)
	if err != nil {
		return err
	}
	s.Scan.Failure = ""
	s.Scan.Tries++
	s.Scan.RetryAt = e.now().Add(time.Duration(e.config.RetryDelay))
	if err := e.save(s); err != nil {
		return err
	}
	e.report(s, "attempt", "Loading GitHub issue history")
	err = e.attempt(ctx, s, g, w)
	if err == nil || s.Scan == nil {
		return err
	}
	var setup *runner.SetupError
	if errors.As(err, &setup) {
		s.Scan.Tries--
	}
	s.Scan.Failure = err.Error()
	return errors.Join(err, e.save(s))
}

// execute runs one prompt and parses its receipt. A missing or truncated receipt
// at the end of an otherwise complete answer gets one recovery pass, in which the
// agent restates the receipt from its own text, before the attempt fails.
func (e engine) execute(ctx context.Context, s *State, phase string, a Agent, prompt, prefix string, parse func(string) error) error {
	text, err := a.Execute(ctx, prompt)
	if err != nil {
		return err
	}
	err = parse(text)
	var missing *receiptError
	if !errors.As(err, &missing) {
		return err
	}
	e.log.Warn("Recovering agent receipt", "receipt", prefix, "error", err)
	e.report(s, phase, "Recovering the "+prefix+" receipt")
	recovered, execErr := a.Execute(ctx, recoveryPrompt(prefix, text))
	if execErr != nil {
		return fmt.Errorf("%w; receipt recovery failed: %w", err, execErr)
	}
	if parseErr := parse(recovered); parseErr != nil {
		return fmt.Errorf("%w; receipt recovery failed: %v", err, parseErr)
	}
	e.log.Info("Recovered agent receipt", "receipt", prefix)
	return nil
}
func containsMarker(i Issue, key string) bool {
	return strings.Contains(i.Body, marker(key))
}

func (e engine) attempt(ctx context.Context, s *State, g, w checkout) error {
	issues, err := e.source.issues(ctx)
	if err != nil {
		return err
	}
	a := e.agent(w.config)
	if !s.Scan.Discovered {
		// Keep a complete, untruncated snapshot in the agent's workspace for discovery.
		file, err := os.CreateTemp(w.config.Directory, ".feature-bot-issues-*.json")
		if err != nil {
			return err
		}
		path := file.Name()
		defer os.Remove(path)
		encodeErr := json.NewEncoder(file).Encode(issues)
		if err := errors.Join(encodeErr, file.Close()); err != nil {
			return err
		}
		e.report(s, "investigating", "Researching new features")
		e.log.Info("Researching new features", "commit", s.Scan.Commit, "issues", len(issues), "directory", w.config.Directory)
		var r ScanResult
		if err := e.execute(ctx, s, "investigating", a, scanPrompt(e.config, s, path), "FEATURE_RESULT", func(text string) (err error) {
			r, err = parseScan(text, e.config.MaxIssues)
			return err
		}); err != nil {
			return err
		}
		e.report(s, "verifying", "Checking discovered findings and workspace")
		if err := w.verify(ctx, s.Scan); err != nil {
			return err
		}
		var candidates []*Candidate
		for _, f := range r.Findings {
			if err := w.verifyFiles(ctx, f); err != nil {
				return err
			}
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return err
			}
			candidates = append(candidates, &Candidate{RequestID: fmt.Sprintf("%x", id), Finding: f, Status: "pending"})
		}
		s.Scan.Summary = r.Summary
		s.Scan.Candidates = candidates
		s.Scan.Discovered = true
		if err := e.save(s); err != nil {
			return err
		}
	}
	for _, c := range s.Scan.Candidates {
		if c.Status != "pending" {
			continue
		}
		if err := e.reviewAndPublish(ctx, s, g, w, a, c); err != nil {
			return err
		}
	}
	return e.finish(s)
}

// Bound each review prompt without truncating any issue or discussion. Large individual
// issues are split into pieces, each retaining the issue number, title, and state.
func reviewChunks(issues []Issue) [][]Issue {
	const limit = 24000
	var chunks [][]Issue
	var chunk []Issue
	size := 0
	for _, i := range issues {
		text := i.Body
		for _, c := range i.Comments {
			text += "\n\nIssue comment:\n" + c
		}
		runes := []rune(text)
		for first := 0; first < len(runes) || first == 0; {
			last := min(first+limit, len(runes))
			part := i
			part.Body = string(runes[first:last])
			part.Comments = nil
			n := len([]rune(jsonContext(part)))
			if len(chunk) > 0 && (size+n > limit || len(chunk) >= 20 || chunk[len(chunk)-1].Number == i.Number) {
				chunks = append(chunks, chunk)
				chunk = nil
				size = 0
			}
			chunk = append(chunk, part)
			size += n
			first = last
			if first == len(runes) {
				break
			}
		}
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	if len(chunks) == 0 {
		chunks = append(chunks, []Issue{})
	}
	return chunks
}
func issueDigest(i Issue) [32]byte { b, _ := json.Marshal(i); return sha256.Sum256(b) }

func (e engine) reviewAndPublish(ctx context.Context, s *State, g, w checkout, a Agent, c *Candidate) error {
	e.report(s, "reviewing", "Refreshing issue history: "+c.Finding.Title)
	contextKey := fmt.Sprintf("%x", sha256.Sum256([]byte(jsonContext(c.Finding)+s.Scan.Commit)))
	if c.Checkpoint == nil || c.Checkpoint.Context != contextKey {
		c.Checkpoint = &ReviewCheckpoint{Context: contextKey}
	}
	if c.Checkpoint.Batches == nil {
		c.Checkpoint.Batches = map[string]bool{}
	}
	validated := c.Checkpoint.Validated
	// Refresh until every currently visible issue version has been reviewed. If the
	// history changes continuously, stop rather than publish against a stale snapshot.
	for refresh := 0; refresh < 5; refresh++ {
		issues, err := e.source.issues(ctx)
		if err != nil {
			return err
		}
		chunks := reviewChunks(issues)
		var pending [][]Issue
		for _, chunk := range chunks {
			if !c.Checkpoint.Batches[reviewBatchKey(chunk)] {
				pending = append(pending, chunk)
			}
		}
		if len(pending) > 0 || !validated {
			if len(pending) == 0 {
				pending = reviewChunks(nil)
			}
			chunks = pending
			for index, chunk := range chunks {
				e.report(s, "reviewing", fmt.Sprintf("%s (batch %d/%d)", c.Finding.Title, index+1, len(chunks)))
				r, err := e.executeReview(ctx, s, a, c.Finding, s.Scan.Commit, chunk, !validated)
				if err != nil {
					return err
				}
				if err := w.verify(ctx, s.Scan); err != nil {
					return err
				}
				if !validated {
					c.Review = r.Reason
					validated = true
				}
				if r.Verdict != "new" {
					c.Status = r.Verdict
					c.Review = r.Reason
					for _, i := range issues {
						if i.Number == r.Duplicate {
							c.URL = i.URL
						}
					}
					e.log.Info("Finding skipped", "title", c.Finding.Title, "status", c.Status, "reason", c.Review)
					return e.save(s)
				}
				c.Checkpoint.Validated = validated
				c.Checkpoint.Batches[reviewBatchKey(chunk)] = true
				if err := e.save(s); err != nil {
					return err
				}
			}
			continue
		}
		checked := map[int][32]byte{}
		for _, i := range issues {
			checked[i.Number] = issueDigest(i)
		}
		e.report(s, "verifying", "Verifying source and evidence: "+c.Finding.Title)
		if err := g.open(ctx); err != nil {
			return err
		}
		head, err := g.head(ctx)
		if err != nil {
			return err
		}
		if head != s.Scan.Commit {
			return errors.New("remote branch advanced during scan; next attempt will scan the new commit")
		}
		if err := w.verify(ctx, s.Scan); err != nil {
			return err
		}
		if len(e.config.Verify) > 0 {
			if _, err := osrun.Run(ctx, w.config.Directory, map[string]string{"FEATURE_COMMIT": s.Scan.Commit, "FEATURE_FINDING": jsonContext(c.Finding)}, e.config.Verify...); err != nil {
				return fmt.Errorf("operator verification: %w", err)
			}
			if err := w.verify(ctx, s.Scan); err != nil {
				return err
			}
		}
		// Git fetch and operator verification may take time. Re-read immediately
		// before POST and return to review if any issue was added or edited.
		latest, err := e.source.issues(ctx)
		if err != nil {
			return err
		}
		changed := false
		for _, i := range latest {
			if digest, ok := checked[i.Number]; !ok || digest != issueDigest(i) {
				changed = true
				break
			}
		}
		if changed {
			continue
		}
		if e.config.DryRun {
			c.Status = "dry_run"
			e.log.Info("Would create issue", "title", c.Finding.Title, "body", issueBody(c, s.Scan.Commit))
			return e.save(s)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Save BEFORE POST. Even a process crash must never turn into a blind retry.
		c.Status = "posting"
		if err := e.save(s); err != nil {
			c.Status = "pending"
			return err
		}
		e.report(s, "publishing", c.Finding.Title)
		i, err := e.source.create(ctx, c, s.Scan.Commit)
		if err != nil {
			var rejected *rejectedCreateError
			if errors.As(err, &rejected) {
				c.Status = "pending"
				return errors.Join(err, e.save(s))
			}
			return err
		}
		if err := validateCreated(e.config, c, i); err != nil {
			return err
		}
		c.Status = "submitted"
		c.URL = i.URL
		e.log.Info("Created feature proposal", "title", c.Finding.Title, "url", c.URL)
		return e.save(s)
	}
	return errors.New("issue history kept changing during review; refusing stale duplicate check")
}
func (e engine) finish(s *State) error {
	summary := s.Scan.Summary
	phase := "complete"
	if !s.Scan.Discovered {
		phase = "discarded"
	}
	for _, c := range s.Scan.Candidates {
		if c.Status == "stale" {
			phase = "discarded"
		}
	}
	s.History = append(s.History, s.Scan.Commit+": "+s.Scan.Summary)
	if len(s.History) > 20 {
		s.History = s.History[len(s.History)-20:]
	}
	s.Completed = append(s.Completed, s.Scan.Candidates...)
	s.Scan = nil
	s.NextScan = e.now().Add(time.Duration(e.config.Poll))
	if err := e.save(s); err != nil {
		return err
	}
	e.report(s, phase, summary)
	return nil
}

func reviewBatchKey(chunk []Issue) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(jsonContext(chunk))))
}

func (e engine) executeReview(ctx context.Context, s *State, a Agent, f Finding, commit string, chunk []Issue, validate bool) (Review, error) {
	prompt := reviewPrompt(f, commit, chunk, validate)
	for attempt := 0; attempt < 2; attempt++ {
		var r Review
		err := e.execute(ctx, s, "reviewing", a, prompt, "FEATURE_REVIEW", func(text string) (parseErr error) {
			r, parseErr = parseReview(text, chunk)
			return parseErr
		})
		if err == nil {
			return r, nil
		}
		var coverage *coverageError
		if !errors.As(err, &coverage) {
			return Review{}, err
		}
		if attempt == 1 {
			return r, fmt.Errorf("review coverage correction exhausted after 2 attempts: %w", err)
		}
		required := map[int]bool{}
		for _, i := range chunk {
			required[i.Number] = true
		}
		prompt = fmt.Sprintf("Correct the rejected review receipt. Validation error: %s\nRequired issue-number set: %s\nReview every supplied issue before attesting coverage; do not merely fill in numbers. Return a complete FEATURE_REVIEW receipt.\n\n", err, jsonContext(numberList(required))) + reviewPrompt(f, commit, chunk, validate)
	}
	panic("unreachable")
}
