package config

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the default slog logger: validation intentionally emits
// INFO messages (e.g. direct connections while proxies exist), which would
// otherwise pollute test output.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
