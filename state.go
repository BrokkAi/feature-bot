package featurebot

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ReviewCheckpoint is scoped to the exact candidate and source revision. Batch
// hashes attest all text, including each fragment of a split issue.
type ReviewCheckpoint struct {
	Context   string          `json:"context"`
	Validated bool            `json:"validated"`
	Batches   map[string]bool `json:"batches"`
}
type Candidate struct {
	Checkpoint *ReviewCheckpoint `json:"review_checkpoint,omitempty"`
	RequestID  string            `json:"request_id"`
	Finding    Finding           `json:"finding"`
	Status     string            `json:"status"`
	URL        string            `json:"url,omitempty"`
	Review     string            `json:"review,omitempty"`
}
type Scan struct {
	Commit     string       `json:"commit"`
	Directory  string       `json:"directory"`
	Tries      int          `json:"tries"`
	RetryAt    time.Time    `json:"retry_at,omitempty"`
	Failure    string       `json:"failure,omitempty"`
	Summary    string       `json:"summary,omitempty"`
	Candidates []*Candidate `json:"candidates,omitempty"`
	Discovered bool         `json:"discovered"`
}
type State struct {
	Format    int          `json:"format"`
	Remote    string       `json:"remote"`
	Branch    string       `json:"branch"`
	Directory string       `json:"directory"`
	Repo      string       `json:"repo"`
	Host      string       `json:"host"`
	Scan      *Scan        `json:"scan,omitempty"`
	History   []string     `json:"history,omitempty"`
	Completed []*Candidate `json:"completed,omitempty"`
	NextScan  time.Time    `json:"next_scan,omitempty"`
}

func newState(cfg Config) *State {
	return &State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Repo: cfg.GitHubRepo(), Host: cfg.GitHub.Host}
}
func ReadState(cfg Config) (*State, error) {
	b, err := os.ReadFile(filepath.Join(cfg.StateDirectory, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("invalid saved state: %w", err)
	}
	if s.Format != 1 || s.Remote != cfg.Remote || s.Branch != cfg.Branch || s.Directory != cfg.Directory || s.Repo != cfg.GitHubRepo() || s.Host != cfg.GitHub.Host {
		return nil, errors.New("state version or repository identity differs from configuration")
	}
	candidates := append([]*Candidate(nil), s.Completed...)
	if s.Scan != nil {
		if !validCommit(s.Scan.Commit) || s.Scan.Tries < 0 || filepath.Dir(s.Scan.Directory) != cfg.Directory+"-scans" || !strings.HasPrefix(filepath.Base(s.Scan.Directory), "scan-") {
			return nil, errors.New("invalid saved scan")
		}
		candidates = append(candidates, s.Scan.Candidates...)
	}
	for _, c := range candidates {
		if c == nil || c.Finding.validate() != nil || (len(c.RequestID) != 32 || strings.Trim(c.RequestID, "0123456789abcdef") != "") {
			return nil, errors.New("invalid saved finding")
		}
		switch c.Status {
		case "pending", "posting", "submitted", "duplicate", "uncertain", "invalid", "dry_run", "stale":
		default:
			return nil, errors.New("invalid saved finding status")
		}
	}
	return &s, nil
}
func validCommit(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

func writeState(cfg Config, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(cfg.StateDirectory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(cfg.StateDirectory, "state.json")); err != nil {
		return err
	}
	dir, err := os.Open(cfg.StateDirectory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another process holds %s: %w", path, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func lockConfig(cfg Config) (func(), error) {
	// Coordinate repositories across branches, remotes and custom config paths on this account.
	base, err := stateHome()
	if err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(strings.ToLower(cfg.GitHub.Host + "/" + cfg.GitHubRepo())))
	repoUnlock, err := lockFile(filepath.Join(base, "feature-bot", "locks", fmt.Sprintf("%x.lock", key)))
	if err != nil {
		return nil, err
	}
	stateUnlock, err := lockFile(filepath.Join(cfg.StateDirectory, "daemon.lock"))
	if err != nil {
		repoUnlock()
		return nil, err
	}
	checkoutUnlock, err := lockFile(cfg.Directory + ".feature-bot.lock")
	if err != nil {
		stateUnlock()
		repoUnlock()
		return nil, err
	}
	return func() { checkoutUnlock(); stateUnlock(); repoUnlock() }, nil
}

func Retry(cfg Config) error {
	unlock, err := lockConfig(cfg)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(cfg)
	if err != nil {
		return err
	}
	if s == nil || s.Scan == nil {
		return errors.New("no saved scan to retry")
	}
	for _, c := range s.Scan.Candidates {
		if c.Status == "posting" {
			return errors.New("issue creation has an unknown outcome; run once to reconcile its marker, never blindly repost")
		}
	}
	s.Scan.Tries = 0
	s.Scan.RetryAt = time.Time{}
	s.NextScan = time.Time{}
	return writeState(cfg, s)
}
