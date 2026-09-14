package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	bot "github.com/BrokkAi/feature-bot"
	"github.com/BrokkAi/feature-bot/internal/worker"
)

func workerCommand(ctx context.Context, args []string, version string) error {
	fs := flag.NewFlagSet("bfb worker", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: bfb worker --socket PATH\n\nServe versioned one-shot feature research to Brokk Town over a private Unix socket.")
		fs.PrintDefaults()
	}
	socket := fs.String("socket", "", "private Unix-domain socket path (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *socket == "" {
		return fmt.Errorf("worker requires exactly one --socket PATH")
	}
	return worker.Serve(ctx, *socket, worker.Initialize{
		Protocol: worker.ProtocolVersion, MinimumProtocol: worker.MinimumProtocol,
		Bot: "feature-bot", Version: version, Capabilities: []string{"run", "progress", "feature-research"},
	}, func(ctx context.Context, request worker.Request, progress func(worker.Progress)) (worker.Result, error) {
		cfg := bot.DefaultConfig()
		cfg.Remote = request.Remote
		cfg.Branch = request.Branch
		cfg.Directory = request.Directory
		cfg.StateDirectory = request.StateDirectory
		cfg.Agent = request.Agent
		cfg.GitHub.Repo = request.Repo
		cfg.GitHub.Host = request.Host
		cfg.Verify = request.Verify
		ctx = bot.WithProgress(ctx, func(p bot.Progress) {
			progress(worker.Progress{Phase: p.Phase, Task: p.Task})
		})
		return worker.Result{}, bot.Run(ctx, cfg, slog.Default(), true)
	}, slog.Default())
}
