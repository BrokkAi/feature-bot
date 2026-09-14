// Package worker exposes the bot's one-shot operations over a private,
// versioned local HTTP service. The current transport is a Unix-domain socket;
// the request and event schemas are independent of that transport so mutual TLS
// can be added without changing bot semantics.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BrokkAi/acp-go/runner"
)

const (
	ProtocolVersion = 1
	MinimumProtocol = 1
	maxRequest      = 1 << 20
	contentType     = "application/json"
	streamType      = "application/x-ndjson"
)

type Initialize struct {
	Protocol        int      `json:"protocol"`
	MinimumProtocol int      `json:"minimum_protocol"`
	Bot             string   `json:"bot"`
	Version         string   `json:"version"`
	Capabilities    []string `json:"capabilities"`
}

type Request struct {
	Protocol       int                `json:"protocol"`
	Remote         string             `json:"remote"`
	Branch         string             `json:"branch"`
	Directory      string             `json:"directory"`
	StateDirectory string             `json:"state_directory"`
	Repo           string             `json:"repo"`
	Host           string             `json:"host"`
	Agent          runner.AgentConfig `json:"agent"`
	Verify         []string           `json:"verify,omitempty"`
	PR             int                `json:"pr,omitempty"`
	BaseSHA        string             `json:"base_sha,omitempty"`
	HeadSHA        string             `json:"head_sha,omitempty"`
}

type Progress struct {
	Phase string `json:"phase"`
	Task  string `json:"task"`
}

type IssueOwnership struct {
	PR     int    `json:"pr"`
	Branch string `json:"branch"`
	Issue  int    `json:"issue"`
}

type IssueResult struct {
	Owned []IssueOwnership `json:"owned"`
}

type ReviewResult struct {
	Complete  bool              `json:"complete"`
	Findings  map[string]string `json:"findings,omitempty"`
	ExactBase string            `json:"exact_base,omitempty"`
	ExactHead string            `json:"exact_head,omitempty"`
}

type Result struct {
	Issue  *IssueResult  `json:"issue,omitempty"`
	Review *ReviewResult `json:"review,omitempty"`
}

type Event struct {
	Type     string    `json:"type"`
	Seq      uint64    `json:"seq"`
	Progress *Progress `json:"progress,omitempty"`
	Result   *Result   `json:"result,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type RunFunc func(context.Context, Request, func(Progress)) (Result, error)

type server struct {
	info     Initialize
	run      RunFunc
	stop     chan struct{}
	stopOnce sync.Once
}

func Serve(ctx context.Context, socketPath string, info Initialize, run RunFunc, log *slog.Logger) error {
	if strings.TrimSpace(socketPath) == "" {
		return errors.New("worker socket path is required")
	}
	if info.Protocol != ProtocolVersion || info.MinimumProtocol != MinimumProtocol {
		return errors.New("worker protocol constants are inconsistent")
	}
	if run == nil {
		return errors.New("worker run function is required")
	}
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		return err
	}
	if info.Capabilities == nil {
		info.Capabilities = []string{"run", "progress"}
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen on worker socket: %w", err)
	}
	if err = os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("secure worker socket: %w", err)
	}
	defer func() { _ = os.Remove(socketPath) }()
	s := &server{info: info, run: run, stop: make(chan struct{})}
	httpServer := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- httpServer.Serve(listener) }()
	log.Info("Brokk worker listening", "socket", socketPath, "protocol", ProtocolVersion, "bot", info.Bot, "version", info.Version)
	select {
	case err = <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-s.stop:
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
		}
		<-serveDone
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
		}
		<-serveDone
		return ctx.Err()
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/initialize", s.initialize)
	mux.HandleFunc("POST /v1/runs", s.runs)
	mux.HandleFunc("POST /v1/shutdown", s.shutdown)
	return mux
}

func (s *server) initialize(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.info)
}

func (s *server) runs(w http.ResponseWriter, r *http.Request) {
	if value := r.Header.Get("Content-Type"); !strings.HasPrefix(value, contentType) {
		http.Error(w, "Content-Type must be application/json\n", http.StatusUnsupportedMediaType)
		return
	}
	var request Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "Invalid worker run request: "+err.Error()+"\n", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "Expected one JSON run request\n", http.StatusBadRequest)
		return
	}
	if request.Protocol != ProtocolVersion {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": "protocol version not supported", "protocol": ProtocolVersion})
		return
	}
	w.Header().Set("Content-Type", streamType)
	w.Header().Set("X-Brokk-Worker-Protocol", fmt.Sprint(ProtocolVersion))
	w.WriteHeader(http.StatusOK)

	var seq uint64
	var mu sync.Mutex
	write := func(event Event) {
		mu.Lock()
		defer mu.Unlock()
		seq++
		event.Seq = seq
		_ = json.NewEncoder(w).Encode(event)
		if flusher, ok := w.(interface{ Flush() }); ok {
			flusher.Flush()
		}
	}
	runCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	progress := func(p Progress) {
		if runCtx.Err() != nil {
			return
		}
		p.Phase = strings.TrimSpace(p.Phase)
		p.Task = strings.TrimSpace(p.Task)
		if p.Phase == "" {
			p.Phase = "running"
		}
		write(Event{Type: "progress", Progress: &p})
	}
	result, err := s.run(runCtx, request, progress)
	if result.Issue != nil || result.Review != nil {
		write(Event{Type: "result", Result: &result})
	}
	if err != nil {
		if runCtx.Err() != nil {
			write(Event{Type: "canceled", Error: runCtx.Err().Error()})
			return
		}
		write(Event{Type: "error", Error: err.Error()})
		return
	}
	write(Event{Type: "complete"})
}

func (s *server) shutdown(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		http.Error(w, "shutdown accepts an empty body\n", http.StatusBadRequest)
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
	writeJSON(w, http.StatusAccepted, map[string]bool{"stopping": true})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Brokk-Worker-Protocol", fmt.Sprint(ProtocolVersion))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
