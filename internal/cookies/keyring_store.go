// OS-keyring cookie store built on github.com/99designs/keyring.
//
// service ("collection") is "hhauto", the item key is the raw accountID and
// the item payload is the same {"cookies":[...]} JSON blob the encrypted
// file store uses, so both backends stay wire-compatible.
//
// Allowed backends are the native, session-backed credential services only:
// Windows Credential Manager, macOS Keychain, Secret Service (GNOME
// Keyring/KWallet via D-Bus), KWallet and pass. Two keyring backends are
// deliberately NOT in the allow list:
//
//   - FileBackend: its opener cannot fail (it performs no availability
//     check), so with it in the list NewKeyring would always succeed, even
//     headless, silently degrading to a second, weaker file format (JOSE/
//     jose2go) instead of reporting ErrUnavailable. That is exactly the
//     silent fallback this package forbids; the encrypted-file store covers
//     this role with Argon2id + XChaCha20-Poly1305.
//   - KeyCtlBackend: enabled, it opens unconditionally on any Linux (kernel
//     user keyring exists without a session) and would likewise defeat the
//     "headless => ErrUnavailable" contract; its content is also
//     session-scoped (keys do not survive reboot in common scopes).
//
// Construction never falls back silently: if no allowed backend can be
// opened (headless CI, no D-Bus, no desktop keystore) NewKeyring returns an
// error wrapping ErrUnavailable and the caller decides what to do.

package cookies

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/99designs/keyring"
)

const keyringServiceName = "hhauto"

// allowedKeyringBackends lists native OS credential services in keyring's
// own priority order (OS-specific first, generic last).
var allowedKeyringBackends = []keyring.BackendType{
	keyring.WinCredBackend,
	keyring.KeychainBackend,
	keyring.SecretServiceBackend,
	keyring.KWalletBackend,
	keyring.PassBackend,
}

// keyringStore adapts 99designs/keyring to the Store contract.
type keyringStore struct {
	ring keyring.Keyring
}

var _ Store = (*keyringStore)(nil)

// NewKeyring opens the OS keyring. StorageConfig.Path is not used by this
// backend. Failure to open any allowed backend yields an error wrapping
// ErrUnavailable; it is the caller's decision to downgrade (only "auto"
// resolution does, and only on ErrUnavailable).
func NewKeyring(_ StorageConfig) (Store, error) {
	ring, err := keyring.Open(keyring.Config{
		ServiceName:     keyringServiceName,
		AllowedBackends: allowedKeyringBackends,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &keyringStore{ring: ring}, nil
}

func (s *keyringStore) Backend() string { return BackendKeyring }

func (s *keyringStore) Load(ctx context.Context, accountID string) ([]*http.Cookie, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAccountID(accountID); err != nil {
		return nil, err
	}
	item, err := s.ring.Get(accountID)
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, accountID)
		}
		return nil, fmt.Errorf("cookies: keyring get: %w", err)
	}
	// item.Data is decoded into fresh http.Cookie structs; the payload is
	// not retained beyond this call (no caching anywhere in the package).
	return decodeCookieBlob(item.Data)
}

func (s *keyringStore) Save(ctx context.Context, accountID string, cs []*http.Cookie) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	data, err := encodeCookieBlob(cs)
	if err != nil {
		return err
	}
	if err := s.ring.Set(keyring.Item{
		Key:   accountID,
		Data:  data,
		Label: keyringServiceName + " session cookies",
	}); err != nil {
		return fmt.Errorf("cookies: keyring set: %w", err)
	}
	return nil
}

func (s *keyringStore) Delete(ctx context.Context, accountID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	if err := s.ring.Remove(accountID); err != nil {
		// Backends disagree on the missing-item error; ErrKeyNotFound is the
		// documented one, anything else is surfaced as-is.
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return fmt.Errorf("%w: %s", ErrNotFound, accountID)
		}
		return fmt.Errorf("cookies: keyring remove: %w", err)
	}
	return nil
}
