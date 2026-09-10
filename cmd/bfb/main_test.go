package main

import (
	"context"
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
