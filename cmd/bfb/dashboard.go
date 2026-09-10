package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	bot "github.com/BrokkAi/feature-bot"
	"github.com/rivo/uniseg"
)

const activityLimit = 200

type dashboard struct {
	mu                                         sync.Mutex
	cfg                                        bot.Config
	progress                                   bot.Progress
	started, phaseStarted, lastActivity        time.Time
	tool                                       string
	activity                                   []string
	streamKey                                  string
	streamOpen                                 bool
	scans, attempts, sessions, tools, failures int
	view, selected, scroll                     int
	detail                                     bool
	stopping                                   bool
	initial                                    map[string]string
}

func newDashboard(cfg bot.Config) *dashboard {
	now := time.Now()
	return &dashboard{cfg: cfg, started: now, phaseStarted: now, lastActivity: now,
		progress: bot.Progress{Phase: "starting", Task: "Loading saved scan"}}
}

func (d *dashboard) update(p bot.Progress) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.initial == nil {
		d.initial = make(map[string]string)
		for _, f := range p.Findings {
			d.initial[f.ID] = f.Status
		}
	}
	if p.Phase == "" {
		p.Phase, p.Task = d.progress.Phase, d.progress.Task
	} else {
		if p.Phase != d.progress.Phase || p.Task != d.progress.Task {
			d.phaseStarted = time.Now()
			d.tool = ""
		}
		switch p.Phase {
		case "complete":
			d.scans++
		case "attempt":
			d.attempts++
		}
	}
	if p.Commit == "" {
		p.Commit = d.progress.Commit
	}
	// Keep a finding open while new results arrive; the overview follows new
	// findings only when the user has not browsed away from the newest one.
	if (d.detail || d.selected > 0) && len(d.progress.Findings) > d.selected {
		id := d.progress.Findings[len(d.progress.Findings)-1-d.selected].ID
		for i, f := range p.Findings {
			if f.ID == id {
				d.selected = len(p.Findings) - 1 - i
				break
			}
		}
	}
	d.progress = p
	d.selected = max(0, min(d.selected, len(p.Findings)-1))
}

type dashboardHandler struct {
	d     *dashboard
	attrs []slog.Attr
	group string
}

