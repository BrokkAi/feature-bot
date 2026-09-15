package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	bot "github.com/BrokkAi/feature-bot"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"golang.org/x/term"
)

// version is replaced with the release tag when building published binaries.
var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:], nil); err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		os.Exit(1)
	}
}
func execute(ctx context.Context, args []string, log *slog.Logger) error {
	return executeWithRun(ctx, args, log, bot.Run)
}

type runFunc func(context.Context, bot.Config, *slog.Logger, bool) error

func executeWithRun(ctx context.Context, args []string, log *slog.Logger, run runFunc) (result error) {
	ownOutput := log == nil
	if ownOutput {
		log = slog.New(newConsole(os.Stderr))
		defer func() {
			if result != nil && ctx.Err() == nil && !errors.Is(result, context.Canceled) {
				log.Error("Stopped", "error", result)
			}
		}()
	}
	mode := "run"
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			return versionCommand(args[1:], os.Stdout)
		case "worker":
			return workerCommand(ctx, args[1:], buildVersion())
		case "run", "once", "status", "report", "retry":
			mode = args[0]
			args = args[1:]
		}
	}
	fs := flag.NewFlagSet("bfb", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: bfb [run|once|status|report|retry|worker|version] [repository path or URL] [options]\n\nFind valuable new features and file new GitHub issues without duplicates. No config file is required.")
		fs.PrintDefaults()
	}
	file := fs.String("config", "", "optional JSON configuration")
	reportStatus := fs.String("status", "", "report only: filter by saved candidate status (e.g. dry_run)")
	branch := fs.String("branch", "", "base branch (default: repository default)")
	agent := fs.String("agent", "", "ACP executable (default: codex-acp or npx)")
	model := fs.String("model", "", "agent model ID")
	effort := fs.String("effort", "", "reasoning effort")
	maxIssues := fs.Int("max-issues", 3, "maximum new issues per scan (1-20)")
	focus := fs.String("focus", "", "user workflow or feature area to research")
	dryRun := fs.Bool("dry-run", false, "research and review features without creating issues")
	once := fs.Bool("once", mode == "once", "run one scan (or resume a saved scan), then exit")
	jsonOutput := fs.Bool("json", false, "structured logs (disables the dashboard)")
	plain := fs.Bool("plain", false, "scrolling console output (disables the dashboard)")
	defaults := bot.DefaultConfig()
	poll := fs.Duration("poll", 0, "poll interval, e.g. 5m")
	timeout := fs.Duration("timeout", 0, "budget for each attempt, e.g. 2h")
	attempts := fs.Int("attempts", defaults.Attempts, "maximum attempts per scan")
	var labels, agentArgs []string
	fs.Func("label", "label to add to created issues; repeat as needed", func(s string) error { labels = append(labels, s); return nil })
	fs.Func("agent-arg", "argument to agent; repeat as needed", func(s string) error { agentArgs = append(agentArgs, s); return nil })
	if err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if ownOutput && *jsonOutput {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	if *plain && *jsonOutput {
		return errors.New("--plain and --json cannot be used together")
	}
	if fs.NArg() > 1 {
		return errors.New("pass one repository path or URL")
	}
	if mode == "report" {
		if err := bot.ValidateReportStatus(*reportStatus); err != nil {
			return err
		}
	} else {
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "status" {
				result = errors.New("--status is only supported by report")
			}
		})
		if result != nil {
			return result
		}
	}
	var cfg bot.Config
	var err error
	if *file != "" {
		if fs.NArg() > 0 {
			return errors.New("use a repository argument or --config")
		}
		cfg, err = bot.ReadConfig(*file)
	} else {
		cfg, err = bot.Discover(ctx, fs.Arg(0), *branch)
	}
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "branch":
			cfg.Branch = *branch
		case "agent":
			cfg.Agent.Command = []string{*agent}
		case "model":
			cfg.Agent.Model = *model
			if strings.TrimSpace(*model) == "" {
				err = errors.New("model cannot be empty")
			}
		case "effort":
			cfg.Agent.Effort = *effort
			if strings.TrimSpace(*effort) == "" {
				err = errors.New("effort cannot be empty")
			}
		case "max-issues":
			cfg.MaxIssues = *maxIssues
		case "focus":
			cfg.Focus = *focus
		case "dry-run":
			cfg.DryRun = *dryRun
		case "label":
			cfg.Labels = labels
		case "poll":
			cfg.Poll = bot.Duration(*poll)
		case "timeout":
			cfg.Timeout = bot.Duration(*timeout)
		case "attempts":
			cfg.Attempts = *attempts
		}
	})
	if err != nil {
		return err
	}
	cfg.Agent.Command = append(cfg.Agent.Command, agentArgs...)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.GitHubRepo() == "" {
		return errors.New("GitHub remote required; set github.repo for a local mirror")
	}
	if mode == "status" || mode == "report" {
		s, err := bot.ReadState(cfg)
		if err != nil {
			return err
		}
		if mode == "report" {
			return bot.WriteReport(os.Stdout, cfg, s, *reportStatus)
		}
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		return e.Encode(s)
	}
	if err := bot.ResolveAgent(&cfg, *file == "" && *agent == ""); err != nil {
		return err
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("install GitHub CLI and run gh auth login")
	}
	if mode == "retry" {
		if err := bot.Retry(cfg); err != nil {
			return err
		}
	}
	if ownOutput && dashboardEnabled(*plain, *jsonOutput, term.IsTerminal(int(os.Stdin.Fd())), term.IsTerminal(int(os.Stderr.Fd())), os.Getenv("TERM")) {
		return runDashboard(ctx, cfg, *once, run, os.Stdin, os.Stderr)
	}
	log.Info("Researching new features", "repository", cfg.GitHubRepo(), "branch", cfg.Branch, "checkout", cfg.Directory, "state", cfg.StateDirectory)
	return run(ctx, cfg, log, *once)
}

func versionCommand(args []string, output io.Writer) error {
	if len(args) != 0 {
		return errors.New("version does not accept arguments")
	}
	_, err := fmt.Fprintln(output, buildVersion())
	return err
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// Accept the repository before or after flags, as users expect from CLI tools.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		options = append(options, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(append(options, "--"), positional...))
}
