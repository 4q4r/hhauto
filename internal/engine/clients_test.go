package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
	"hhauto/internal/testutil/tlsfix"
	"hhauto/internal/tlsfp"
)

// stubCapture replaces the auto-capture hook for the duration of the test.
func stubCapture(t *testing.T, fn func(ctx context.Context, cfg *config.Config, log *slog.Logger) (*tlsfp.Spec, error)) {
	t.Helper()
	saved := captureTLSSpec
	captureTLSSpec = fn
	t.Cleanup(func() { captureTLSSpec = saved })
}

// TestLoadBrowserSpecAutoCaptures: no spec on disk -> the capture runs and
// its spec is used (strict TLS parity, zero approximation).
func TestLoadBrowserSpecAutoCaptures(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.General.DataDir = dir
	log := discardLogger()

	calls := 0
	stubCapture(t, func(_ context.Context, _ *config.Config, _ *slog.Logger) (*tlsfp.Spec, error) {
		calls++
		return tlsfix.Spec(t, "captured-ua/1"), nil
	})

	spec, err := loadBrowserSpec(context.Background(), cfg, log)
	if err != nil {
		t.Fatalf("loadBrowserSpec: %v", err)
	}
	if calls != 1 {
		t.Fatalf("capture calls = %d, want 1", calls)
	}
	if spec == nil || spec.UserAgent != "captured-ua/1" {
		t.Fatalf("captured spec not used: %+v", spec)
	}
}

// Corrupt spec -> recapture, never a preset fallback.
func TestLoadBrowserSpecRecapturesCorrupt(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.General.DataDir = dir
	path := tlsfp.DefaultPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt spec: %v", err)
	}

	calls := 0
	stubCapture(t, func(_ context.Context, _ *config.Config, _ *slog.Logger) (*tlsfp.Spec, error) {
		calls++
		return tlsfix.Spec(t, "captured-ua/1"), nil
	})

	spec, err := loadBrowserSpec(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("loadBrowserSpec: %v", err)
	}
	if calls != 1 {
		t.Fatalf("capture calls = %d, want 1", calls)
	}
	if spec == nil {
		t.Fatal("nil spec after recapture")
	}
}

// Capture failure is fatal: no preset-profile fallback may exist.
func TestLoadBrowserSpecCaptureFailureIsFatal(t *testing.T) {
	cfg := &config.Config{}
	cfg.General.DataDir = t.TempDir()
	stubCapture(t, func(_ context.Context, _ *config.Config, _ *slog.Logger) (*tlsfp.Spec, error) {
		return nil, errors.New("no chrome installed")
	})
	if _, err := loadBrowserSpec(context.Background(), cfg, discardLogger()); err == nil {
		t.Fatal("capture failure must fail loudly")
	}

	cfg.Accounts = map[string]config.Account{"a1": {DisplayName: "one", UserAgent: "ua/1"}}
	if _, err := BuildAPIClients(context.Background(), cfg, nil, nil, discardLogger()); err == nil {
		t.Fatal("BuildAPIClients must fail when the automatic capture fails")
	}
}

// T50: the captured browser UA wins over the configured account UA.
func TestResolveUserAgentPrefersCaptured(t *testing.T) {
	spec := tlsfix.Spec(t, "captured-ua/1")

	ua, changed := resolveUserAgent(config.Account{UserAgent: "configured/1"}, spec)
	if !changed || ua != "captured-ua/1" {
		t.Fatalf("ua = %q changed = %v, want captured-ua/1/true", ua, changed)
	}

	// Equal: no override, no log line.
	ua, changed = resolveUserAgent(config.Account{UserAgent: "captured-ua/1"}, spec)
	if changed || ua != "captured-ua/1" {
		t.Fatalf("ua = %q changed = %v, want no override", ua, changed)
	}

	// T82 (owner rule): the configured UA is inert — a spec without a UA
	// is never patched over with the config value; the resolver returns
	// empty and the engine fails loudly instead.
	noUA := *spec
	noUA.UserAgent = ""
	ua, changed = resolveUserAgent(config.Account{UserAgent: "configured/1"}, &noUA)
	if changed || ua != "" {
		t.Fatalf("ua = %q changed = %v, want empty/no-override (config UA is inert)", ua, changed)
	}

	// T82: no spec at all is equally empty — the engine owns the error.
	ua, changed = resolveUserAgent(config.Account{UserAgent: "configured/1"}, nil)
	if changed || ua != "" {
		t.Fatalf("ua = %q changed = %v, want empty/no-override for a nil spec", ua, changed)
	}
}

