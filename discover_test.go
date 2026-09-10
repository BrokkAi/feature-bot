package featurebot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func discoveryRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := canonicalTestDir(t)
	remote := filepath.Join(dir, "published.git")
	source := filepath.Join(dir, "source")
	localGit(t, dir, "init", "--bare", remote)
	localGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	localGit(t, dir, "init", "-b", "main", source)
	writeTestFile(t, filepath.Join(source, "README.md"), "# Task coordinator\nCoordinate tasks and prepare team reports.\nComplete command reference: task list [--status STATUS] displays matching tasks as text, ordered by creation time. No other commands are supported.\n")
	localGit(t, source, "add", ".")
	localGit(t, source, "commit", "-m", "initial")
	localGit(t, source, "remote", "add", "origin", remote)
	localGit(t, source, "push", "origin", "main")
	localGit(t, source, "switch", "-c", "work-in-progress")
	return source, remote
}
func TestDiscoveryDefaultsAndPersistentWorkspace(t *testing.T) {
	source, remote := discoveryRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	nested := filepath.Join(source, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "unfinished.txt"), "user edits")
	cfg, err := Discover(context.Background(), nested, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote != remote || cfg.Branch != "main" {
		t.Fatalf("should watch remote main, not current feature branch: %+v", cfg)
	}
	if !strings.Contains(cfg.StateDirectory, string(os.PathSeparator)+"feature-bot"+string(os.PathSeparator)) || strings.Contains(cfg.StateDirectory, "bug-bot") {
		t.Fatalf("feature discovery did not isolate its state: %s", cfg.StateDirectory)
	}
	if strings.HasPrefix(cfg.Directory, source+string(os.PathSeparator)) {
		t.Fatal("managed checkout is inside user repository")
	}
	if _, err := os.Stat(cfg.Directory); !os.IsNotExist(err) {
		t.Fatal("discovery should not clone or modify repositories")
	}
	again, err := Discover(context.Background(), source, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Directory != again.Directory || cfg.StateDirectory != again.StateDirectory {
		t.Fatal("restart would lose state")
	}
	urlConfig, err := Discover(context.Background(), "file://"+remote, "")
	if err != nil || urlConfig.Branch != "main" {
		t.Fatalf("URL discovery: %+v %v", urlConfig, err)
	}
	other, err := Discover(context.Background(), source, "release/1.x")
	if err != nil {
		t.Fatal(err)
	}
	if other.Branch != "release/1.x" || other.StateDirectory == cfg.StateDirectory {
		t.Fatal("branch override did not get isolated state")
	}
	data, _ := os.ReadFile(filepath.Join(source, "unfinished.txt"))
	if string(data) != "user edits" {
		t.Fatal("discovery changed source checkout")
	}
}
func TestDiscoveryFromCwdAndMissingRemote(t *testing.T) {
	source, _ := discoveryRepo(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(source)
	if _, err := Discover(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	localGit(t, source, "remote", "remove", "origin")
	if _, err := Discover(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "no remote") {
		t.Fatalf("missing remote error: %v", err)
	}
}
func TestAgentFallbackHonorsExplicitCommands(t *testing.T) {
	dir := canonicalTestDir(t)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "npx"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	if err := ResolveAgent(&cfg, true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Agent.Command, " ") != "npx --yes @agentclientprotocol/codex-acp" {
		t.Fatalf("unexpected fallback %v", cfg.Agent.Command)
	}
	cfg = DefaultConfig()
	if ResolveAgent(&cfg, false) == nil {
		t.Fatal("explicit missing executable was replaced")
	}
	if err := os.WriteFile(filepath.Join(dir, "codex-acp"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg = DefaultConfig()
	if err := ResolveAgent(&cfg, true); err != nil || cfg.Agent.Command[0] != "codex-acp" {
		t.Fatalf("installed adapter not preferred: %v", err)
	}
}
