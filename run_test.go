package featurebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/runner"
	"github.com/BrokkAi/feature-bot/internal/osrun"
)

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	p, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func writeTestFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}
func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	env := map[string]string{"GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.com", "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.com", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}
	out, err := osrun.Run(context.Background(), dir, env, append([]string{"git"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func finding() Finding {
	return Finding{
		Title:              "Export a filtered task list as CSV",
		UserProblem:        "Task coordinators cannot bring filtered task lists into a spreadsheet",
		CurrentWorkflow:    "Read the task list and manually copy each visible row into a spreadsheet",
		ProposedSolution:   "Add a CSV export command that uses the same filters and ordering as task list",
		UserValue:          "Lets coordinators prepare spreadsheet reports without manually copying tasks; demand is inferred from the documented coordination workflow",
		Scope:              "Reuse list filtering and task fields for local CSV output; exclude imports, scheduling and cloud integrations",
		AcceptanceCriteria: []string{"The CSV contains only tasks matching the supplied filters in list order", "Fields containing commas, quotes or newlines round-trip through a CSV parser", "An empty list exports only column headers"},
		Files:              []string{"README.md"},
		Evidence:           []string{"README.md describes task coordination and a filtered text-only list; its complete command reference has no export command"},
	}
}

type fakeSource struct {
	cfg            Config
	items          []Issue
	reads, creates int
	onRead         func(*fakeSource) error
	createErr      error
	lost           bool
}

func (f *fakeSource) issues(context.Context) ([]Issue, error) {
	f.reads++
	if f.onRead != nil {
		if err := f.onRead(f); err != nil {
			return nil, err
		}
	}
	return append([]Issue(nil), f.items...), nil
}
func (f *fakeSource) create(_ context.Context, c *Candidate, commit string) (*Issue, error) {
	f.creates++
	if f.createErr != nil {
		return nil, f.createErr
	}
	i := Issue{Number: 100 + f.creates, Title: c.Finding.Title, Body: issueBody(c, commit), State: "open"}
	i.URL = fmt.Sprintf("https://github.com/o/r/issues/%d", i.Number)
	f.items = append(f.items, i)
	if f.lost {
		return nil, errors.New("connection lost after POST")
	}
	return &i, nil
}

type fakeAgent struct {
	scans, reviews int
	scanPrompt     string
	findings       []Finding
	onReview       func([]Issue) Review
	onScan         func()
	err            error
}

func (a *fakeAgent) Execute(_ context.Context, prompt string) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	if strings.Contains(prompt, "Scan context (data):") {
		a.scans++
		a.scanPrompt = prompt
		if a.onScan != nil {
			a.onScan()
		}
		fs := a.findings
		if fs == nil {
			fs = []Finding{finding()}
		}
		return "FEATURE_RESULT " + jsonContextCompact(ScanResult{Summary: "Inspected task coordination workflow and list command; researched bounded CSV export", Findings: fs}), nil
	}
	a.reviews++
	var data struct{ Issues []Issue }
	if err := json.Unmarshal([]byte(strings.Split(prompt, "Review context (data):\n")[1]), &data); err != nil {
		return "", err
	}
	r := Review{Verdict: "new", Reason: "Independently checked README.md's complete text-only list reference; CSV export adds a distinct bounded capability with testable criteria", Checked: []int{}}
	for _, i := range data.Issues {
		r.Checked = append(r.Checked, i.Number)
		// Simulated LLM verdict for this fixture's one known user goal.
		if strings.Contains(i.Body, finding().UserProblem) {
			r.Verdict = "duplicate"
			r.Duplicate = i.Number
			r.Reason = "Same spreadsheet export capability"
		}
	}
	if a.onReview != nil {
		custom := a.onReview(data.Issues)
		custom.Checked = r.Checked
		r = custom
	}
	return "FEATURE_REVIEW " + jsonContextCompact(r), nil
}
func jsonContextCompact(v any) string { b, _ := json.Marshal(v); return string(b) }
func fixture(t *testing.T) (engine, *State, *fakeSource, *fakeAgent, string) {
	t.Helper()
	source, remote := discoveryRepo(t)
	dir := canonicalTestDir(t)
	cfg := DefaultConfig()
	cfg.Remote = remote
	cfg.Branch = "main"
	cfg.GitHub.Repo = "o/r"
	cfg.Directory = filepath.Join(dir, "checkout")
	cfg.StateDirectory = filepath.Join(dir, "state")
	if err := os.MkdirAll(cfg.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	f := &fakeSource{cfg: cfg}
	a := &fakeAgent{}
	e := engine{config: cfg, source: f, log: slog.New(slog.NewTextHandler(io.Discard, nil)), agent: func(Config) Agent { return a }, now: time.Now}
	return e, newState(cfg), f, a, source
}
func TestScanPublishAndRestartDoesNotDuplicate(t *testing.T) {
	e, s, f, a, source := fixture(t)
	writeTestFile(t, filepath.Join(source, "unfinished.txt"), "user work")
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || a.reviews != 1 || len(s.Completed) != 1 || s.Completed[0].Status != "submitted" {
		t.Fatalf("unexpected completion %+v, creates %d, reviews %d", s, f.creates, a.reviews)
	}
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.step(context.Background(), saved, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 {
		t.Fatal("duplicate created after restart")
	}
	if got, _ := os.ReadFile(filepath.Join(source, "unfinished.txt")); string(got) != "user work" {
		t.Fatal("source checkout changed")
	}
}
func TestSemanticClosedDuplicate(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	f.items = []Issue{{Number: 5, Title: "Download rows for reporting", Body: "Different wording", State: "closed", Comments: []string{"Spreadsheet export was rejected; use external tools to transform the list output"}}}
	a.onReview = func(issues []Issue) Review {
		if len(issues) != 1 || len(issues[0].Comments) != 0 || !strings.Contains(issues[0].Body, "Spreadsheet export was rejected") {
			t.Fatalf("discussion missing from review: %+v", issues)
		}
		return Review{Verdict: "duplicate", Reason: "Same user goal and CSV capability as the rejected spreadsheet export proposal", Duplicate: 5}
	}
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 || s.Completed[0].Status != "duplicate" {
		t.Fatal("closed semantic duplicate was filed")
	}
}

func TestLLMDecidesEvenWhenTitlesMatch(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	f.items = []Issue{{Number: 12, Title: finding().Title, Body: "An unrelated task-import proposal with an inaccurate title", State: "open"}}
	a.onReview = func(issues []Issue) Review {
		if len(issues) != 1 || issues[0].Number != 12 {
			t.Fatalf("LLM did not receive existing issue: %+v", issues)
		}
		return Review{Verdict: "new", Reason: "Same title, but importing tasks and exporting a filtered report serve different user goals"}
	}
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || a.reviews != 1 {
		t.Fatal("title matching overrode the LLM's decision")
	}
}
func TestNewIssueDuringReviewIsCompared(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	a.onReview = func(issues []Issue) Review {
		if len(issues) == 0 {
			f.items = []Issue{{Number: 9, Title: "Save task rows to a spreadsheet", State: "open"}}
			return Review{Verdict: "new", Reason: "Verified gap and implementation fit"}
		}
		return Review{Verdict: "duplicate", Reason: "New human proposal covers the same export capability", Duplicate: 9}
	}
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 || a.reviews != 2 {
		t.Fatalf("missed concurrent issue: creates %d, reviews %d", f.creates, a.reviews)
	}
}
func TestIssueAppearsImmediatelyBeforePost(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	f.onRead = func(f *fakeSource) error {
		if f.reads == 4 {
			f.items = []Issue{{Number: 8, Title: finding().Title, Body: finding().UserProblem, State: "open"}}
		}
		return nil
	}
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 {
		t.Fatal("failed to refresh before POST")
	}
}
func TestLostCreateResponseReconcilesAtAttemptLimit(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	e.config.Attempts = 1
	f.lost = true
	if err := e.step(context.Background(), s, true); err == nil {
		t.Fatal("expected transport error")
	}
	if s.Scan.Candidates[0].Status != "posting" {
		t.Fatal("POST intent was not retained")
	}
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.step(context.Background(), saved, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || a.scans != 1 || saved.Scan != nil || saved.Completed[0].Status != "submitted" {
		t.Fatal("lost response was not reconciled without a second POST")
	}
}
func TestUnknownCreateNeverBlindlyRetries(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	f.createErr = errors.New("unknown outcome")
	if err := e.step(context.Background(), s, true); err == nil {
		t.Fatal("expected error")
	}
	for range 3 {
		if err := e.step(context.Background(), s, true); err == nil {
			t.Fatal("unknown outcome accepted")
		}
	}
	if f.creates != 1 {
		t.Fatal("POST was retried")
	}
	t.Setenv("XDG_STATE_HOME", canonicalTestDir(t))
	if Retry(e.config) == nil {
		t.Fatal("retry allowed ambiguous POST")
	}
}
func TestDryRunThenRealRun(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	e.config.DryRun = true
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 || s.Completed[0].Status != "dry_run" {
		t.Fatal("dry run published")
	}
	e.config.DryRun = false
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 {
		t.Fatal("dry run suppressed later publication")
	}
}
func TestIncompleteHistoryAndUncertainReviewFailClosed(t *testing.T) {
	for _, mode := range []string{"history", "uncertain", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			e, s, f, a, _ := fixture(t)
			if mode == "history" {
				f.onRead = func(*fakeSource) error { return errors.New("GitHub unavailable") }
			} else {
				a.onReview = func([]Issue) Review {
					return Review{Verdict: mode, Reason: "Cannot establish a distinct valuable and feasible feature"}
				}
			}
			err := e.step(context.Background(), s, true)
			if mode == "history" && err == nil {
				t.Fatal("history error hidden")
			}
			if mode != "history" && err != nil {
				t.Fatal(err)
			}
			if f.creates != 0 {
				t.Fatal("published without complete evidence")
			}
		})
	}
}

func TestFeatureReviewOutcomesPreserveRejectedAndUncertainProposals(t *testing.T) {
	for _, tc := range []struct {
		name, verdict, reason string
	}{
		{"novel capability", "new", "CSV export fits task coordination and is absent from the current command reference"},
		{"bug fix", "invalid", "The proposal only repairs incorrect list filtering and adds no capability"},
		{"refactor only", "invalid", "The proposal only reorganizes internals without a user-facing capability"},
		{"already supported", "invalid", "The current command reference already supports this workflow"},
		{"uncertain demand", "uncertain", "Repository evidence does not establish the proposed user value"},
		{"uncertain feasibility", "uncertain", "The proposed integration relies on an unverified extension point"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, s, source, agent, _ := fixture(t)
			agent.onReview = func([]Issue) Review { return Review{Verdict: tc.verdict, Reason: tc.reason} }
			if err := e.step(context.Background(), s, true); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantCreates := tc.verdict, 0
			if tc.verdict == "new" {
				wantStatus, wantCreates = "submitted", 1
			}
			if source.creates != wantCreates || len(s.Completed) != 1 || s.Completed[0].Status != wantStatus || s.Completed[0].Review != tc.reason {
				t.Fatalf("review outcome lost or incorrectly published: creates=%d, completed=%+v", source.creates, s.Completed)
			}
			saved, err := ReadState(e.config)
			if err != nil || len(saved.Completed) != 1 || saved.Completed[0].Status != wantStatus || saved.Completed[0].Review != tc.reason {
				t.Fatalf("review evidence did not survive restart: %+v, %v", saved, err)
			}
		})
	}
}
func TestScanDoesNotPublishAgainstAdvancedBranch(t *testing.T) {
	e, s, f, a, source := fixture(t)
	a.onScan = func() {
		localGit(t, source, "switch", "main")
		writeTestFile(t, filepath.Join(source, "next.txt"), "new commit")
		localGit(t, source, "add", ".")
		localGit(t, source, "commit", "-m", "advance")
		localGit(t, source, "push", "origin", "main")
	}
	if err := e.step(context.Background(), s, true); err == nil || !strings.Contains(err.Error(), "advanced") {
		t.Fatalf("expected stale branch error: %v", err)
	}
	if f.creates != 0 {
		t.Fatal("published stale finding")
	}
}
func TestChangedSourceAndMissingSourceRefused(t *testing.T) {
	for _, mode := range []string{"edit", "missing"} {
		t.Run(mode, func(t *testing.T) {
			e, s, f, a, _ := fixture(t)
			if mode == "missing" {
				bad := finding()
				bad.Files = []string{"does-not-exist.go"}
				a.findings = []Finding{bad}
			} else {
				e.agent = func(cfg Config) Agent {
					a.onScan = func() { writeTestFile(t, filepath.Join(cfg.Directory, "README.md"), "edited source") }
					return a
				}
			}
			if err := e.step(context.Background(), s, true); err == nil {
				t.Fatal("invalid source accepted")
			}
			if f.creates != 0 {
				t.Fatal("published invalid source evidence")
			}
		})
	}
}
func TestSetupFailureDoesNotConsumeAttempt(t *testing.T) {
	e, s, _, a, _ := fixture(t)
	a.err = &runner.SetupError{Err: errors.New("missing model")}
	if err := e.step(context.Background(), s, true); err == nil {
		t.Fatal("setup error hidden")
	}
	if s.Scan.Tries != 0 {
		t.Fatal("setup failure consumed attempt")
	}
}
func TestBatchDuplicatesAndZeroFindings(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			e, s, f, a, _ := fixture(t)
			if empty {
				a.findings = []Finding{}
			} else {
				a.findings = []Finding{finding(), finding()}
			}
			if err := e.step(context.Background(), s, true); err != nil {
				t.Fatal(err)
			}
			want := 1
			if empty {
				want = 0
			}
			if f.creates != want {
				t.Fatalf("created %d, want %d", f.creates, want)
			}
		})
	}
}
func TestOperatorVerifierFailureStopsPublication(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	e.config.Verify = []string{"sh", "-c", "test -n \"$FEATURE_COMMIT\" && test -n \"$FEATURE_FINDING\" && exit 7"}
	if err := e.step(context.Background(), s, true); err == nil || !strings.Contains(err.Error(), "operator verification") {
		t.Fatalf("wrong error %v", err)
	}
	if f.creates != 0 {
		t.Fatal("failed verification still published")
	}
}

