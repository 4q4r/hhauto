package cookies

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/99designs/keyring"
)

func TestResolveUnknownBackend(t *testing.T) {
	s, err := New(StorageConfig{Backend: "redis", Path: t.TempDir()})
	if err == nil {
		t.Fatalf("New(unknown backend): want error, got store %v", s)
	}
	if s != nil {
		t.Fatalf("New(unknown backend): store must be nil, got %T", s)
	}
	if !strings.Contains(err.Error(), "redis") {
		t.Errorf("error must mention the offending backend value, got: %v", err)
	}
}

func TestResolveExplicitEncryptedFile(t *testing.T) {
	t.Setenv(vaultPassEnv, testPass)
	s, err := New(StorageConfig{Backend: BackendEncryptedFile, Path: t.TempDir()})
	if err != nil {
		t.Fatalf("New(encrypted-file): %v", err)
	}
	if s.Backend() != BackendEncryptedFile {
		t.Fatalf("Backend() = %q, want %q", s.Backend(), BackendEncryptedFile)
	}
	if err := s.Save(testCtx(), testAccountID, sampleCookies()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := s.Load(testCtx(), testAccountID); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestResolveExplicitKeyringErrorIsUnavailable(t *testing.T) {
	_, err := NewKeyring(StorageConfig{})
	if err == nil {
		t.Skip("an OS keyring is available on this host; unavailability semantics not testable here")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewKeyring failure must wrap ErrUnavailable, got: %v", err)
	}
	_, rerr := New(StorageConfig{Backend: BackendKeyring, Path: filepath.Join(t.TempDir(), "unused")})
	if !errors.Is(rerr, ErrUnavailable) {
		t.Fatalf("New(keyring) must surface ErrUnavailable, got: %v", rerr)
	}
}

// TestResolveAutoDetectHeadlessForced proves the CI/headless branch of the
// "auto" resolution on any host: with no reachable D-Bus session bus and a
// PATH that has no `pass` binary, every allowed keyring backend must fail to
// open, NewKeyring must report ErrUnavailable, and "auto" must resolve to
// the encrypted-file store — never to an error, never to keyring.
//
// Note: this simulation is only possible when the test binary itself starts
// without a live D-Bus session. 99designs/keyring registers its D-Bus
// backends conditionally in init() (godbus caches the session connection in
// a package-level singleton), so on a desktop host with a running session
// bus the backends are already registered and connected before any
// t.Setenv can take effect — the honest answer there is t.Skip.
func TestResolveAutoDetectHeadlessForced(t *testing.T) {
	for _, b := range keyring.AvailableBackends() {
		if b == keyring.SecretServiceBackend || b == keyring.KWalletBackend {
			t.Skip("host had a live D-Bus session at process start; headless cannot be simulated in-process (godbus caches the connection)")
		}
	}

	noBinDir := t.TempDir() // empty PATH: exec.LookPath("pass") must fail
	t.Setenv("PATH", noBinDir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/hhauto-test-bus")
	t.Setenv("XDG_RUNTIME_DIR", noBinDir)

	_, kerr := NewKeyring(StorageConfig{})
	if !errors.Is(kerr, ErrUnavailable) {
		t.Fatalf("forced-headless NewKeyring: want ErrUnavailable, got %v", kerr)
	}
	s, err := New(StorageConfig{Backend: BackendAuto, Path: t.TempDir()})
	if err != nil {
		t.Fatalf("New(auto) under forced headless: %v", err)
	}
	if s.Backend() != BackendEncryptedFile {
		t.Fatalf("forced-headless auto: Backend() = %q, want %q", s.Backend(), BackendEncryptedFile)
	}
	// And it must be a fully functional store, not a stub.
	t.Setenv(vaultPassEnv, testPass)
	if err := s.Save(testCtx(), testAccountID, sampleCookies()); err != nil {
		t.Fatalf("Save via headless auto store: %v", err)
	}
	if _, err := s.Load(testCtx(), testAccountID); err != nil {
		t.Fatalf("Load via headless auto store: %v", err)
	}
}

// TestResolveAutoDetect mirrors the spec contract: "auto" (and empty) must use
// the keyring when one is available; otherwise — and this is the CI/headless
// case — it must deterministically fall through to encrypted-file. It must
// never error out or pick anything else.
func TestResolveAutoDetect(t *testing.T) {
	_, kerr := NewKeyring(StorageConfig{})
	headless := errors.Is(kerr, ErrUnavailable)

	for _, backend := range []string{BackendAuto, ""} {
		t.Run("backend="+backend, func(t *testing.T) {
			t.Setenv(vaultPassEnv, testPass)
			s, err := New(StorageConfig{Backend: backend, Path: t.TempDir()})
			if err != nil {
				t.Fatalf("New(%q): %v", backend, err)
			}
			got := s.Backend()
			switch {
			case headless:
				if got != BackendEncryptedFile {
					t.Fatalf("headless auto: Backend() = %q, want %q", got, BackendEncryptedFile)
				}
			case got != BackendKeyring && got != BackendEncryptedFile:
				t.Fatalf("auto: Backend() = %q, want %q or %q", got, BackendKeyring, BackendEncryptedFile)
			}
			// Whatever was picked must be functional.
			if err := s.Save(testCtx(), testAccountID, sampleCookies()); err != nil {
				t.Fatalf("Save via auto-resolved store: %v", err)
			}
			if _, err := s.Load(testCtx(), testAccountID); err != nil {
				t.Fatalf("Load via auto-resolved store: %v", err)
			}
		})
	}
}
