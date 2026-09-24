// Package cookies persists per-account hh.ru sessions.
// Implementations: OS keyring (99designs/keyring) or encrypted file
// (Argon2id + XChaCha20-Poly1305). Secrets never go to logs.
package cookies

import (
	"context"
	"errors"
	"net/http"
)

// Sentinel errors for cookie-store lookups.
var (
	ErrNotFound = errors.New("cookies: not found")
	ErrDecrypt  = errors.New("cookies: decrypt failed")
	ErrCorrupt  = errors.New("cookies: corrupt store")
)

// Store is the integration contract for session persistence.
type Store interface {
	Load(ctx context.Context, accountID string) ([]*http.Cookie, error)
	Save(ctx context.Context, accountID string, c []*http.Cookie) error
	Delete(ctx context.Context, accountID string) error
	// Backend reports the active storage backend name ("keyring" | "encrypted-file").
	Backend() string
}
