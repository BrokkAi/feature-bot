package main

import (
	"bytes"
	"context"
	"encoding/json"
	bot "github.com/BrokkAi/feature-bot"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIOverridesAndInterspersedFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"remote":"https://github.com/o/r.git","agent":{"command":["sh"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	called := false
	run := func(_ context.Context, c bot.Config, _ *slog.Logger, once bool) error {
		called = true
		if c.Poll != bot.Duration(2*time.Minute) || !once || c.MaxIssues != 7 || !c.DryRun || c.Focus != "parser" || c.Agent.Model != "fixture" || c.Agent.Effort != "low" || len(c.Labels) != 2 {
			t.Fatalf("wrong settings %+v", c)
		}
		return nil
	}
	err := executeWithRun(context.Background(), []string{"once", "--config", path, "--max-issues", "7", "--poll", "2m", "--dry-run", "--focus", "parser", "--model", "fixture", "--effort", "low", "--label", "enhancement", "--label", "ready"}, slog.New(slog.NewTextHandler(io.Discard, nil)), run)
	if err != nil || !called {
		t.Fatalf("CLI %v %v", called, err)
	}
}

func TestVersionCommand(t *testing.T) {
	original := version
	version = "v1.2.3"
	t.Cleanup(func() { version = original })

	var output strings.Builder
	if err := versionCommand(nil, &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "v1.2.3\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
	if err := versionCommand([]string{"extra"}, &output); err == nil {
		t.Fatal("version accepted an argument")
	}
}

func TestReportCLIReadsStateOffline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"remote":"https://github.com/o/r.git","agent":{"command":["missing-acp"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := bot.ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	candidate := func(title, status string) *bot.Candidate {
		return &bot.Candidate{RequestID: strings.Repeat("b", 32), Status: status, Review: "review\n\nmore review", Finding: bot.Finding{
			Title: title, UserProblem: "problem\n\nmore problem", CurrentWorkflow: "workflow", ProposedSolution: "solution", UserValue: "value", Scope: "scope", AcceptanceCriteria: []string{"criterion"}, Files: []string{"README.md"}, Evidence: []string{"evidence"},
		}}
	}
	state := bot.State{Format: 1, Remote: cfg.Remote, Branch: cfg.Branch, Directory: cfg.Directory, Repo: cfg.GitHubRepo(), Host: cfg.GitHub.Host,
		Completed: []*bot.Candidate{candidate("Saved dry run", "dry_run")}, Scan: &bot.Scan{Commit: strings.Repeat("a", 40), Directory: filepath.Join(cfg.Directory+"-scans", "scan-fixture"), Candidates: []*bot.Candidate{candidate("Active pending", "pending")}}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cfg.StateDirectory, "state.json")
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	// No external commands (including gh, Git, npx, or ACP) can be found.
	t.Setenv("PATH", t.TempDir())
	run := func(context.Context, bot.Config, *slog.Logger, bool) error {
		t.Fatal("report invoked research runner")
		return nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, filter := range []string{"", "dry_run", "unsupported"} {
		args := []string{"report", "--config", path}
		if filter != "" {
			args = append(args, "--status", filter)
		}
		output, err := os.CreateTemp(dir, "stdout-")
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = output
		runErr := func() error {
			defer func() { os.Stdout = original }()
			return executeWithRun(context.Background(), args, log, run)
		}()
		if err := output.Close(); err != nil {
			t.Fatal(err)
		}
		result, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		if filter == "unsupported" {
			if runErr == nil || !strings.Contains(runErr.Error(), "dry_run") || len(result) != 0 {
				t.Fatalf("invalid filter: %v %s", runErr, result)
			}
		} else {
			if runErr != nil {
				t.Fatal(runErr)
			}
			if !strings.Contains(string(result), "Saved dry run") || strings.Contains(string(result), "Active pending") != (filter == "") {
				t.Fatalf("wrong report: %s", result)
			}
		}
		after, err := os.ReadFile(statePath)
		if err != nil || !bytes.Equal(after, data) {
			t.Fatalf("state changed: %v", err)
		}
		entries, err := os.ReadDir(cfg.StateDirectory)
		if err != nil || len(entries) != 1 {
			t.Fatalf("report created state files: %v", err)
		}
	}
}

func TestReportCLIExplicitBranchWithoutTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	output, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	original := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = original }()
	err = executeWithRun(context.Background(), []string{"report", "https://github.com/o/r.git", "--branch", "master"}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(context.Context, bot.Config, *slog.Logger, bool) error { t.Fatal("runner invoked"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil || !strings.Contains(string(data), "No saved findings") {
		t.Fatalf("empty report: %s %v", data, err)
	}
}
