// Package logging builds the process-wide slog.Logger.
//
// Credential redaction is MANDATORY and cannot be disabled: values of
// attributes whose keys look sensitive are masked, and credential
// assignments (e.g. "hhtoken=...") embedded in free-form strings are
// masked in place. Masking shows the first 4 and last 2 characters of
// a secret separated by "***" (short secrets become "***").
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"hhauto/internal/config"
)

// sensitiveKeySubstrings: an attribute key containing any of these
// (case-insensitively) has its whole string value masked.
var sensitiveKeySubstrings = []string{
	"hhtoken", "xsrf", "authorization", "cookie", "password", "passwd",
	"pass", "token", "secret", "key",
}

// credInValueRe finds credential assignments embedded in free-form strings
// (cookies, response bodies): hhtoken=..., XSRF: ..., password="..." etc.
var credInValueRe = regexp.MustCompile(
	`(?i)\b(hhtoken|xsrf|authorization|password|passwd|pass|token|secret|api_?key|key)\s*[=:]\s*("[^"]*"|'[^']*'|[^\s;,}&]+)`)

// maskSecret keeps the first 4 and last 2 characters of a secret; short
// secrets are masked completely so the formula cannot reveal most of them.
func maskSecret(s string) string {
	if len(s) < 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-2:]
}

// redactString masks credential assignments embedded in s.
func redactString(s string) string {
	return credInValueRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := credInValueRe.FindStringSubmatch(match)
		return sub[1] + "=" + maskSecret(strings.Trim(sub[2], `"'`))
	})
}

func containsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeySubstrings {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// redactAttr masks one attribute: sensitive keys are masked entirely, other
// string values are scanned for embedded credentials. Any-typed values
// (errors especially) are formatted and scanned the same way — but left
// untouched when nothing fires, so structured non-string values keep their
// type. Group values are redacted recursively.
func redactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		s := a.Value.String()
		if containsSensitiveKey(a.Key) {
			return slog.String(a.Key, maskSecret(s))
		}
		return slog.String(a.Key, redactString(s))
	case slog.KindGroup:
		attrs := a.Value.Group()
		for i := range attrs {
			attrs[i] = redactAttr(attrs[i])
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(attrs...)}
	case slog.KindAny:
		s := fmt.Sprint(a.Value.Any())
		if containsSensitiveKey(a.Key) {
			return slog.String(a.Key, maskSecret(s))
		}
		if red := redactString(s); red != s {
			return slog.String(a.Key, red)
		}
	}
	return a
}

// redactHandler wraps another slog.Handler and redacts every record.
type redactHandler struct{ next slog.Handler }

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, redactString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		redacted[i] = redactAttr(a)
	}
	return &redactHandler{next: h.next.WithAttrs(redacted)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &redactHandler{next: h.next.WithGroup(name)}
}

// parseLevel maps a config string to a slog level; ok=false for unknown
// values (caller falls back to info with a warning).
func parseLevel(s string) (level slog.Level, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "", "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// openLogFile creates parent directories and opens the log file append-only
// with owner-only permissions.
func openLogFile(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("log.file is required for json format")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

// New builds the application logger from the [log] config section.
//
// Text format goes to stderr; JSON goes to the configured file (created with
// 0600, parent directories included). Redaction is always enabled: requesting
// redact_secrets=false logs a WARN and changes nothing. By contract New has
// no error return, so degraded setups (unknown level, unwritable file) warn
// on the default logger and fall back safely.
func New(cfg config.Log) (*slog.Logger, func() error) {
	if !cfg.RedactSecrets {
		slog.Default().Warn("log.redact_secrets=false requested; credential redaction remains enabled and cannot be disabled")
	}
	level, ok := parseLevel(cfg.Level)
	if !ok {
		slog.Default().Warn("log.level unknown, falling back to info", "configured", cfg.Level)
	}

	var handler slog.Handler
	closeFn := func() error { return nil }

	if cfg.Format == "json" {
		f, err := openLogFile(cfg.File)
		if err != nil {
			slog.Default().Warn("log.file unavailable, json logs fall back to stderr", "file", cfg.File, "err", err)
			handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
		} else {
			handler = slog.NewJSONHandler(f, &slog.HandlerOptions{Level: level})
			closeFn = f.Close
		}
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}

	return slog.New(&redactHandler{next: handler}), closeFn
}
