package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/runner"
	bot "github.com/BrokkAi/feature-bot"
	"github.com/BrokkAi/feature-bot/internal/worker"
)

func TestWorkerCapabilities(t *testing.T) {
	info := workerInfo("fixture")
	if info.Protocol != 1 || info.MinimumProtocol != 1 || info.Bot != "feature-bot" || info.Version != "fixture" || !reflect.DeepEqual(info.Capabilities, []string{"run", "progress", "feature-research", "feature-research-controls"}) {
		t.Fatalf("unexpected initialization: %+v", info)
	}
}

func TestWorkerResearchMappingAndIsolation(t *testing.T) {
	dir := t.TempDir()
	request := worker.Request{Remote: "https://github.com/o/r.git", Branch: "main", Directory: filepath.Join(dir, "checkout"), StateDirectory: filepath.Join(dir, "state"), Repo: "o/r", Host: "github.com", Agent: runner.AgentConfig{Command: []string{"simulated"}}, Verify: []string{"fixture-verifier"}}
	var configs []bot.Config
	run := workerRun(func(_ context.Context, cfg bot.Config, _ *slog.Logger, once bool) error {
		if !once {
			t.Fatal("worker must run once")
		}
		configs = append(configs, cfg)
		return nil
	})
	one := 1
	for _, options := range []*worker.FeatureResearch{{Focus: "onboarding", MaxIssues: &one}, nil, {}, {Focus: "reporting"}, {MaxIssues: &one}, nil} {
		request.FeatureResearch = options
		if _, err := run(context.Background(), request, func(worker.Progress) {}); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []struct {
		focus string
		max   int
	}{{"onboarding", 1}, {"", 3}, {"", 3}, {"reporting", 3}, {"", 1}, {"", 3}} {
		cfg := configs[i]
		if cfg.Focus != want.focus || cfg.MaxIssues != want.max || cfg.Remote != request.Remote || cfg.Branch != request.Branch || cfg.Directory != request.Directory || cfg.StateDirectory != request.StateDirectory || cfg.GitHub.Repo != request.Repo || cfg.GitHub.Host != request.Host || !reflect.DeepEqual(cfg.Agent, request.Agent) || !reflect.DeepEqual(cfg.Verify, request.Verify) {
			t.Fatalf("request %d: %+v", i, cfg)
		}
	}
	for _, max := range []int{-1, 0, 21} {
		request.FeatureResearch = &worker.FeatureResearch{MaxIssues: &max}
		if _, err := run(context.Background(), request, func(worker.Progress) {}); err == nil || !strings.Contains(err.Error(), "max_issues must be between 1 and 20") {
			t.Fatalf("maximum %d: %v", max, err)
		}
	}
	if len(configs) != 6 {
		t.Fatal("invalid maximum reached execution")
	}
	for _, max := range []int{1, 20} {
		request.FeatureResearch = &worker.FeatureResearch{MaxIssues: &max}
		if _, err := run(context.Background(), request, func(worker.Progress) {}); err != nil {
			t.Fatal(err)
		}
	}
}
