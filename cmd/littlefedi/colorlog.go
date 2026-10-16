package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/term"
)

// ANSI colors per level, so log lines on a terminal are as easy to scan as
// chi's per-request logger (see httplog.go). Only used when stdout is a
// terminal and NO_COLOR isn't set; JSON stays the default for files, systemd
// and pipes.
const (
	ansiReset  = "\033[0m"
	ansiGray   = "\033[90m"
	ansiCyan   = "\033[36m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
)

func levelColor(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return ansiGray
	case l < slog.LevelWarn:
		return ansiCyan
	case l < slog.LevelError:
		return ansiYellow
	default:
		return ansiRed
	}
}

// stdoutSupportsColor reports whether os.Stdout is an interactive terminal
// that should receive ANSI-colored logs.
func stdoutSupportsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// colorHandler is a minimal slog.Handler printing one colored, human-readable
// line per record. No group support - the codebase never calls WithGroup.
type colorHandler struct {
	w     io.Writer
	level slog.Leveler
	attrs []slog.Attr
}

func newColorHandler(w io.Writer, level slog.Leveler) *colorHandler {
	return &colorHandler{w: w, level: level}
}

func (h *colorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *colorHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(levelColor(r.Level))
	fmt.Fprintf(&b, "%-5s", r.Level.String())
	b.WriteString(ansiReset)
	b.WriteByte(' ')
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	b.WriteByte('\n')
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *colorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &colorHandler{w: h.w, level: h.level, attrs: merged}
}

func (h *colorHandler) WithGroup(string) slog.Handler {
	return h
}
