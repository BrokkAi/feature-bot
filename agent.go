package featurebot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/clienthost"
	"github.com/BrokkAi/acp-go/runner"
	"github.com/BrokkAi/acp-go/schema"
	"github.com/BrokkAi/feature-bot/internal/osrun"
)

type Agent interface {
	Execute(context.Context, string) (string, error)
}
type agentProcess struct {
	config Config
	log    *slog.Logger
}

// Execute follows acp-go v0.7.0 runner/runner.go (Apache-2.0,
// Copyright 2026 Brokk.ai and contributors; originally part of
// BrokkAi/release-bot). Keep the lifecycle aligned with that runner while
// applying the legacy effort selector fallback after model selection.
func (a agentProcess) Execute(ctx context.Context, prompt string) (result string, runErr error) {
	if a.log == nil {
		a.log = slog.Default()
	}
	if len(a.config.Agent.Command) == 0 || a.config.Agent.Command[0] == "" {
		return "", &runner.SetupError{Err: errors.New("agent command is required")}
	}
	promptStarted := false
	defer func() {
		if runErr != nil && !promptStarted {
			runErr = &runner.SetupError{Err: runErr}
		}
	}()
	dir := filepath.Join(a.config.StateDirectory, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	transcript, err := os.CreateTemp(dir, "session-*.jsonl")
	if err != nil {
		return result, err
	}
	defer transcript.Close()
	host, err := clienthost.Open(ctx, clienthost.Config{Directory: a.config.Directory, Transcript: transcript, Logger: a.log})
	if err != nil {
		return result, err
	}
	defer host.Close()
	host.SetAutoApprove(true)
	a.log.Info("Starting agent", "command", strings.Join(a.config.Agent.Command, " "), "transcript", transcript.Name())
	cmd := osrun.StartCommand(context.Background(), a.config.Directory, a.config.Agent.Command, a.config.Agent.Environment)
	diagnostics := &osrun.Tail{Capacity: 64 << 10}
	cmd.Stderr = io.MultiWriter(diagnostics, host.ProcessWriter("Agent stderr", ""))
	in, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	defer out.Close()
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("launch ACP agent: %w", err)
	}
	defer func() {
		_ = osrun.Kill(cmd)
		_ = cmd.Wait()
		text, _ := diagnostics.Text()
		_ = host.Record(map[string]string{"stderr": text})
		if runErr != nil && text != "" {
			runErr = fmt.Errorf("%w\nAgent diagnostics: %s", runErr, text)
		}
	}()
	connection := acp.Connect(out, in, host.Request, host.Notification)
	phase := "initialize"
	started := time.Now()
	defer func() {
		record := map[string]any{"event": "session_end", "phase": phase, "elapsed": time.Since(started).String()}
		if runErr != nil {
			record["error"] = runErr.Error()
		}
		if cause := context.Cause(ctx); cause != nil {
			record["context_cause"] = cause.Error()
		}
		if err := connection.Err(); err != nil {
			record["transport_error"] = err.Error()
		}
		_ = host.Record(record)
		_ = connection.Close()
	}()
	capabilities := acp.WorkspaceCapabilities(true, true, true)
	capabilities.Session = acp.ConfigOptionsClientCapabilities(true)
	init, err := connection.InitializeWithInfo(ctx, capabilities, acp.ClientInfo{Name: "feature-bot", Version: "0.1.0"})
	if err != nil {
		return result, err
	}
	if a.config.Agent.AuthMethod != "" {
		phase = "authenticate"
		if err := connection.Authenticate(ctx, init, a.config.Agent.AuthMethod); err != nil {
			return result, err
		}
	}
	phase = "session/new"
	session, err := connection.NewSession(ctx, a.config.Directory)
	if err != nil {
		return result, fmt.Errorf("create ACP session (check agent login): %w", err)
	}
	host.SetSession(session.SessionID)
	if a.config.Agent.Mode != "" {
		phase = "select mode"
		if err := connection.SetMode(ctx, &session, a.config.Agent.Mode); err != nil {
			return result, err
		}
	}
	if a.config.Agent.Model != "" {
		phase = "select model"
		if err := connection.SetModel(ctx, &session, a.config.Agent.Model); err != nil {
			return result, err
		}
		a.log.Info("Using model", "model", a.config.Agent.Model)
	}
	if a.config.Agent.Effort != "" {
		phase = "select effort"
		if err := setAgentEffort(ctx, connection, &session, a.config.Agent.Effort); err != nil {
			return result, err
		}
		a.log.Info("Using reasoning effort", "effort", a.config.Agent.Effort)
	}
	a.log.Info("agent session", "id", session.SessionID, "transcript", transcript.Name())
	if err := host.Record(map[string]string{"prompt": prompt}); err != nil {
		return result, err
	}
	phase = "session/prompt"
	promptStarted = true
	reason, err := connection.Prompt(ctx, session, prompt)
	if err != nil {
		return result, err
	}
	if reason != schema.StopReasonEndTurn {
		return result, fmt.Errorf("agent stopped with %s", reason)
	}
	if logErr := host.LogErr(); logErr != nil {
		return result, logErr
	}
	text, truncated := host.Answer()
	if truncated {
		return "", errors.New("agent answer exceeded 2 MiB")
	}
	return text, nil
}

// setAgentEffort restores v0.1.0's category-less thought_level fallback.
// Explicit thought_level categories take priority; otherwise the legacy ID
// precedes reasoning_effort. The upstream selector still validates advertised
// values and requires acknowledgement using the original wire config ID.
func setAgentEffort(ctx context.Context, connection *acp.Connection, session *acp.Session, effort string) error {
	category := schema.SessionConfigOptionCategoryThoughtLevel
	for _, option := range session.ConfigOptions {
		if option.Select != nil && option.Category != nil && *option.Category == category {
			return connection.SetEffort(ctx, session, effort)
		}
	}
	// Normalize a copy so the compatibility hint never changes advertised data.
	compatible := *session
	compatible.ConfigOptions = append([]schema.SessionConfigOption(nil), session.ConfigOptions...)
	for i := range compatible.ConfigOptions {
		option := &compatible.ConfigOptions[i]
		if option.Select != nil && option.Category == nil && option.ID == "thought_level" {
			option.Category = &category
			err := connection.SetEffort(ctx, &compatible, effort)
			if err == nil {
				session.ConfigOptions = compatible.ConfigOptions
			}
			return err
		}
	}
	return connection.SetEffort(ctx, session, effort)
}