// T82: a spec that carries no UA must fail the build loudly — the config
// UA must never silently take the wire.
func TestBuildAPIClientsFailsWithoutSpecUA(t *testing.T) {
	cfg := &config.Config{}
	cfg.General.DataDir = t.TempDir()
	cfg.Accounts = map[string]config.Account{"a1": {DisplayName: "one", UserAgent: "configured/1"}}
	bare := tlsfix.Spec(t, "")
	stubCapture(t, func(_ context.Context, _ *config.Config, _ *slog.Logger) (*tlsfp.Spec, error) {
		return bare, nil
	})
	if _, err := BuildAPIClients(context.Background(), cfg, nil, nil, discardLogger()); err == nil {
		t.Fatal("BuildAPIClients must fail when the spec carries no UA (config UA is inert)")
	}
}

func TestBuildAPIClientsBuildsPerAccount(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }() // test teardown
	ck := &fakeCookies{loadOK: true}
	stubCapture(t, func(_ context.Context, _ *config.Config, _ *slog.Logger) (*tlsfp.Spec, error) {
		return tlsfix.Spec(t, "captured-ua/1"), nil
	})

	cfg := &config.Config{}
	cfg.Accounts = map[string]config.Account{
		"a1": {DisplayName: "one", UserAgent: "ua/1"},
		"a2": {DisplayName: "two", UserAgent: "ua/2", ProxyRef: ""},
	}
	apis, err := BuildAPIClients(context.Background(), cfg, nil, ck, discardLogger())
	if err != nil {
		t.Fatalf("BuildAPIClients: %v", err)
	}
	if len(apis) != 2 {
		t.Fatalf("clients = %d, want 2", len(apis))
	}
	for _, id := range []string{"a1", "a2"} {
		if apis[id] == nil {
			t.Errorf("client %s missing", id)
		}
	}

	// The loaded session must be flushable back into the store (cookies
	// seeded from the store are visible to the client snapshot).
	flushCookies(context.Background(), ck, apis, discardLogger())
	if got := ck.savesFor("a1"); got <= 0 {
		t.Errorf("flushCookies saved %d cookies for a1, want > 0", got)
	}
}

func TestBuildAPIClientsFailsClosed(t *testing.T) {
	stubCapture(t, func(_ context.Context, _ *config.Config, _ *slog.Logger) (*tlsfp.Spec, error) {
		return tlsfix.Spec(t, "captured-ua/1"), nil
	})
	// Missing user agent: no longer fatal (T50) — the captured browser
	// UA is the single source of truth and takes over; without a capture
	// the build fails in loadBrowserSpec anyway, so an account can never
	// run without a UA.
	cfg := &config.Config{}
	cfg.Accounts = map[string]config.Account{
		"bad": {DisplayName: "no ua"},
	}
	if _, err := BuildAPIClients(context.Background(), cfg, nil, nil, discardLogger()); err != nil {
		t.Fatalf("captured UA must cover a missing configured UA: %v", err)
	}

	// proxy_ref with no registry: fail closed, never direct fallback.
	cfg2 := &config.Config{}
	cfg2.Accounts = map[string]config.Account{
		"px": {DisplayName: "px", UserAgent: "ua", ProxyRef: "p1"},
	}
	if _, err := BuildAPIClients(context.Background(), cfg2, nil, nil, discardLogger()); err == nil {
		t.Fatal("proxy_ref without registry must fail the build")
	}

	// Unresolvable ref from a registry: same.
	cfg3 := &config.Config{}
	cfg3.Accounts = map[string]config.Account{
		"px": {DisplayName: "px", UserAgent: "ua", ProxyRef: "p1"},
	}
	_, err := BuildAPIClients(context.Background(), cfg3, stubProxyReg{dialErr: errors.New("unknown ref")}, nil, discardLogger())
	if err == nil {
		t.Fatal("unresolvable proxy_ref must fail the build")
	}
}

// T38 regression: the CLI callers (account check, ai status) pass a nil
// logger — the T37 loadBrowserSpec must not panic on it (no captured TLS
// spec in a temp data_dir hits the Warn path).
func TestBuildAPIClientsNilLoggerNoPanic(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.General.DataDir = dir
	cfg.API.Transport = config.TransportTLSClient
	cfg.Accounts = map[string]config.Account{
		"a1": {DisplayName: "one", UserAgent: "ua/1"},
	}
	ck := &fakeCookies{loadOK: true}
	stubCapture(t, func(_ context.Context, _ *config.Config, _ *slog.Logger) (*tlsfp.Spec, error) {
		return tlsfix.Spec(t, "captured-ua/1"), nil
	})
	var cl map[string]hhapi.HHAPI
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("BuildAPIClients panicked with a nil logger: %v", r)
			}
		}()
		cl, err = BuildAPIClients(context.Background(), cfg, nil, ck, nil)
	}()
	if err != nil {
		t.Fatalf("BuildAPIClients: %v", err)
	}
	if cl["a1"] == nil {
		t.Fatal("client a1 missing")
	}
}
