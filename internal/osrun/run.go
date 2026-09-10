// Package osrun supplies bounded command output for the Unix daemon.
package osrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

type Tail struct {
	sync.Mutex
	Bytes     []byte
	Capacity  int
	Truncated bool
}

func (t *Tail) Write(p []byte) (int, error) {
	t.Lock()
	defer t.Unlock()
	n := len(p)
	if len(p) > t.Capacity {
		p = p[len(p)-t.Capacity:]
		t.Truncated = true
	}
	if excess := len(t.Bytes) + len(p) - t.Capacity; excess > 0 {
		t.Bytes = t.Bytes[excess:]
		t.Truncated = true
	}
	t.Bytes = append(t.Bytes, p...)
	return n, nil
}
func (t *Tail) Text() (string, bool) {
	t.Lock()
	defer t.Unlock()
	b := t.Bytes
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return strings.ToValidUTF8(string(b), "�"), t.Truncated
}
func StartCommand(ctx context.Context, dir string, args []string, env map[string]string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error { return Kill(cmd) }
	return cmd
}
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// Run separates stdout from diagnostic stderr. JSON callers fail if truncated.
func Run(ctx context.Context, dir string, env map[string]string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("empty command")
	}
	cmd := StartCommand(ctx, dir, args, env)
	stdout := &Tail{Capacity: 8 << 20}
	stderr := &Tail{Capacity: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	text, cut := stdout.Text()
	detail, _ := stderr.Text()
	if cut {
		// A tail is not a complete response, even when the process also failed.
		// In particular, callers must not interpret a body fragment as headers.
		return "", errors.Join(fmt.Errorf("%s output exceeded 8 MiB", args[0]), err)
	}
	if err != nil {
		return text, fmt.Errorf("%s: %w\n%s\n%s", args[0], err, detail, text)
	}
	return strings.TrimSpace(text), nil
}
