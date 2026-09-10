//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bot "github.com/BrokkAi/feature-bot"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// This subprocess exercises the real CLI's output selection and terminal lifecycle
// with a simulated worker: no network, agent, or GitHub writes.
func TestTerminalProcess(t *testing.T) {
	mode := os.Getenv("BFB_TERMINAL_TEST")
	if mode == "" {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	args := []string{"once", "--config", os.Getenv("BFB_TERMINAL_CONFIG")}
	if mode == "plain" {
		args = append(args, "--plain")
	}
	if mode == "json" {
		args = append(args, "--json")
	}
	err := executeWithRun(ctx, args, nil, func(ctx context.Context, _ bot.Config, log *slog.Logger, _ bool) error {
		log.Info("Tool", "title", "fixture investigation")
		if mode == "wait" {
			<-ctx.Done()
			return ctx.Err()
		}
		time.Sleep(150 * time.Millisecond)
		if mode == "error" {
			return errors.New("fixture scan failed")
		}
		return nil
	})
	if mode == "error" {
		if err == nil || !strings.Contains(err.Error(), "fixture scan failed") {
			t.Fatal(err)
		}
	} else if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type lockedBuffer struct {
	sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.buffer.Write(p)
}
func (b *lockedBuffer) text() string { b.Lock(); defer b.Unlock(); return b.buffer.String() }

func TestTerminalLifecycle(t *testing.T) {
	for _, mode := range []string{"q", "ctrl-c", "sigterm", "success", "error", "plain", "json"} {
		t.Run(mode, func(t *testing.T) {
			master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Skipf("PTY unavailable: %v", err)
			}
			defer master.Close()
			if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
				t.Fatal(err)
			}
			n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
			if err != nil {
				t.Fatal(err)
			}
			slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer slave.Close()
			if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
				t.Fatal(err)
			}
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			config := filepath.Join(dir, "config.json")
			if err := os.WriteFile(config, []byte(`{"remote":"https://github.com/o/r.git","agent":{"command":["sh"]}}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			workerMode := mode
			if mode == "q" || mode == "ctrl-c" || mode == "sigterm" {
				workerMode = "wait"
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestTerminalProcess$")
			cmd.Env = append(os.Environ(), "BFB_TERMINAL_TEST="+workerMode, "BFB_TERMINAL_CONFIG="+config, "TERM=tmux-256color", "PATH="+dir+":"+os.Getenv("PATH"))
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			var output lockedBuffer
			readDone := make(chan struct{})
			go func() { _, _ = io.Copy(&output, master); close(readDone) }()
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			if workerMode == "wait" {
				deadline := time.Now().Add(5 * time.Second)
				for !strings.Contains(output.text(), "fixture investigation") && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if !strings.Contains(output.text(), "fixture investigation") {
					t.Fatalf("dashboard did not start: %s", output.text())
				}
				if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 12, Col: 40}); err != nil {
					t.Fatal(err)
				}
				// A short delay allows the resize frame and the standalone Escape timeout.
				_, _ = master.Write([]byte("3\x1b"))
				time.Sleep(250 * time.Millisecond)
				switch mode {
				case "q":
					_, _ = master.Write([]byte("q"))
				case "ctrl-c":
					_, _ = master.Write([]byte{3})
				case "sigterm":
					_ = cmd.Process.Signal(syscall.SIGTERM)
				}
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("worker: %v\n%s", err, output.text())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("dashboard failed to exit")
			}
			after, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("terminal mode was not restored")
			}
			slave.Close()
			select {
			case <-readDone:
			case <-time.After(time.Second):
				master.Close()
				<-readDone
			}
			text := output.text()
			if mode == "plain" || mode == "json" {
				if strings.Contains(text, "\x1b") || !strings.Contains(text, "fixture investigation") {
					t.Fatalf("invalid non-TUI output: %q", text)
				}
				if mode == "json" && !strings.Contains(text, `"msg":"Tool"`) {
					t.Fatalf("JSON output was not selected: %s", text)
				}
			} else {
				if !strings.Contains(text, "\x1b[?1049h") || !strings.Contains(text, "\x1b[?25h\x1b[?2004l\x1b[?1049l") || !strings.Contains(text, "bfb o/r") {
					t.Fatalf("missing terminal setup, cleanup, or summary: %q", text)
				}
			}
		})
	}
}
