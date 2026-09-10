package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	bot "github.com/BrokkAi/feature-bot"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func dashboardEnabled(plain, json, inputTTY, outputTTY bool, terminal string) bool {
	return !plain && !json && inputTTY && outputTTY && terminal != "dumb"
}

func runDashboard(ctx context.Context, cfg bot.Config, once bool, run runFunc, input, output *os.File) error {
	d := newDashboard(cfg)
	old, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return fmt.Errorf("start dashboard (use --plain for scrolling logs): %w", err)
	}
	// session returns only after the worker exits; no logger can write after cleanup.
	err = func() error {
		defer term.Restore(int(input.Fd()), old)
		defer fmt.Fprint(output, "\x1b[0m\x1b[?25h\x1b[?2004l\x1b[?1049l")
		if _, err := fmt.Fprint(output, "\x1b[?1049h\x1b[?25l\x1b[?2004h"); err != nil {
			return err
		}
		return dashboardSession(ctx, d, once, run, input, output)
	}()
	_, writeErr := fmt.Fprint(output, d.summary(err))
	return errors.Join(err, writeErr)
}

func dashboardSession(ctx context.Context, d *dashboard, once bool, run runFunc, input, output *os.File) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	log := slog.New(&dashboardHandler{d: d})
	result := make(chan error, 1)
	go func() { result <- run(bot.WithProgress(ctx, d.update), d.cfg, log, once) }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var keys keyDecoder
	var last string
	var uiErr error
	color := os.Getenv("NO_COLOR") == ""
	render := func() {
		width, height, err := term.GetSize(int(output.Fd()))
		if err != nil {
			width, height = 80, 24
		}
		frame := d.render(width, height, time.Now(), color)
		if frame == last {
			return
		}
		// Erase each row, rather than clearing the screen between frames.
		last = frame
		frame = strings.ReplaceAll(frame, "\r\n", "\x1b[K\r\n") + "\x1b[K\x1b[J"
		if _, err := fmt.Fprint(output, "\x1b[H"+frame); err != nil {
			uiErr = err
			cancel()
		}
	}
	render()
	for {
		select {
		case err := <-result:
			return errors.Join(err, uiErr)
		case <-ticker.C:
			fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
			if _, err := unix.Poll(fds, 0); err != nil && !errors.Is(err, unix.EINTR) {
				uiErr = err
				cancel()
			}
			if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				cancel()
			}
			if fds[0].Revents&unix.POLLIN != 0 {
				var buf [128]byte
				n, err := unix.Read(int(input.Fd()), buf[:])
				if err != nil && !errors.Is(err, unix.EINTR) {
					uiErr = err
					cancel()
				}
				for _, key := range keys.feed(string(buf[:max(0, n)])) {
					if d.key(key) {
						cancel()
					}
				}
			}
			if fds[0].Revents&unix.POLLIN == 0 && keys.pending == "\x1b" {
				keys.pending = ""
				d.key("esc")
			}
			if ctx.Err() != nil {
				d.mu.Lock()
				d.stopping = true
				d.mu.Unlock()
			}
			render()
		}
	}
}

// Decode split escape sequences and discard bracketed paste, so pasted text
// cannot accidentally act as dashboard shortcuts.
type keyDecoder struct {
	pending string
	paste   bool
}

func (d *keyDecoder) feed(text string) []string {
	d.pending += text
	var keys []string
	for len(d.pending) > 0 {
		if d.pending[0] == '\x1b' {
			if len(d.pending) == 1 {
				break
			}
			if d.pending[1] != '[' && d.pending[1] != 'O' {
				d.pending = d.pending[1:]
				if !d.paste {
					keys = append(keys, "esc")
				}
				continue
			}
			end := 2
			for end < len(d.pending) && (d.pending[end] < 0x40 || d.pending[end] > 0x7e) {
				end++
			}
			if end == len(d.pending) {
				if len(d.pending) > 32 {
					d.pending = ""
				}
				break
			}
			seq := d.pending[:end+1]
			d.pending = d.pending[end+1:]
			if seq == "\x1b[200~" {
				d.paste = true
				continue
			}
			if seq == "\x1b[201~" {
				d.paste = false
				continue
			}
			if d.paste {
				continue
			}
			key := map[string]string{"\x1b[A": "up", "\x1b[B": "down", "\x1bOA": "up", "\x1bOB": "down", "\x1b[5~": "pgup", "\x1b[6~": "pgdown"}[seq]
			if key != "" {
				keys = append(keys, key)
			}
			continue
		}
		key := string(d.pending[0])
		d.pending = d.pending[1:]
		if d.paste {
			continue
		}
		switch key {
		case "\x03":
			key = "ctrl+c"
		case "\r", "\n":
			key = "enter"
		case "\t":
			key = "tab"
		}
		keys = append(keys, key)
	}
	return keys
}
