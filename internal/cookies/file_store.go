// encrypted-file cookie store.
//
// Blob layout (version 1):
//
//	"HHAUTO1\n" (8-byte magic) | 0x01 (version) | salt[16] | nonce[24] | XChaCha20-Poly1305 ciphertext
//
// The whole header (magic, version, salt, nonce) is bound to the ciphertext
// as AEAD associated data, so any bit flip in it fails authentication.
//
// KDF: Argon2id (golang.org/x/crypto/argon2) with the OWASP-recommended
// interactive parameters m=19456 KiB (19 MiB), t=2, p=1; the derived key is
// 32 bytes (chacha20poly1305.KeySize). A fresh random salt and nonce are
// generated for every Save.
//
// Passphrase sources, in strict order:
//  1. environment variable HHAUTO_VAULT_PASS (a non-empty value);
//  2. an interactive terminal prompt (golang.org/x/term.ReadPassword, echo
//     disabled, hint printed to stderr).
//
// There is no third source and no silent default: if neither is available
// the operation fails loudly. The passphrase is never cached and never
// logged; an empty passphrase is refused.
//
// Persistence: blobs live under StorageConfig.Path (directory, enforced
// 0700), one file per account ("<accountID>.bin"). Writes are atomic
// (temp file + fsync + rename) with 0600 permissions; a failed write never
// leaves a .tmp file behind.
//
// Memory hygiene: derived keys, passphrases and plaintext buffers are zeroed
// with clear() once an operation finishes, and cookie values are never
// cached beyond the call that uses them. Residual risk: Go's garbage
// collector may have already copied byte slices and immutable strings before
// we can zero them, so wiping is best-effort hardening, not a guarantee — a
// process memory snapshot could still contain cookie values. This is an
// accepted, documented limitation of handling secrets in a GC language.
package cookies

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/term"
)

const (
	blobMagic       = "HHAUTO1\n"
	blobVersion1    = 0x01
	argonSaltLen    = 16
	argonTime       = 2
	argonMemoryKiB  = 19456 // 19 MiB, OWASP interactive recommendation
	argonThreads    = 1
	blobFileExt     = ".bin"
	vaultPassEnv    = "HHAUTO_VAULT_PASS"
	nonceLenX       = chacha20poly1305.NonceSizeX // 24
	vaultPromptLine = "Enter hhauto vault passphrase: "
)

// stdinPasswordReader is the terminal prompt injection point (tests replace
// it; production uses term.ReadPassword, which disables echo).
var stdinPasswordReader = term.ReadPassword

// cookieBlob is the on-disk (and keyring) payload: {"cookies":[...]}.
// Only the fields hh.ru sessions rely on are serialized — no Raw data.
type cookieBlob struct {
	Cookies []storedCookie `json:"cookies"`
}

type storedCookie struct {
	Name     string     `json:"name"`
	Value    string     `json:"value"`
	Domain   string     `json:"domain,omitempty"`
	Path     string     `json:"path,omitempty"`
	Expires  *time.Time `json:"expires,omitempty"`
	Secure   bool       `json:"secure,omitempty"`
	HTTPOnly bool       `json:"httponly,omitempty"`
}

// encryptedFileStore persists cookies as encrypted blobs, one file per
// account under its directory. It satisfies Store.
type encryptedFileStore struct {
	path string // directory holding per-account blobs
}

var _ Store = (*encryptedFileStore)(nil)

// NewEncryptedFile builds the Argon2id + XChaCha20-Poly1305 file store.
// cfg.Path must name the directory that will hold the blobs; it is created
// (0700) on first write, not at construction.
func NewEncryptedFile(cfg StorageConfig) (Store, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("cookies: encrypted-file backend requires storage path")
	}
	return &encryptedFileStore{path: cfg.Path}, nil
}

func (s *encryptedFileStore) Backend() string { return BackendEncryptedFile }

func (s *encryptedFileStore) blobPath(accountID string) string {
	return filepath.Join(s.path, accountID+blobFileExt)
}

func (s *encryptedFileStore) Load(ctx context.Context, accountID string) ([]*http.Cookie, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAccountID(accountID); err != nil {
		return nil, err
	}
	// Existence is checked before the passphrase is resolved: a missing file
	// must yield ErrNotFound without prompting.
	data, err := os.ReadFile(s.blobPath(accountID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, accountID)
		}
		return nil, fmt.Errorf("cookies: read blob: %w", err)
	}
	defer clear(data)
	pass, err := resolveVaultPassphrase()
	if err != nil {
		return nil, err
	}
	defer clear(pass)
	plaintext, err := decryptBlob(pass, data)
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	return decodeCookieBlob(plaintext)
}

func (s *encryptedFileStore) Save(ctx context.Context, accountID string, cs []*http.Cookie) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	plaintext, err := encodeCookieBlob(cs)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	pass, err := resolveVaultPassphrase()
	if err != nil {
		return err
	}
	defer clear(pass)
	blob, err := encryptBlob(pass, plaintext)
	if err != nil {
		return err
	}
	defer clear(blob)
	return writeFileAtomic(s.blobPath(accountID), blob)
}

