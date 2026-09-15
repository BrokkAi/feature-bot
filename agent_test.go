package featurebot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/runner"
)

// Exercise the bot's real subprocess adapter against independent JSON wire
// fixtures, so schema or client-host migrations cannot be hidden by a mock Agent.
func TestACPAgentProcess(t *testing.T) {
	for _, scenario := range []string{"success", "thought-level-id", "reasoning-effort-id", "category-priority", "legacy-priority", "effort-unavailable", "effort-unconfirmed", "effort-wrong-category", "setup-error", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, state := t.TempDir(), t.TempDir()
			a := agentProcess{config: Config{Directory: workspace, StateDirectory: state, Agent: AgentConfig{
				Command:     []string{os.Args[0], "-test.run=^TestACPWireHelper$"},
				Environment: map[string]string{"FEATURE_BOT_ACP_FIXTURE": scenario},
				Mode:        "research", Model: "test-model", Effort: "high",
			}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if scenario == "cancel" {
				done := make(chan struct{})
				defer func() { cancel(); <-done }()
				go func() {
					defer close(done)
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							if _, err := os.Stat(filepath.Join(workspace, "prompt-started")); err == nil {
								cancel()
								return
							}
						}
					}
				}()
			}
			answer, err := a.Execute(ctx, "research this repository")
			var setup *runner.SetupError
			switch scenario {
			case "success", "thought-level-id", "reasoning-effort-id", "category-priority", "legacy-priority":
				if err != nil || answer != "first second" {
					t.Fatalf("answer=%q, err=%v", answer, err)
				}
			case "effort-unavailable", "effort-unconfirmed", "effort-wrong-category":
				want := map[string]string{
					"effort-unavailable":    "unknown thought_level",
					"effort-unconfirmed":    "agent did not confirm thought_level",
					"effort-wrong-category": "agent does not advertise ACP reasoning effort selection",
				}[scenario]
				if answer != "" || !errors.As(err, &setup) || !strings.Contains(err.Error(), want) {
					t.Fatalf("expected setup error containing %q, got answer=%q, err=%v", want, answer, err)
				}
			case "setup-error":
				if !errors.As(err, &setup) || !strings.Contains(err.Error(), "fixture setup failure") {
					t.Fatalf("expected setup error, got %v", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || errors.As(err, &setup) {
					t.Fatalf("expected prompt cancellation, got %v", err)
				}
			}
			files, err := filepath.Glob(filepath.Join(state, "sessions", "session-*.jsonl"))
			if err != nil || len(files) != 1 {
				t.Fatalf("transcripts=%v, err=%v", files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid transcript record: %s", line)
				}
			}
			if !strings.Contains(string(data), `"session_end"`) {
				t.Fatalf("missing session end: %s", data)
			}
			if strings.HasPrefix(scenario, "effort-") && strings.Contains(string(data), "research this repository") {
				t.Fatalf("prompt sent after effort setup failure: %s", data)
			}
			if answer == "first second" {
				for _, want := range []string{"research this repository", "first ", "second", "permission_request"} {
					if !strings.Contains(string(data), want) {
						t.Errorf("transcript missing %q", want)
					}
				}
				info, err := os.Stat(files[0])
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Errorf("transcript permissions=%v", info.Mode())
				}
			}
		})
	}
}