func (h *dashboardHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}
func (h *dashboardHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}
func (h *dashboardHandler) WithGroup(name string) slog.Handler {
	next := *h
	if name != "" {
		next.group += name + "."
	}
	return &next
}
func (h *dashboardHandler) Handle(_ context.Context, r slog.Record) error {
	d := h.d
	d.mu.Lock()
	defer d.mu.Unlock()
	attrs := make(map[string]string)
	var extra []string
	add := func(a slog.Attr) bool {
		a.Value = a.Value.Resolve()
		attrs[h.group+a.Key] = a.Value.String()
		if a.Key != "body" && a.Key != "text" {
			extra = append(extra, a.Key+": "+fmt.Sprint(a.Value))
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	d.lastActivity = r.Time
	if r.Message == "agent transcript" {
		key := attrs["source"] + "/" + attrs["stream_id"]
		for text := attrs["text"]; text != ""; {
			line, rest, newline := strings.Cut(text, "\n")
			line = cleanText(line)
			if d.streamOpen && d.streamKey == key && len(d.activity) > 0 {
				i := len(d.activity) - 1
				d.activity[i] = clipText(d.activity[i]+line, 2000)
			} else {
				d.addActivity(r.Time.Format("15:04:05") + " " + attrs["source"] + " › " + line)
			}
			d.streamKey, d.streamOpen = key, !newline
			text = rest
		}
		return nil
	}
	d.streamOpen = false
	switch r.Message {
	case "Tool":
		d.tools++
		d.tool = attrs["title"]
	case "Tool completed", "Tool failed":
		if d.tool == attrs["title"] {
			d.tool = ""
		}
	case "Starting agent":
		d.sessions++
	case "Using model":
		d.cfg.Agent.Model = attrs["model"]
	case "Using reasoning effort":
		d.cfg.Agent.Effort = attrs["effort"]
	}
	if r.Level >= slog.LevelError {
		d.failures++
	}
	line := r.Time.Format("15:04:05") + " " + r.Message
	if len(extra) > 0 {
		line += " · " + strings.Join(extra, " · ")
	}
	d.addActivity(line)
	return nil
}

func (d *dashboard) addActivity(line string) {
	d.activity = append(d.activity, clipText(cleanText(line), 2000))
	if len(d.activity) > activityLimit {
		copy(d.activity, d.activity[len(d.activity)-activityLimit:])
		d.activity = d.activity[:activityLimit]
	}
	if d.view == 2 && d.scroll > 0 {
		d.scroll++
	}
}

func (d *dashboard) key(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if key == "ctrl+c" || key == "q" {
		d.stopping = true
		return true
	}
	if key == "esc" || key == "enter" && d.detail {
		d.detail, d.scroll = false, 0
		return false
	}
	if d.detail {
		switch key {
		case "j", "down":
			d.scroll++
		case "k", "up":
			d.scroll = max(0, d.scroll-1)
		case "pgdown":
			d.scroll += 10
		case "pgup":
			d.scroll = max(0, d.scroll-10)
		case "g":
			d.scroll = 0
		case "G":
			d.scroll = 1 << 30
		}
		return false
	}
	switch key {
	case "1", "2", "3":
		d.view, d.scroll = int(key[0]-'1'), 0
	case "tab":
		d.view, d.scroll = (d.view+1)%3, 0
	case "enter":
		if len(d.progress.Findings) > 0 && d.view != 2 {
			d.detail, d.scroll = true, 0
		}
	case "j", "down", "pgdown":
		if d.view == 2 {
			d.scroll = max(0, d.scroll-1)
		} else {
			d.selected = max(0, min(len(d.progress.Findings)-1, d.selected+1))
		}
	case "k", "up", "pgup":
		if d.view == 2 {
			d.scroll++
		} else {
			d.selected = max(0, d.selected-1)
		}
	case "g":
		if d.view == 2 {
			d.scroll = 1 << 30
		} else {
			d.selected = 0
		}
	case "G":
		if d.view == 2 {
			d.scroll = 0
		} else {
			d.selected = max(0, len(d.progress.Findings)-1)
		}
	}
	return false
}

// render always fits inside the pane, reserving the last column to avoid autowrap.
func (d *dashboard) render(width, height int, now time.Time, color bool) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	width, height = max(1, width-1), max(1, height)
	var rows []string
	add := func(text, style string) {
		text = clipText(cleanText(text), width)
		if color && style != "" {
			text = "\x1b[" + style + "m" + text + "\x1b[0m"
		}
		rows = append(rows, text)
	}
	p := d.progress
	phase := strings.ToUpper(p.Phase)
	if d.stopping {
		phase = "STOPPING"
	}
	style := "35;1"
	if p.Phase == "paused" || p.Phase == "blocked" {
		style = "33;1"
	}
	add("(o-o) FEATURE BOT  "+d.cfg.GitHubRepo(), "1;35")
	if d.detail && len(p.Findings) > 0 {
		add(fmt.Sprintf("FEATURE PROPOSAL · %d/%d", d.selected+1, len(p.Findings)), "2")
		f := p.Findings[len(p.Findings)-1-d.selected]
		body := wrapText(statusLabel(f.Status)+" · "+f.Title, width)
		if f.URL != "" {
			body = append(body, wrapText(f.URL, width)...)
		}
		body = append(body, "")
		for _, line := range strings.Split(f.Body, "\n") {
			body = append(body, wrapText(line, width)...)
		}
		remaining := max(0, height-len(rows)-1)
		d.scroll = min(d.scroll, max(0, len(body)-remaining))
		for _, line := range body[d.scroll:min(len(body), d.scroll+remaining)] {
			add(line, "")
		}
		for len(rows) < height-1 {
			add("", "")
		}
		if len(rows) >= height {
			rows = rows[:max(0, height-1)]
		}
		add("↑↓ scroll · esc back · q stop", "2")
		return strings.Join(rows, "\r\n")
	}
	branch := d.cfg.Branch
	if p.Commit != "" {
		branch += " · " + p.Commit[:min(8, len(p.Commit))]
	}
	if d.cfg.DryRun {
		branch += " · DRY RUN"
	}
	add(branch+" · up "+elapsed(now.Sub(d.started)), "2")
	if height >= 18 {
		model := d.cfg.Agent.Model
		if model == "" {
			model = "agent default"
		}
		if d.cfg.Agent.Effort != "" {
			model += " / " + d.cfg.Agent.Effort
		}
		if d.cfg.Focus != "" {
			model += " · focus: " + d.cfg.Focus
		}
		add("[__|__] "+model, "2")
		add(strings.Repeat("─", width), "2")
	}
	phase += " · " + elapsed(now.Sub(d.phaseStarted))
	if p.Attempt > 0 {
		phase += fmt.Sprintf(" · attempt %d/%d", p.Attempt, p.MaxAttempts)
	}
	add(phase, style)
	task := p.Task
	if p.Phase == "waiting" || p.Phase == "paused" {
		if !p.WakeAt.IsZero() {
			task = "Next check in " + elapsed(max(time.Duration(0), p.WakeAt.Sub(now))) + " · " + p.WakeAt.Local().Format("15:04:05")
		}
	}
	if d.stopping {
		task = "Stopping agent and saving progress…"
	}
	add(task, "")
	if height >= 12 {
		activity := d.tool
		if activity != "" {
			activity = "Tool: " + activity
		} else {
			activity = "Last activity " + elapsed(now.Sub(d.lastActivity)) + " ago"
		}
		if p.Phase == "paused" || p.Phase == "blocked" {
			activity = p.Task
		}
		add(activity, "2")
	}
	c := p.Counts
	add(fmt.Sprintf("Saved: %d found · %d filed · %d dup", c.Found, c.Filed, c.Duplicates), "1")
	if height >= 18 {
		add(fmt.Sprintf("%d pending · %d dry run · %d skipped", c.Pending, c.DryRun, c.Skipped), "2")
	}
	run := fmt.Sprintf("Run: %d scans · %d agents · %d tools", d.scans, d.sessions, d.tools)
	if width >= 65 {
		run = fmt.Sprintf("Run: %d scans · %d attempts · %d agents · %d tools · %d errors", d.scans, d.attempts, d.sessions, d.tools, d.failures)
	}
	add(run, "2")
	if height >= 18 {
		add("", "")
	}
	tabs := []string{"1 Overview", "2 Proposals", "3 Activity"}
	tabs[d.view] = "[" + tabs[d.view] + "]"
	add(strings.Join(tabs, "  "), "1")
	remaining := max(0, height-len(rows)-1)
	var body []string
	if d.view == 2 {
		if len(d.activity) == 0 {
			body = []string{"Waiting for agent activity…"}
		} else {
			var lines []string
			for _, line := range d.activity {
				lines = append(lines, wrapText(line, width)...)
			}
			d.scroll = min(d.scroll, max(0, len(lines)-remaining))
			end := max(0, len(lines)-d.scroll)
			body = lines[max(0, end-remaining):end]
		}
	} else {
		limit := remaining
		if d.view == 0 && remaining >= 5 {
			limit = max(2, remaining/2)
		}
		body = d.findingRows(limit)
		if d.view == 0 && remaining-len(body) >= 3 {
			body = append(body, "", "RECENT ACTIVITY")
			space := remaining - len(body)
			body = append(body, d.activity[max(0, len(d.activity)-space):]...)
		}
	}
	for _, line := range body {
		style := ""
		if d.view != 2 {
			if strings.HasPrefix(line, "› ") {
				style = "1;35"
			} else if strings.HasPrefix(line, "  FILED ") {
				style = "32"
			}
		}
		add(line, style)
	}
	for len(rows) < height-1 {
		add("", "")
	}
	footer := "1–3 view · ↑↓ browse · enter detail · q stop"
	if d.detail {
		footer = "↑↓ scroll · esc back · q stop"
	} else if d.view == 2 {
		footer = "↑↓ scroll · G follow · 1 overview · q stop"
	}
	if width < 42 {
		footer = "1–3 view · ↑↓ · enter · q stop"
	}
	if height <= len(rows) {
		rows = rows[:max(0, height-1)]
	}
	add(footer, "2")
	return strings.Join(rows, "\r\n")
}

func (d *dashboard) findingRows(limit int) []string {
	if limit <= 0 {
		return nil
	}
	findings := d.progress.Findings
	if len(findings) == 0 {
		if d.progress.Phase == "waiting" || d.progress.Phase == "complete" {
			return []string{"No feature proposals in saved history."}
		}
		return []string{"Researching feature opportunities…"}
	}
	d.selected = max(0, min(d.selected, len(findings)-1))
	start := max(0, d.selected-limit+1)
	var rows []string
	for i := start; i < min(len(findings), start+limit); i++ {
		f := findings[len(findings)-1-i]
		prefix := "  "
		if i == d.selected {
			prefix = "› "
		}
		label := statusLabel(f.Status)
		if f.URL != "" {
			label += " #" + f.URL[strings.LastIndex(f.URL, "/")+1:]
		}
		rows = append(rows, prefix+label+" · "+f.Title)
	}
	return rows
}

func statusLabel(status string) string {
	switch status {
	case "submitted":
		return "FILED"
	case "posting":
		return "POSTING"
	case "dry_run":
		return "DRY RUN"
	default:
		return strings.ToUpper(status)
	}
}

func (d *dashboard) summary(err error) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	status := "stopped"
	if err == nil && !d.stopping {
		status = "complete"
	}
	c := d.progress.Counts
	text := fmt.Sprintf("bfb %s · %s · saved: %d found, %d filed, %d duplicates\n", cleanText(d.cfg.GitHubRepo()), status, c.Found, c.Filed, c.Duplicates)
	for _, f := range d.progress.Findings {
		if initial, ok := d.initial[f.ID]; ok && initial == f.Status {
			continue
		}
		text += fmt.Sprintf("  %s  %s\n", statusLabel(f.Status), cleanText(f.Title))
		if f.URL != "" {
			text += "  " + cleanText(f.URL) + "\n"
		}
		if f.Status == "dry_run" {
			text += cleanMultiline(f.Body) + "\n"
		}
	}
	text += "State: " + cleanText(d.cfg.StateDirectory) + "\n"
	return text
}

