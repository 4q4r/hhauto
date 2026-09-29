package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ANSI SGR color codes for console rendering: improved/applied results
// are green, info cyan, warnings yellow, errors red; dim de-emphasizes
// secondary attributes; reset clears the color.
const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiCyan   = "\033[36m"
	ansiDim    = "\033[2m"
)

// consoleHandler renders records as human-readable console lines: the bare
// message followed by key=value attributes — no time=, level= or msg=
// prefixes. Messages are colored on a terminal (improved green, WARN
// yellow, error red); redirected output stays plain. It sits under the
// redactHandler, so every rendered value is already masked.
type consoleHandler struct {
	w        io.Writer
	color    bool
	minLevel slog.Leveler
	prefix   string
	preset   []slog.Attr
}

// newConsoleHandler builds the console handler. color forces ANSI codes on
// or off; NewConsoleHandler auto-detects a terminal writer.
func newConsoleHandler(w io.Writer, color bool, minLevel slog.Leveler) *consoleHandler {
	return &consoleHandler{w: w, color: color, minLevel: minLevel}
}

// NewConsoleHandler builds the human-readable console handler, enabling
// colors only when w is a terminal and the environment asks for them
// (NO_COLOR and TERM=dumb force plain output).
func NewConsoleHandler(w io.Writer, minLevel slog.Leveler) slog.Handler {
	f, ok := w.(*os.File)
	envPlain := os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
	return newConsoleHandler(w, ok && term.IsTerminal(int(f.Fd())) && !envPlain, minLevel)
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.minLevel.Level()
}

// lineColor picks the ANSI code for a record: errors red, warnings yellow,
// improved/applied results green, everything else info-cyan.
func lineColor(level slog.Level, msg string, attrs []slog.Attr) string {
	switch {
	case level >= slog.LevelError:
		return ansiRed
	case level >= slog.LevelWarn:
		return ansiYellow
	case strings.Contains(msg, "improved") || successAttr(attrs):
		// "improved" (T29 per-field verdict status; "improved <field>
		// waits for owner approval") and applied=true mark success.
		return ansiGreen
	case level >= slog.LevelInfo:
		return ansiCyan
	default:
		return ""
	}
}

// successAttr reports whether attrs mark a successful improvement:
// status=improved or applied=true.
func successAttr(attrs []slog.Attr) bool {
	for _, a := range attrs {
		switch {
		case a.Key == "status" && a.Value.Kind() == slog.KindString && a.Value.String() == "improved":
			return true
		case a.Key == "applied" && a.Value.Kind() == slog.KindBool && a.Value.Bool():
			return true
		}
	}
	return false
}

// isSecondaryAttr marks attribute strings that render dim on a terminal:
// the secondary identifiers (hash, account) that never carry a verdict.
func isSecondaryAttr(s string) bool {
	return strings.HasPrefix(s, "hash=") || strings.HasPrefix(s, "account=")
}

// flattenAttrs renders attrs as flat key=value strings, descending into
// groups with a dotted key prefix.
func flattenAttrs(out []string, prefix string, attrs []slog.Attr) []string {
	for _, a := range attrs {
		if a.Value.Kind() == slog.KindGroup {
			out = flattenAttrs(out, prefix+a.Key+".", a.Value.Group())
			continue
		}
		v := a.Value.String()
		if v == "" || strings.ContainsAny(v, " \t\"'") {
			v = strconv.Quote(v)
		}
		out = append(out, prefix+a.Key+"="+v)
	}
	return out
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	all := append(append([]slog.Attr(nil), h.preset...), recordAttrs(r)...)
	attrs := flattenAttrs(nil, h.prefix, all)

	msg := r.Message
	if h.color {
		if c := lineColor(r.Level, msg, all); c != "" {
			msg = c + msg + ansiReset
		}
		for i, a := range attrs {
			if isSecondaryAttr(a) {
				attrs[i] = ansiDim + a + ansiReset
			}
		}
	}
	line := msg
	if len(attrs) > 0 {
		line = msg + " " + strings.Join(attrs, " ")
	}
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

// recordAttrs materializes the record's own attributes.
func recordAttrs(r slog.Record) []slog.Attr {
	var out []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		out = append(out, a)
		return true
	})
	return out
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	preset := make([]slog.Attr, 0, len(h.preset)+len(attrs))
	preset = append(preset, h.preset...)
	preset = append(preset, attrs...)
	return &consoleHandler{w: h.w, color: h.color, minLevel: h.minLevel, prefix: h.prefix, preset: preset}
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &consoleHandler{w: h.w, color: h.color, minLevel: h.minLevel, prefix: h.prefix + name + ".", preset: h.preset}
}
