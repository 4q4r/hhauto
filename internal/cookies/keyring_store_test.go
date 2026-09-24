package cookies

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func testCtx() context.Context { return context.Background() }

// TestKeyringRoundtrip exercises the full Save/Load/Delete cycle against a
// real OS keyring. Headless environments (CI without D-Bus / desktop
// keystore) have no keyring: NewKeyring must report ErrUnavailable there,
// and the test skips instead of failing — the encrypted-file path is covered
// separately and unconditionally.
func TestKeyringRoundtrip(t *testing.T) {
	s, err := NewKeyring(StorageConfig{})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			t.Skip("no OS keyring in this environment (expected in CI): skipping keyring roundtrip")
		}
		t.Fatalf("NewKeyring: unexpected error: %v", err)
	}
	if s.Backend() != BackendKeyring {
		t.Fatalf("Backend() = %q, want %q", s.Backend(), BackendKeyring)
	}

	// Unique key so reruns never collide with a previously leaked item.
	id := fmt.Sprintf("t4-selftest-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = s.Delete(context.Background(), id) })

	want := sampleCookies()
	if err := s.Save(context.Background(), id, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertCookiesEqual(t, want, got)

	if err := s.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after Delete: want ErrNotFound, got %v", err)
	}
	// Re-delete is backend-dependent: most native backends map a missing
	// item to ErrKeyNotFound (surfaced as ErrNotFound), but some (e.g. the
	// pass backend) return success for force-removes. Both answers satisfy
	// the contract; the encrypted-file store additionally guarantees
	// ErrNotFound and is tested for that separately.
	if err := s.Delete(context.Background(), id); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete after Delete: got unexpected error %v", err)
	}
}

// TestKeyringInvalidAccountID validates the shared account-id guard without
// requiring a live keyring (NewKeyring result irrelevant: validation must
// happen before any backend I/O).
func TestKeyringInvalidAccountID(t *testing.T) {
	s, err := NewKeyring(StorageConfig{})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			s = &keyringStore{} // validation is store-side, backend not touched
		} else {
			t.Fatalf("NewKeyring: unexpected error: %v", err)
		}
	}
	for _, id := range []string{"", "a/b", "x y", "id\nnl"} {
		if err := s.Save(context.Background(), id, sampleCookies()); err == nil {
			t.Errorf("Save(%q): want validation error, got nil", id)
		}
		// Validation must fire before any backend I/O: a missing item would
		// surface as ErrNotFound, and that is NOT an acceptable answer for
		// an invalid id.
		if _, err := s.Load(context.Background(), id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Load(%q): want validation error, got ErrNotFound/nil", id)
		}
		if err := s.Delete(context.Background(), id); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Delete(%q): want validation error, got ErrNotFound/nil", id)
		}
	}
}
