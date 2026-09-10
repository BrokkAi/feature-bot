package featurebot

import (
	"context"
	"testing"
	"time"
)

func TestProgressTracksPublicationAndRestart(t *testing.T) {
	e, s, _, _, _ := fixture(t)
	var snapshots []Progress
	e.observe = func(p Progress) {
		snapshots = append(snapshots, p)
		if p.Phase == "publishing" {
			saved, err := ReadState(e.config)
			if err != nil || saved.Scan.Candidates[0].Status != "posting" {
				t.Fatalf("publication shown before durable intent: %v", err)
			}
		}
	}
	if err := e.step(context.Background(), s, true); err != nil {
		t.Fatal(err)
	}
	phases := map[string]bool{}
	var pending *Progress
	for i, p := range snapshots {
		phases[p.Phase] = true
		if p.Counts.Pending == 1 {
			pending = &snapshots[i]
		}
	}
	for _, phase := range []string{"fetching", "preparing", "attempt", "investigating", "reviewing", "verifying", "publishing", "complete"} {
		if !phases[phase] {
			t.Errorf("missing phase %s", phase)
		}
	}
	last := snapshots[len(snapshots)-1]
	if last.Counts.Found != 1 || last.Counts.Filed != 1 || last.Findings[0].URL == "" {
		t.Fatalf("incorrect final progress: %+v", last)
	}
	if pending == nil || pending.Findings[0].Status != "posting" && pending.Findings[0].Status != "pending" {
		t.Fatal("old snapshot changed with mutable candidate")
	}
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.step(context.Background(), saved, true); err != nil {
		t.Fatal(err)
	}
	last = snapshots[len(snapshots)-1]
	if last.Counts.Found != 2 || last.Counts.Filed != 1 || last.Counts.Duplicates != 1 {
		t.Fatalf("restart totals: %+v", last.Counts)
	}
	if last.Phase != "complete" {
		t.Fatalf("final phase %q", last.Phase)
	}
}

func TestProgressDryRunAndRejectedFindings(t *testing.T) {
	for _, status := range []string{"dry_run", "invalid", "uncertain"} {
		t.Run(status, func(t *testing.T) {
			e, s, f, a, _ := fixture(t)
			if status == "dry_run" {
				e.config.DryRun = true
			} else {
				a.onReview = func([]Issue) Review { return Review{Verdict: status, Reason: "Fixture rejected finding"} }
			}
			var p Progress
			e.observe = func(next Progress) { p = next }
			if err := e.step(context.Background(), s, true); err != nil {
				t.Fatal(err)
			}
			if p.Counts.Filed != 0 || f.creates != 0 || p.Findings[0].Status != status {
				t.Fatalf("nonpublished finding counted as filed: %+v", p)
			}
			if status == "dry_run" && p.Counts.DryRun != 1 || status != "dry_run" && p.Counts.Skipped != 1 {
				t.Fatalf("wrong totals: %+v", p.Counts)
			}
		})
	}
}

func TestProgressWaitMatchesPollingAndBoundsHistory(t *testing.T) {
	e, s, _, _, _ := fixture(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	e.config.Poll = Duration(30 * time.Minute)
	s.NextScan = now.Add(time.Minute)
	for i := 0; i < 250; i++ {
		s.Completed = append(s.Completed, &Candidate{Finding: finding(), Status: "submitted"})
	}
	var p Progress
	e.observe = func(next Progress) { p = next }
	e.report(s, "waiting", "Next scan")
	if !p.WakeAt.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("wake ignores poll interval: %s", p.WakeAt)
	}
	if len(p.Findings) != 200 || p.Counts.Found != 250 || p.Counts.Filed != 250 {
		t.Fatalf("bounded history changed totals: %d %+v", len(p.Findings), p.Counts)
	}
	s.NextScan = now.Add(time.Hour)
	e.report(s, "waiting", "Next scan")
	if !p.WakeAt.Equal(s.NextScan) {
		t.Fatal("next scan shown before eligibility")
	}
}