func TestACPAgentStartupFailure(t *testing.T) {
	a := agentProcess{config: Config{Directory: t.TempDir(), StateDirectory: t.TempDir(), Agent: AgentConfig{Command: []string{filepath.Join(t.TempDir(), "missing-agent")}}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := a.Execute(context.Background(), "unused")
	var setup *runner.SetupError
	if !errors.As(err, &setup) {
		t.Fatalf("expected setup error, got %v", err)
	}
}

func TestACPWireHelper(t *testing.T) {
	scenario := os.Getenv("FEATURE_BOT_ACP_FIXTURE")
	if scenario == "" {
		return
	}
	// Exit directly to keep the Go test harness from writing to the ACP stream.
	runACPWireFixture(t, scenario)
	os.Exit(0)
}

func runACPWireFixture(t *testing.T, scenario string) {
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	type message struct {
		ID     json.RawMessage            `json:"id"`
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
		Result json.RawMessage            `json:"result"`
	}
	receive := func(method string) message {
		var m message
		if err := decoder.Decode(&m); err != nil {
			t.Fatal(err)
		}
		if m.Method != method {
			t.Fatalf("method=%q, want %q", m.Method, method)
		}
		return m
	}
	send := func(v any) {
		if err := encoder.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	reply := func(m message, result any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) }
	require := func(raw json.RawMessage, want string) {
		var got string
		if err := json.Unmarshal(raw, &got); err != nil || got != want {
			t.Fatalf("value=%s, want %q", raw, want)
		}
	}
	init := receive("initialize")
	var info struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(init.Params["clientInfo"], &info); err != nil || info.Name != "feature-bot" {
		t.Fatalf("clientInfo=%s", init.Params["clientInfo"])
	}
	reply(init, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{}})
	session := receive("session/new")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var requestedCWD string
	if err := json.Unmarshal(session.Params["cwd"], &requestedCWD); err != nil {
		t.Fatal(err)
	}
	resolvedCWD, err := filepath.EvalSymlinks(requestedCWD)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedCWD != cwd {
		t.Fatalf("cwd=%q, want %q", resolvedCWD, cwd)
	}
	if scenario == "setup-error" {
		send(map[string]any{"jsonrpc": "2.0", "id": session.ID, "error": map[string]any{"code": -32603, "message": "fixture setup failure"}})
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	option := func(id, category, value string) map[string]any {
		return map[string]any{"id": id, "name": id, "type": "select", "category": category, "currentValue": value, "options": []any{map[string]any{"value": value, "name": value}}}
	}
	model := option("model", "model", "test-model")
	effortID := "effort"
	if scenario == "thought-level-id" || scenario == "legacy-priority" || strings.HasPrefix(scenario, "effort-") {
		effortID = "thought_level"
	} else if scenario == "reasoning-effort-id" {
		effortID = "reasoning_effort"
	}
	effort := option(effortID, "thought_level", "high")
	effort["currentValue"] = "low"
	if effortID != "effort" {
		delete(effort, "category")
	}
	reply(session, map[string]any{"sessionId": "fixture-session", "modes": map[string]any{"currentModeId": "research", "availableModes": []any{map[string]any{"id": "research", "name": "Research"}}}, "configOptions": []any{model}})
	mode := receive("session/set_mode")
	require(mode.Params["modeId"], "research")
	reply(mode, map[string]any{})
	selection := receive("session/set_config_option")
	require(selection.Params["configId"], "model")
	require(selection.Params["value"], "test-model")
	// Effort becomes available only after the model selection is acknowledged.
	options := []any{model, effort}
	if scenario == "category-priority" || scenario == "legacy-priority" {
		legacy := option("thought_level", "", "high")
		delete(legacy, "category")
		reasoning := option("reasoning_effort", "", "high")
		delete(reasoning, "category")
		if scenario == "category-priority" {
			options = []any{reasoning, legacy, model, effort}
		} else {
			options = []any{reasoning, model, effort}
		}
	}
	if scenario == "effort-unavailable" {
		effort["options"] = []any{map[string]any{"value": "low", "name": "low"}}
	} else if scenario == "effort-wrong-category" {
		effort["category"] = "unrelated"
	}
	reply(selection, map[string]any{"configOptions": options})
	if scenario == "effort-unavailable" || scenario == "effort-wrong-category" {
		// No selection or prompt may be sent after local validation fails.
		var unexpected message
		if err := decoder.Decode(&unexpected); err != io.EOF {
			t.Fatalf("unexpected request after invalid effort: %+v, err=%v", unexpected, err)
		}
		return
	}
	selection = receive("session/set_config_option")
	require(selection.Params["configId"], effortID)
	require(selection.Params["value"], "high")
	if scenario != "effort-unconfirmed" {
		effort["currentValue"] = "high"
	}
	reply(selection, map[string]any{"configOptions": options})
	if scenario == "effort-unconfirmed" {
		var unexpected message
		if err := decoder.Decode(&unexpected); err != io.EOF {
			t.Fatalf("unexpected request after unconfirmed effort: %+v, err=%v", unexpected, err)
		}
		return
	}
	prompt := receive("session/prompt")
	require(prompt.Params["sessionId"], "fixture-session")
	if !strings.Contains(string(prompt.Params["prompt"]), "research this repository") {
		t.Fatalf("prompt=%s", prompt.Params["prompt"])
	}
	if scenario == "cancel" {
		if err := os.WriteFile("prompt-started", nil, 0600); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	send(map[string]any{"jsonrpc": "2.0", "id": "permission", "method": "session/request_permission", "params": map[string]any{"sessionId": "fixture-session", "toolCall": map[string]any{"toolCallId": "read", "title": "Read source"}, "options": []any{map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"}, map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
	permission := receive("")
	var result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(permission.Result, &result); err != nil || result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "allow" {
		t.Fatalf("permission result=%s", permission.Result)
	}
	for _, chunk := range []string{"first ", "second"} {
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "fixture-session", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": chunk}}}})
	}
	reply(prompt, map[string]any{"stopReason": "end_turn"})
	_, _ = io.Copy(io.Discard, os.Stdin)
}
