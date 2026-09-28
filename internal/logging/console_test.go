package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestConsoleFormatHasNoSlogKeys(t *testing.T) {
	var buf bytes.Buffer
	h := newConsoleHandler(&buf, false, slog.LevelInfo)
	logger := slog.New(h)

	logger.Info("resume improve: start", "hash", "abc123", "user", "ivan")
	out := buf.String()

	for _, key := range []string{"time=", "level=", "msg="} {
		if strings.Contains(out, key) {
			t.Errorf("console output must not contain %q, got:\n%s", key, out)
		}
	}
	if !strings.Contains(out, "resume improve: start hash=abc123 user=ivan") {
		t.Errorf("expected bare message plus key=value attrs, got:\n%s", out)
	}
}

func TestConsoleColors(t *testing.T) {
	cases := []struct {
		name  string
		log   func(l *slog.Logger)
		color string
	}{
		{"error is red", func(l *slog.Logger) { l.Error("persist failed", "err", "boom") }, ansiRed},
		{"warn is yellow", func(l *slog.Logger) { l.Warn("profile load failed") }, ansiYellow},
		{"improved msg is green", func(l *slog.Logger) { l.Info("improved about waits for owner approval") }, ansiGreen},
		{"improved status is green", func(l *slog.Logger) {
			l.Info("autoclone: improve field verdict", "field", "about", "status", "improved")
		}, ansiGreen},
		{"plain info is uncolored", func(l *slog.Logger) { l.Info("resume improve: start", "hash", "abc") }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(newConsoleHandler(&buf, true, slog.LevelInfo))
			tc.log(logger)
			out := buf.String()
			if tc.color == "" {
				if strings.Contains(out, "\033[") {
					t.Errorf("plain info must be uncolored, got:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.color) || !strings.Contains(out, ansiReset) {
				t.Errorf("expected color %q plus reset, got:\n%s", tc.color, out)
			}
		})
	}
}

func TestNewConsoleHandlerPlainOnNonTerminal(t *testing.T) {
	var buf bytes.Buffer // not an *os.File terminal
	h := NewConsoleHandler(&buf, slog.LevelInfo)
	ch, ok := h.(*consoleHandler)
	if !ok {
		t.Fatalf("NewConsoleHandler returned %T", h)
	}
	if ch.color {
		t.Error("non-terminal writer must not enable colors")
	}
	_ = h.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelError, "x", 0))
	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("non-terminal output must be plain, got:\n%s", buf.String())
	}
}

func TestConsoleLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newConsoleHandler(&buf, false, slog.LevelWarn))
	logger.Info("hidden")
	logger.Warn("shown")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("info must be filtered at minLevel=warn, got:\n%s", out)
	}
	if !strings.Contains(out, "shown") {
		t.Errorf("warn must pass, got:\n%s", out)
	}
}