func TestResearchControlsFreshScan(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			e, s, f, a, _ := fixture(t)
			e.config.Focus = "onboarding"
			e.config.MaxIssues = 1
			a.findings = make([]Finding, count)
			for i := range a.findings {
				a.findings[i] = finding()
			}
			err := e.step(context.Background(), s, true)
			if !strings.Contains(a.scanPrompt, `"Focus": "onboarding"`) || !strings.Contains(a.scanPrompt, `"MaxIssues": 1`) {
				t.Fatalf("research controls missing from prompt: %s", a.scanPrompt)
			}
			if count > 1 {
				if err == nil || !strings.Contains(err.Error(), "max_issues") || a.reviews != 0 || f.creates != 0 {
					t.Fatalf("excess receipt: err=%v reviews=%d creates=%d", err, a.reviews, f.creates)
				}
			} else if err != nil || f.creates != count {
				t.Fatalf("valid receipt: err=%v creates=%d", err, f.creates)
			}
		})
	}
}

func TestResearchControlsPreserveDiscoveredCandidates(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	second := finding()
	second.Title = "Export selected tasks"
	a.findings = []Finding{finding(), second}
	f.createErr = &rejectedCreateError{errors.New("permission denied")}
	if err := e.step(context.Background(), s, true); err == nil {
		t.Fatal("expected publication failure")
	}
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Scan.Discovered || len(saved.Scan.Candidates) != 2 {
		t.Fatal("discovery was not saved")
	}
	ids := []string{saved.Scan.Candidates[0].RequestID, saved.Scan.Candidates[1].RequestID}
	e.config.Focus = "onboarding"
	e.config.MaxIssues = 1
	f.createErr = nil
	if err := e.step(context.Background(), saved, true); err != nil {
		t.Fatal(err)
	}
	if a.scans != 1 || len(saved.Completed) != 2 || a.reviews < 2 {
		t.Fatalf("saved work changed: scans=%d reviews=%d completed=%d", a.scans, a.reviews, len(saved.Completed))
	}
	for i, c := range saved.Completed {
		if c.RequestID != ids[i] {
			t.Fatal("saved candidate replaced")
		}
	}
}

