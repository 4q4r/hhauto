// plain-file cookie store (T48) — the [storage].cookie_store = "plain"
// backend. Same per-account blob path and JSON payload as the encrypted
// file store, written WITHOUT encryption: the owner explicitly trades the
// vault passphrase for an unencrypted 0600 file. HHAUTO_VAULT_PASS is
// never read and never prompted in this mode.
//
// Persistence reuses the encrypted store's hygiene: the containing
// directory is created/tightened to 0700, writes are atomic (temp file +
// fsync + rename) with 0600 permissions, and account ids are validated
// against the same allow-list. A file carrying the vault magic fails
// loudly as ErrCorrupt instead of decoding as garbage JSON — switching
// between backends requires a re-import (account remove + add), not a
// silent misread.

package cookies

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// plainFileStore persists cookies as plain JSON, one file per account
// under its directory. It satisfies Store.
type plainFileStore struct {
	path string // directory holding per-account files
}

var _ Store = (*plainFileStore)(nil)

// NewPlainFile builds the unencrypted file store. cfg.Path must name the
// directory that will hold the files; it is created (0700) on first write,
// not at construction.
func NewPlainFile(cfg StorageConfig) (Store, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("cookies: plain backend requires storage path")
	}
	return &plainFileStore{path: cfg.Path}, nil
}

func (s *plainFileStore) Backend() string { return "plain-file" }

func (s *plainFileStore) filePath(accountID string) string {
	return filepath.Join(s.path, accountID+blobFileExt)
}

func (s *plainFileStore) Load(ctx context.Context, accountID string) ([]*http.Cookie, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAccountID(accountID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.filePath(accountID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, accountID)
		}
		return nil, fmt.Errorf("cookies: read blob: %w", err)
	}
	if strings.HasPrefix(string(data), blobMagic) {
		return nil, fmt.Errorf("%w: %s holds a vault blob (encrypted) — switch storage.cookie_store back to \"vault\" or re-import the cookies", ErrCorrupt, accountID)
	}
	return decodeCookieBlob(data)
}

func (s *plainFileStore) Save(ctx context.Context, accountID string, cs []*http.Cookie) error {
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
	return writeFileAtomic(s.filePath(accountID), data)
}

func (s *plainFileStore) Delete(ctx context.Context, accountID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	if err := os.Remove(s.filePath(accountID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, accountID)
		}
		return fmt.Errorf("cookies: remove blob: %w", err)
	}
	return nil
}