func (s *encryptedFileStore) Delete(ctx context.Context, accountID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateAccountID(accountID); err != nil {
		return err
	}
	if err := os.Remove(s.blobPath(accountID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, accountID)
		}
		return fmt.Errorf("cookies: remove blob: %w", err)
	}
	return nil
}

// resolveVaultPassphrase sources the vault passphrase: env first, then an
// interactive prompt. It never returns a silent default.
func resolveVaultPassphrase() ([]byte, error) {
	if p := os.Getenv(vaultPassEnv); p != "" {
		return []byte(p), nil
	}
	fmt.Fprint(os.Stderr, vaultPromptLine)
	pw, err := stdinPasswordReader(int(os.Stdin.Fd()))
	if err != nil {
		clear(pw)
		return nil, fmt.Errorf("cookies: vault passphrase prompt failed (set %s for non-interactive runs): %w", vaultPassEnv, err)
	}
	defer clear(pw)
	if len(pw) == 0 {
		return nil, errors.New("cookies: empty vault passphrase refused")
	}
	out := make([]byte, len(pw))
	copy(out, pw) // pw is cleared by the defer above; hand out our own copy
	return out, nil
}

func deriveVaultKey(pass, salt []byte) []byte {
	return argon2.IDKey(pass, salt, argonTime, argonMemoryKiB, argonThreads, chacha20poly1305.KeySize)
}

func blobHeaderLen() int {
	return len(blobMagic) + 1 + argonSaltLen + nonceLenX
}

// encryptBlob seals plaintext into the version-1 wire format. The passphrase
// is owned by the caller and not modified here.
func encryptBlob(pass, plaintext []byte) ([]byte, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("cookies: generate salt: %w", err)
	}
	nonce := make([]byte, nonceLenX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("cookies: generate nonce: %w", err)
	}
	key := deriveVaultKey(pass, salt)
	defer clear(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("cookies: init AEAD: %w", err)
	}
	blob := make([]byte, 0, blobHeaderLen()+len(plaintext)+chacha20poly1305.Overhead)
	blob = append(blob, blobMagic...)
	blob = append(blob, blobVersion1)
	blob = append(blob, salt...)
	blob = append(blob, nonce...)
	header := len(blob)
	// The full header travels as associated data.
	return aead.Seal(blob, nonce, plaintext, blob[:header]), nil
}

// decryptBlob opens a version-1 blob. Wrong magic/version/truncation ->
// ErrCorrupt; AEAD verification failure (e.g. wrong passphrase) ->
// ErrDecrypt; tampered header -> ErrDecrypt (authenticated via AAD).
func decryptBlob(pass, blob []byte) ([]byte, error) {
	if len(blob) < blobHeaderLen() {
		return nil, ErrCorrupt
	}
	if string(blob[:len(blobMagic)]) != blobMagic {
		return nil, ErrCorrupt
	}
	if blob[len(blobMagic)] != blobVersion1 {
		return nil, ErrCorrupt
	}
	header := blobHeaderLen()
	salt := blob[len(blobMagic)+1 : len(blobMagic)+1+argonSaltLen]
	nonce := blob[header-nonceLenX : header]
	key := deriveVaultKey(pass, salt)
	defer clear(key)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, ErrCorrupt
	}
	pt, err := aead.Open(nil, nonce, blob[header:], blob[:header])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	return pt, nil
}

func encodeCookieBlob(cs []*http.Cookie) ([]byte, error) {
	blob := cookieBlob{Cookies: make([]storedCookie, 0, len(cs))}
	for _, c := range cs {
		if c == nil {
			continue
		}
		sc := storedCookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HttpOnly,
		}
		if !c.Expires.IsZero() {
			e := c.Expires
			sc.Expires = &e
		}
		blob.Cookies = append(blob.Cookies, sc)
	}
	data, err := json.Marshal(blob)
	if err != nil {
		return nil, fmt.Errorf("cookies: encode blob: %w", err)
	}
	return data, nil
}

func decodeCookieBlob(data []byte) ([]*http.Cookie, error) {
	var blob cookieBlob
	if err := json.Unmarshal(data, &blob); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	cs := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, sc := range blob.Cookies {
		c := &http.Cookie{
			Name:     sc.Name,
			Value:    sc.Value,
			Domain:   sc.Domain,
			Path:     sc.Path,
			Secure:   sc.Secure,
			HttpOnly: sc.HTTPOnly,
		}
		if sc.Expires != nil {
			c.Expires = *sc.Expires
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// writeFileAtomic writes data to path via temp file + fsync + rename.
// The containing directory is created/tightened to 0700, the file is 0600,
// and no .tmp file survives either success or failure.
func writeFileAtomic(path string, data []byte) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cookies: create storage dir: %w", err)
	}
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm() != 0o700 {
		// Tighten directories that pre-existed with looser permissions.
		_ = os.Chmod(dir, 0o700)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return fmt.Errorf("cookies: create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if retErr != nil {
			_ = f.Close()
			_ = os.Remove(tmp) // never leave a .tmp behind on failure
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("cookies: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("cookies: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cookies: close temp file: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("cookies: chmod temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("cookies: rename blob into place: %w", err)
	}
	return nil
}
