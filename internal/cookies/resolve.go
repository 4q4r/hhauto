package cookies

import (
	"errors"
	"fmt"
	"strings"
)

// Backend names reported by Store.Backend and accepted by StorageConfig.Backend.
const (
	BackendKeyring       = "keyring"
	BackendEncryptedFile = "encrypted-file"
	// BackendAuto (or an empty value) resolves to the OS keyring when one is
	// available, and to the encrypted file store otherwise.
	BackendAuto = "auto"
)

// ErrUnavailable reports that no OS keyring backend could be opened on this
// host (for example a headless CI runner without D-Bus). New and NewKeyring
// wrap it; callers can test with errors.Is. Resolution NEVER falls back
// silently: only BackendAuto (and an empty Backend) is allowed to downgrade
// to the encrypted file store, and every other failure is surfaced.
var ErrUnavailable = errors.New("cookies: keyring unavailable")

// StorageConfig selects the cookie storage backend.
//
// Backend: "" or "auto" (detect), "keyring", "encrypted-file". Any other
// value is a hard error — no silent fallbacks.
// Path: directory holding the encrypted blobs for the encrypted-file
// backend; ignored by the keyring backend.
type StorageConfig struct {
	Backend string
	Path    string
}

// New builds the cookie Store described by cfg.
//
//   - "auto"/"": keyring if a native OS backend is available, otherwise the
//     encrypted file store (explicit, observable via Store.Backend).
//   - "keyring": OS keyring only; failure -> error wrapping ErrUnavailable.
//   - "encrypted-file": Argon2id + XChaCha20-Poly1305 file store.
//   - anything else: error (default deny).
func New(cfg StorageConfig) (Store, error) {
	switch normalizeBackend(cfg.Backend) {
	case BackendAuto, "":
		ks, kerr := NewKeyring(cfg)
		if kerr == nil {
			return ks, nil
		}
		if !errors.Is(kerr, ErrUnavailable) {
			// An unexpected keyring error must not be masked by a downgrade.
			return nil, kerr
		}
		fs, ferr := NewEncryptedFile(cfg)
		if ferr != nil {
			return nil, fmt.Errorf("cookies: auto resolution: keyring unavailable and encrypted file unusable: %w", ferr)
		}
		return fs, nil
	case BackendKeyring:
		return NewKeyring(cfg)
	case BackendEncryptedFile:
		return NewEncryptedFile(cfg)
	default:
		return nil, fmt.Errorf("cookies: unknown storage backend %q (want %q, %q or %q)",
			cfg.Backend, BackendAuto, BackendKeyring, BackendEncryptedFile)
	}
}

func normalizeBackend(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validateAccountID guards every account identifier before it reaches a
// backend. For the encrypted file store the id becomes part of a file name,
// so anything outside [A-Za-z0-9._-] is rejected (default deny): this kills
// path traversal, separators and control characters. The suffix appended to
// build the blob file name additionally neutralises "." and "..".
func validateAccountID(accountID string) error {
	if accountID == "" || len(accountID) > 128 {
		return errors.New(`cookies: account id must be non-empty and at most 128 bytes`)
	}
	if accountID == "." || accountID == ".." {
		return errors.New(`cookies: account id must not be "." or ".."`)
	}
	for _, r := range accountID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return errors.New(`cookies: account id must match [A-Za-z0-9._-] (max 128 bytes)`)
		}
	}
	return nil
}