func elapsed(d time.Duration) string {
	d = max(0, d).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// Strip control sequences and control characters from repository/agent text.
// Only the renderer itself may write terminal commands (including OSC links).
func cleanText(text string) string {
	var b strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\x1b' {
			if i+1 == len(runes) {
				break
			}
			i++
			switch runes[i] {
			case '[':
				for i++; i < len(runes); i++ {
					if runes[i] >= 0x40 && runes[i] <= 0x7e {
						break
					}
				}
			case ']', 'P', '^', '_':
				for i++; i < len(runes); i++ {
					if runes[i] == '\a' {
						break
					}
					if runes[i] == '\x1b' && i+1 < len(runes) && runes[i+1] == '\\' {
						i++
						break
					}
				}
			}
			continue
		}
		if r == '\n' || r == '\r' || r == '\t' {
			b.WriteByte(' ')
			continue
		}
		if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
func cleanMultiline(text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = cleanText(lines[i])
	}
	return strings.Join(lines, "\n")
}
func clipText(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if uniseg.StringWidth(text) <= width {
		return text
	}
	var b strings.Builder
	g := uniseg.NewGraphemes(text)
	used := 0
	for g.Next() {
		if used+g.Width() > width-1 {
			break
		}
		b.WriteString(g.Str())
		used += g.Width()
	}
	return b.String() + "…"
}
func wrapText(text string, width int) []string {
	text = cleanText(text)
	width = max(1, width)
	var rows []string
	var b strings.Builder
	used := 0
	g := uniseg.NewGraphemes(text)
	for g.Next() {
		if used+g.Width() > width && b.Len() > 0 {
			rows = append(rows, b.String())
			b.Reset()
			used = 0
		}
		if g.Width() > width {
			b.WriteString("…")
			used++
		} else {
			b.WriteString(g.Str())
			used += g.Width()
		}
	}
	return append(rows, b.String())
}