// A truncated receipt at the end of a complete research pass must not discard
// the pass. The agent restates its receipt once; only a second failure pauses.
type truncatingAgent struct {
	fakeAgent
	recoveries    int
	unrecoverable bool
	recoveryErr   error
}

func (a *truncatingAgent) Execute(ctx context.Context, prompt string) (string, error) {
	if strings.Contains(prompt, "Previous answer (data):") {
		a.recoveries++
		if a.recoveryErr != nil {
			return "", a.recoveryErr
		}
		if !strings.Contains(prompt, "FEATURE_RESULT receipt, for example") || !strings.Contains(prompt, "Restate that receipt") {
			return "", errors.New("recovery prompt lacks receipt guidance: " + prompt)
		}
		if a.unrecoverable {
			return "UNRECOVERABLE", nil
		}
		answer := strings.Split(prompt, "Previous answer (data):\n")[1]
		return answer + "]}", nil
	}
	text, err := a.fakeAgent.Execute(ctx, prompt)
	if strings.HasPrefix(text, "FEATURE_RESULT ") {
		// Reproduces a codex-acp answer that lost the final closing brackets.
		return "Checked the command path.\n" + strings.TrimSuffix(text, "]}"), nil
	}
	return text, err
}
func TestTruncatedScanReceiptIsRecovered(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	a := &truncatingAgent{}
	e.agent = func(Config) Agent { return a }
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if a.recoveries != 1 || a.scans != 1 || f.creates != 1 || len(s.Completed) != 1 || s.Completed[0].Status != "submitted" {
		t.Fatalf("recovery did not complete the scan: recoveries=%d scans=%d creates=%d state=%+v", a.recoveries, a.scans, f.creates, s)
	}
}
func TestUnrecoverableReceiptPausesWithDetail(t *testing.T) {
	e, s, _, _, _ := fixture(t)
	a := &truncatingAgent{unrecoverable: true}
	e.agent = func(Config) Agent { return a }
	err := e.step(context.Background(), s, true)
	if err == nil || a.recoveries != 1 {
		t.Fatalf("expected one failed recovery, got recoveries=%d err=%v", a.recoveries, err)
	}
	for _, want := range []string{"did not finish with a FEATURE_RESULT receipt", "truncated or invalid", "receipt recovery failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if s.Scan == nil || s.Scan.Discovered || s.Scan.Tries != 1 || !strings.Contains(s.Scan.Failure, "receipt recovery failed") {
		t.Fatalf("attempt not recorded for retry: %+v", s.Scan)
	}
}

func TestRecoverySetupFailureRefundsAttempt(t *testing.T) {
	e, s, _, _, _ := fixture(t)
	a := &truncatingAgent{recoveryErr: &runner.SetupError{Err: errors.New("missing model")}}
	e.agent = func(Config) Agent { return a }
	err := e.step(context.Background(), s, true)
	var setup *runner.SetupError
	if !errors.As(err, &setup) || a.recoveries != 1 || s.Scan.Tries != 0 {
		t.Fatalf("recovery setup error consumed attempt: err=%v recoveries=%d scan=%+v", err, a.recoveries, s.Scan)
	}
}
