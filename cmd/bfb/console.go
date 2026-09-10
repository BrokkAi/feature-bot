package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

type console struct {
	writer io.Writer
	mu     *sync.Mutex
	attrs  []slog.Attr
	group  string
	stream *consoleStream
}

type consoleStream struct {
	key  string
	open bool
}

func newConsole(w io.Writer) *console {
	return &console{writer: w, mu: new(sync.Mutex), stream: new(consoleStream)}
}
func (h *console) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelInfo }
func (h *console) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Message == "agent transcript" {
		var source, id, text string
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "source":
				source = a.Value.String()
			case "stream_id":
				id = a.Value.String()
			case "text":
				text = a.Value.String()
			}
			return true
		})
		key := source + "/" + id
		if h.stream.open && h.stream.key != key {
			if _, err := fmt.Fprintln(h.writer); err != nil {
				return err
			}
			h.stream.open = false
		}
		h.stream.key = key
		for text != "" {
			if !h.stream.open {
				if _, err := fmt.Fprintf(h.writer, "%s  %s │ ", r.Time.Format("15:04:05"), source); err != nil {
					return err
				}
				h.stream.open = true
			}
			line, rest, newline := strings.Cut(text, "\n")
			if _, err := fmt.Fprint(h.writer, line); err != nil {
				return err
			}
			if newline {
				if _, err := fmt.Fprintln(h.writer); err != nil {
					return err
				}
				h.stream.open = false
			}
			text = rest
		}
		return nil
	}
	if h.stream.open {
		if _, err := fmt.Fprintln(h.writer); err != nil {
			return err
		}
		h.stream.open = false
	}
	var line strings.Builder
	fmt.Fprintf(&line, "%s  %s", r.Time.Format("15:04:05"), r.Message)
	if r.Level >= slog.LevelWarn {
		fmt.Fprintf(&line, " [%s]", strings.ToLower(r.Level.String()))
	}
	write := func(a slog.Attr) bool {
		fmt.Fprintf(&line, " · %s: %v", strings.ReplaceAll(h.group+a.Key, "_", " "), a.Value.Resolve())
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	_, err := fmt.Fprintln(h.writer, line.String())
	return err
}
func (h *console) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}
func (h *console) WithGroup(name string) slog.Handler {
	next := *h
	if name != "" {
		next.group += name + "."
	}
	return &next
}
