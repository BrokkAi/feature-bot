package featurebot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type reviewAgentFunc func(context.Context, string) (string, error)

func (f reviewAgentFunc) Execute(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

func TestCoverageDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		checked []int
		want    string
	}{
		{[]int{1}, "expected count=3; missing=[2 3]; repeated=[]; unexpected=[]"},
		{[]int{4, 1, 1, 4}, "expected count=3; missing=[2 3]; repeated=[1 4]; unexpected=[4]"},
	} {
		_, err := parseReview("FEATURE_REVIEW "+jsonContextCompact(Review{Verdict: "new", Reason: "Checked", Checked: tc.checked}), []Issue{{Number: 3}, {Number: 1}, {Number: 2}})
		var coverage *coverageError
		if !errors.As(err, &coverage) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("diagnostic: %v", err)
		}
	}
	issues := make([]Issue, 1000)
	for i := range issues {
		issues[i].Number = i + 1
	}
	_, err := parseReview(`FEATURE_REVIEW {"verdict":"new","reason":"Checked","checked":[]}`, issues)
	if err == nil || len(err.Error()) > 300 || !strings.Contains(err.Error(), "980 more") {
		t.Fatalf("unbounded diagnostic: %v", err)
	}
}

func TestCorrectiveReview(t *testing.T) {
	for _, verdict := range []string{"new", "duplicate", "invalid", "uncertain", "malformed"} {
		t.Run(verdict, func(t *testing.T) {
			e, s, source, base, _ := fixture(t)
			source.items = []Issue{{Number: 1}, {Number: 2}}
			calls := 0
			e.agent = func(Config) Agent {
				return reviewAgentFunc(func(ctx context.Context, prompt string) (string, error) {
					if strings.Contains(prompt, "Scan context (data):") {
						return base.Execute(ctx, prompt)
					}
					calls++
					r := Review{Verdict: "new", Reason: "Independently reviewed", Checked: []int{1}}
					if calls == 2 {
						for _, required := range []string{"missing=[2]", "Required issue-number set: [\n  1,\n  2\n]", "Review every supplied issue", "Independently inspect code"} {
							if !strings.Contains(prompt, required) {
								t.Errorf("correction missing %q", required)
							}
						}
						if verdict != "malformed" {
							r.Checked = []int{1, 2}
							r.Verdict = verdict
						}
						if verdict == "duplicate" {
							r.Duplicate = 2
						}
					}
					return "FEATURE_REVIEW " + jsonContextCompact(r), nil
				})
			}
			err := e.step(context.Background(), s, true)
			if calls != 2 {
				t.Fatalf("attempts=%d", calls)
			}
			if verdict == "malformed" {
				if err == nil || !strings.Contains(err.Error(), "correction exhausted") || !strings.Contains(err.Error(), "missing=[2]") {
					t.Fatalf("error: %v", err)
				}
				saved, readErr := ReadState(e.config)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if saved.Scan.Candidates[0].Status != "pending" || len(saved.Scan.Candidates[0].Checkpoint.Batches) != 0 || source.creates != 0 {
					t.Fatal("malformed receipt accepted or candidate lost")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if verdict == "new" {
					want = 1
				}
				if source.creates != want {
					t.Fatalf("creates=%d", source.creates)
				}
			}
		})
	}
}

func TestReviewCheckpointRestart(t *testing.T) {
	for _, change := range []string{"none", "issue", "comment", "candidate", "workspace"} {
		t.Run(change, func(t *testing.T) {
			e, s, source, base, _ := fixture(t)
			// A single issue spans three distinct batches. The second fails after the
			// first has been durably saved; no fragment may attest the entire issue.
			source.items = []Issue{{Number: 7, Body: strings.Repeat("a", 24000) + strings.Repeat("b", 24000) + "last"}}
			calls := 0
			e.agent = func(Config) Agent {
				return reviewAgentFunc(func(ctx context.Context, prompt string) (string, error) {
					if strings.Contains(prompt, "Scan context (data):") {
						return base.Execute(ctx, prompt)
					}
					calls++
					if calls == 2 {
						return "", errors.New("interrupted")
					}
					return base.Execute(ctx, prompt)
				})
			}
			if err := e.step(context.Background(), s, true); err == nil {
				t.Fatal("expected interruption")
			}
			saved, err := ReadState(e.config)
			if err != nil {
				t.Fatal(err)
			}
			c := saved.Scan.Candidates[0]
			if len(c.Checkpoint.Batches) != 1 || source.creates != 0 {
				t.Fatal("checkpoint not saved safely")
			}
			switch change {
			case "issue":
				source.items[0].Body = "edited" + source.items[0].Body
			case "comment":
				source.items[0].Comments = []string{"New discussion"}
			case "candidate":
				c.Finding.Scope += "; additional scope"
			case "workspace":
				writeTestFile(t, saved.Scan.Directory+"/README.md", "changed source")
			}
			saved.Scan.RetryAt = time.Time{}
			e.agent = func(Config) Agent { return base }
			before := base.reviews
			err = e.step(context.Background(), saved, true)
			if change == "workspace" {
				if err == nil || source.creates != 0 {
					t.Fatal("published with changed workspace")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if change == "issue" || change == "candidate" {
				want = 3
			}
			if base.reviews-before != want || source.creates != 1 || base.scans != 1 {
				t.Fatalf("reviews=%d want=%d creates=%d scans=%d", base.reviews-before, want, source.creates, base.scans)
			}
		})
	}
}

func TestCheckpointRechecksIssueEditedBeforePost(t *testing.T) {
	e, s, source, a, _ := fixture(t)
	source.items = []Issue{{Number: 8, Body: "Original proposal"}}
	source.onRead = func(f *fakeSource) error {
		if f.reads == 4 {
			f.items[0].Comments = []string{finding().UserProblem}
		}
		return nil
	}
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	if source.creates != 0 || a.reviews != 2 || s.Completed[0].Status != "duplicate" {
		t.Fatal("published against old issue version")
	}
}
