package featurebot

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReportAllSavedCandidates(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Remote = "https://github.com/o/r.git"
	f := finding()
	f.UserProblem = "First paragraph\n\nSecond paragraph"
	f.AcceptanceCriteria = []string{"first line\ncontinuation", "second criterion"}
	s := &State{Directory: "private-checkout", History: []string{"private-transcript"}, Scan: &Scan{Commit: strings.Repeat("a", 40), Directory: "private-scan"}}
	for i := 0; i < 250; i++ {
		c := &Candidate{RequestID: "private-marker", Finding: f, Status: "dry_run", Review: "Review line one\n\nReview line two"}
		c.Finding.Title = fmt.Sprintf("Proposal %03d", i)
		s.Completed = append(s.Completed, c)
	}
	for _, status := range []string{"pending", "posting", "submitted", "duplicate", "uncertain", "invalid", "stale"} {
		s.Scan.Candidates = append(s.Scan.Candidates, &Candidate{Finding: f, Status: status, URL: "https://github.com/o/r/issues/1"})
	}
	var out strings.Builder
	if err := WriteReport(&out, cfg, s, ""); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "\n## ") != 257 {
		t.Fatal("report truncated candidates")
	}
	for _, want := range []string{"Repository: github.com/o/r", "Branch: master", "Proposal 000", "Proposal 249", f.UserProblem, f.CurrentWorkflow, f.ProposedSolution, f.UserValue, f.Scope, "- first line\n  continuation\n- second criterion", "- README.md", f.Evidence[0], "Review line one\n\nReview line two", "https://github.com/o/r/issues/1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, unwanted := range []string{"private-", s.Scan.Commit, "<!-- feature-bot:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("exposed metadata %q", unwanted)
		}
	}
	for _, status := range []string{"dry_run", "pending", "posting", "submitted", "duplicate", "uncertain", "invalid", "stale"} {
		out.Reset()
		if err := WriteReport(&out, cfg, s, status); err != nil {
			t.Fatal(err)
		}
		want := 1
		if status == "dry_run" {
			want = 250
		}
		if strings.Count(out.String(), "\nStatus: ") != want || strings.Count(out.String(), "\nStatus: "+status+"\n") != want {
			t.Fatalf("incorrect %s filter", status)
		}
	}
}

func TestReportEmptyAndInvalidStatus(t *testing.T) {
	for _, s := range []*State{nil, {}, {Completed: []*Candidate{{Finding: finding(), Status: "pending"}}}} {
		var out strings.Builder
		if err := WriteReport(&out, DefaultConfig(), s, "dry_run"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "No saved findings") {
			t.Fatal(out.String())
		}
	}
	var out strings.Builder
	if err := WriteReport(&out, DefaultConfig(), nil, "bogus"); err == nil || !strings.Contains(err.Error(), "dry_run") || out.Len() != 0 {
		t.Fatalf("invalid status: %v", err)
	}
	sentinel := errors.New("write failed")
	if err := WriteReport(reportErrorWriter{sentinel}, DefaultConfig(), nil, ""); !errors.Is(err, sentinel) {
		t.Fatalf("write error: %v", err)
	}
}

type reportErrorWriter struct{ err error }

func (w reportErrorWriter) Write([]byte) (int, error) { return 0, w.err }
