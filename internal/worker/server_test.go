package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, socket string) *http.Client {
	t.Helper()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

func startWorker(t *testing.T, run RunFunc) (*http.Client, string) {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS == "darwin" && len(filepath.Join(base, "worker.sock")) > 90 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "bw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "worker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, socket, Initialize{Protocol: 1, MinimumProtocol: 1, Bot: "test-bot", Version: "1.2.3", Capabilities: []string{"run", "progress"}}, run, nil)
	}()
	client := testClient(t, socket)
	for range 50 {
		resp, err := client.Get("http://worker/v1/initialize")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Cleanup(func() {
					cancel()
					_ = <-done
				})
				return client, socket
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatalf("worker failed: %v", err)
	}
	t.Fatal("worker did not become ready")
	return nil, ""
}

func TestInitializeAndVersionedRunStream(t *testing.T) {
	client, _ := startWorker(t, func(ctx context.Context, request Request, progress func(Progress)) (Result, error) {
		if request.Protocol != 1 || request.Remote != "https://example.invalid/repo.git" {
			return Result{}, errors.New("invalid request")
		}
		progress(Progress{Phase: "investigating", Task: "fixture"})
		return Result{Issue: &IssueResult{Owned: []IssueOwnership{{PR: 7, Branch: "town/7", Issue: 7}}}}, nil
	})
	body, _ := json.Marshal(Request{Protocol: 1, Remote: "https://example.invalid/repo.git"})
	resp, err := client.Post("http://worker/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" || resp.Header.Get("X-Brokk-Worker-Protocol") != "1" {
		t.Fatalf("wrong response: %#v", resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	var events []Event
	for scanner.Scan() {
		var event Event
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != "progress" || events[1].Type != "result" || events[2].Type != "complete" {
		t.Fatalf("wrong event stream: %#v", events)
	}
	for i, event := range events {
		if event.Seq != uint64(i+1) {
			t.Fatalf("event sequence is not contiguous: %#v", events)
		}
	}
	if events[1].Result.Issue.Owned[0].PR != 7 {
		t.Fatalf("wrong result: %#v", events[1].Result)
	}
}

func TestRunRequestRejectsUnknownFieldsAndTrailingData(t *testing.T) {
	client, _ := startWorker(t, func(context.Context, Request, func(Progress)) (Result, error) {
		return Result{}, nil
	})
	for _, body := range []string{`{"protocol":1,"unknown":true}`, `{"protocol":1} {}`} {
		resp, err := client.Post("http://worker/v1/runs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("accepted invalid body %q: %d %s", body, resp.StatusCode, data)
		}
	}
}

func TestSocketIsPrivate(t *testing.T) {
	_, socket := startWorker(t, func(context.Context, Request, func(Progress)) (Result, error) {
		return Result{}, nil
	})
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions are %#o", info.Mode().Perm())
	}
}

func TestShutdownEndpointStopsServe(t *testing.T) {
	client, _ := startWorker(t, func(context.Context, Request, func(Progress)) (Result, error) {
		return Result{}, nil
	})
	response, err := client.Post("http://worker/v1/shutdown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown returned HTTP %d", response.StatusCode)
	}
}

func TestResearchRequestStrictDecoding(t *testing.T) {
	for _, tc := range []struct {
		body, wantError string
		focus           string
		max             *int
	}{
		{body: `{"protocol":1}`},
		{body: `{"protocol":1,"feature_research":{}}`},
		{body: `{"protocol":1,"feature_research":{"focus":"onboarding","max_issues":1}}`, focus: "onboarding", max: intPointer(1)},
		{body: `{"protocol":1,"feature_research":{"max_issues":0}}`, max: intPointer(0)},
		{body: `{"protocol":1,"feature_research":{"unknown":1}}`, wantError: `unknown field "unknown"`},
		{body: `{"protocol":1,"feature_research":{"focus":1}}`, wantError: "focus"},
		{body: `{"protocol":1,"feature_research":{"max_issues":"1"}}`, wantError: "max_issues"},
		{body: `{"protocol":1,"feature_research":{"max_issues":1.5}}`, wantError: "max_issues"},
		{body: `{"protocol":1,"feature_research":{"max_issues":true}}`, wantError: "max_issues"},
		{body: `{"protocol":1,"feature_research":[]}`, wantError: "feature_research"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			called := false
			s := &server{run: func(_ context.Context, r Request, _ func(Progress)) (Result, error) {
				called = true
				options := r.FeatureResearch
				if options == nil {
					options = &FeatureResearch{}
				}
				if options.Focus != tc.focus || (options.MaxIssues == nil) != (tc.max == nil) || (tc.max != nil && *options.MaxIssues != *tc.max) {
					t.Fatalf("decoded options: %+v", options)
				}
				return Result{}, nil
			}}
			req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			s.handler().ServeHTTP(response, req)
			if tc.wantError != "" {
				if called || response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.wantError) {
					t.Fatalf("called=%v response=%d %s", called, response.Code, response.Body)
				}
			} else if !called || response.Code != http.StatusOK {
				t.Fatalf("response=%d %s", response.Code, response.Body)
			}
		})
	}
}

func intPointer(n int) *int { return &n }
