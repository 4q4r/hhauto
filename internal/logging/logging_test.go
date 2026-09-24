package logging

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hhauto/internal/config"
)

// captureStderr runs fn while os.Stderr points at a pipe and returns
// everything written to it during fn. All logging under test must happen
// inside fn, because the handler binds the writer at construction time.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	written := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		written <- buf.String()
	}()

	fn()

	_ = w.Close() // test teardown
	os.Stderr = old
	return <-written
}

// captureDefaultLog redirects slog.Default to a buffer for the duration of fn.
func captureDefaultLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestTextHandlerRedactsSensitiveAttrs(t *testing.T) {
	out := captureStderr(t, func() {
		logger, closeFn := New(config.Log{Level: "debug", Format: "text", RedactSecrets: true})
		defer closeFn() //nolint:errcheck // no file in text mode
		logger.Info("login succeeded",
			"cookie", "hhtoken=SECRETA1B2C3; xsrf=XSRFSECRET99",
			"user", "ivan",
		)
	})

	if strings.Contains(out, "SECRETA1B2C3") {
		t.Errorf("stderr contains full hhtoken value:\n%s", out)
	}
	if strings.Contains(out, "XSRFSECRET99") {
		t.Errorf("stderr contains full xsrf value:\n%s", out)
	}
	if !strings.Contains(out, "user=ivan") {
		t.Errorf("non-sensitive attrs must stay intact, got:\n%s", out)
	}
	if !strings.Contains(out, "hhto***") {
		t.Errorf("expected masked cookie value hhto***..., got:\n%s", out)
	}
}

// Б3a regression: an any-typed attribute (a wrapped error carrying a URL
// with an api key) must be redacted too — KindAny used to pass through raw.
func TestAnyAttrErrorWithURLKeyIsRedacted(t *testing.T) {
	out := captureStderr(t, func() {
		logger, closeFn := New(config.Log{Level: "debug", Format: "text", RedactSecrets: true})
		defer closeFn() //nolint:errcheck // no file in text mode
		logger.Error("captcha balance failed",
			"err", errors.New(`Get "https://api.2captcha.com/res.php?key=SUPERSECRETKEY&action=getbalance": connection refused`),
		)
	})

	if strings.Contains(out, "SUPERSECRETKEY") {
		t.Errorf("stderr contains the raw key from an error attr:\n%s", out)
	}
	if !strings.Contains(out, "key=SUPE***") {
		t.Errorf("expected masked key in the error attr, got:\n%s", out)
	}
}

// KindAny values that contain nothing sensitive must keep their type.
func TestAnyAttrUntouchedWhenNothingToRedact(t *testing.T) {
	out := captureStderr(t, func() {
		logger, closeFn := New(config.Log{Level: "debug", Format: "json", RedactSecrets: true})
		defer closeFn() //nolint:errcheck // no file in json-to-stderr mode
		logger.Info("counts", "attempts", 3)
	})

	if !strings.Contains(out, `"attempts":3`) {
		t.Errorf("numeric any-attr must keep its JSON type, got:\n%s", out)
	}
}

func TestJSONFileRedactsAttrsAndMessage(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "logs", "sub", "hhauto.log")

	logger, closeFn := New(config.Log{Level: "info", Format: "json", File: logFile, RedactSecrets: true})
	logger.Info("got response with password=hunter2secret in body",
		"token", "SUPERSECRETVALUE",
		"attempt", 1,
	)
	if err := closeFn(); err != nil {
		t.Fatalf("closeFn() = %v, want nil", err)
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	out := string(data)

	if strings.Contains(out, "SUPERSECRETVALUE") {
		t.Errorf("log file contains full token value:\n%s", out)
	}
	if strings.Contains(out, "hunter2secret") {
		t.Errorf("log file contains full password from message:\n%s", out)
	}
	if !strings.Contains(out, "SUPE***UE") {
		t.Errorf("expected masked token SUPE***UE, got:\n%s", out)
	}
	if !strings.Contains(out, "hunt***et") {
		t.Errorf("expected masked password hunt***et in message, got:\n%s", out)
	}

	info, err := os.Stat(logFile)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("log file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRedactionNotDisableable(t *testing.T) {
	// New() with redact_secrets=false: WARN goes to slog.Default() while the
	// logger itself writes to (swapped) stderr — capture both at once.
	var stderr string
	warn := captureDefaultLog(t, func() {
		stderr = captureStderr(t, func() {
			logger, closeFn := New(config.Log{Level: "info", Format: "text", RedactSecrets: false})
			defer closeFn() //nolint:errcheck // no file in text mode
			logger.Info("check", "authorization", "Bearer TOPSECRET123")
		})
	})

	if !strings.Contains(warn, "remains enabled") {
		t.Errorf("expected WARN that redaction remains enabled, got:\n%s", warn)
	}
	if strings.Contains(stderr, "TOPSECRET123") {
		t.Errorf("redaction was disabled by config, stderr contains secret:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Bear***23") {
		t.Errorf("expected masked authorization value Bear***23, got:\n%s", stderr)
	}
}

func TestWithAttrsRedactsPresetValues(t *testing.T) {
	out := captureStderr(t, func() {
		logger, closeFn := New(config.Log{Level: "info", Format: "text", RedactSecrets: true})
		defer closeFn() //nolint:errcheck // no file in text mode
		logger.With("secret", "MYCREDENTIAL99").Info("hello", "plain", "value")
	})

	if strings.Contains(out, "MYCREDENTIAL99") {
		t.Errorf("WithAttrs value was not redacted:\n%s", out)
	}
	if !strings.Contains(out, "plain=value") {
		t.Errorf("non-sensitive attrs must stay intact, got:\n%s", out)
	}
}

func TestLevelFiltering(t *testing.T) {
	out := captureStderr(t, func() {
		logger, closeFn := New(config.Log{Level: "error", Format: "text", RedactSecrets: true})
		defer closeFn() //nolint:errcheck // no file in text mode
		logger.Info("suppressed info")
		logger.Error("visible error")
	})

	if strings.Contains(out, "suppressed info") {
		t.Errorf("info record must be filtered at level=error:\n%s", out)
	}
	if !strings.Contains(out, "visible error") {
		t.Errorf("error record must pass at level=error:\n%s", out)
	}
}

func TestUnknownLevelFallsBackToInfo(t *testing.T) {
	out := captureStderr(t, func() {
		logger, closeFn := New(config.Log{Level: "bogus", Format: "text", RedactSecrets: true})
		defer closeFn() //nolint:errcheck // no file in text mode
		logger.Info("still visible")
	})

	if !strings.Contains(out, "still visible") {
		t.Errorf("unknown level must fall back to info, got:\n%s", out)
	}
}

func TestCloseNoOpForStderr(t *testing.T) {
	_, closeFn := New(config.Log{Level: "info", Format: "text", RedactSecrets: true})
	if err := closeFn(); err != nil {
		t.Fatalf("closeFn() = %v, want nil", err)
	}
}
