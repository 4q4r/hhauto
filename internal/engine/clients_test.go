package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

func TestBuildAPIClientsBuildsPerAccount(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }() // test teardown
	ck := &fakeCookies{loadOK: true}

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
	// Missing user agent: hard error (a missing UA would fingerprint the
	// account as a bot).
	cfg := &config.Config{}
	cfg.Accounts = map[string]config.Account{
		"bad": {DisplayName: "no ua"},
	}
	if _, err := BuildAPIClients(context.Background(), cfg, nil, nil, discardLogger()); err == nil {
		t.Fatal("missing user agent must fail the build")
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
