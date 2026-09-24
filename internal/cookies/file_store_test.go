package cookies

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testAccountID = "test-acct"
	testPass      = "t4-test-passphrase"
)

// newTestFileStore returns an encryptedFileStore rooted at a temp directory
// with the vault passphrase sourced from the environment (no prompt).
func newTestFileStore(t *testing.T) Store {
	t.Helper()
	t.Setenv(vaultPassEnv, testPass)
	s, err := NewEncryptedFile(StorageConfig{Backend: BackendEncryptedFile, Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEncryptedFile: %v", err)
	}
	return s
}

func sampleCookies() []*http.Cookie {
	exp := time.Date(2027, 3, 1, 12, 30, 0, 0, time.UTC)
	return []*http.Cookie{
		{Name: "hhtoken", Value: "tok-abc123", Domain: ".hh.ru", Path: "/", Expires: exp, Secure: true, HttpOnly: true},
		{Name: "__ddg1", Value: "ddg-val", Domain: "hh.ru", Path: "/", Secure: true},
		{Name: "_xsrf", Value: "xsrf-token"},
		{Name: "noexpiry", Value: "v1"},
	}
}

func assertCookiesEqual(t *testing.T, want, got []*http.Cookie) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("cookie count: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.Name != g.Name || w.Value != g.Value || w.Domain != g.Domain || w.Path != g.Path || w.Secure != g.Secure || w.HttpOnly != g.HttpOnly {
			t.Errorf("cookie[%d] mismatch:\nwant %+v\ngot  %+v", i, w, g)
			continue
		}
		if !w.Expires.Equal(g.Expires) {
			t.Errorf("cookie[%d] %s: Expires: want %v, got %v", i, w.Name, w.Expires, g.Expires)
		}
	}
}

func TestEncryptedFileRoundtrip(t *testing.T) {
	s := newTestFileStore(t)
	if s.Backend() != BackendEncryptedFile {
		t.Fatalf("Backend() = %q, want %q", s.Backend(), BackendEncryptedFile)
	}
	ctx := context.Background()

	want := sampleCookies()
	if err := s.Save(ctx, testAccountID, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, testAccountID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertCookiesEqual(t, want, got)

	// Overwrite with a smaller set must fully replace the blob.
	want2 := want[:1]
	if err := s.Save(ctx, testAccountID, want2); err != nil {
		t.Fatalf("Save (overwrite): %v", err)
	}
	got2, err := s.Load(ctx, testAccountID)
	if err != nil {
		t.Fatalf("Load (overwrite): %v", err)
	}
	assertCookiesEqual(t, want2, got2)

	if err := s.Delete(ctx, testAccountID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx, testAccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after Delete: want ErrNotFound, got %v", err)
	}
}

func TestEncryptedFileEmptyListRoundtrip(t *testing.T) {
	s := newTestFileStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, testAccountID, nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
	got, err := s.Load(ctx, testAccountID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load after Save(nil): got %d cookies, want 0", len(got))
	}
}

func TestEncryptedFileNotFound(t *testing.T) {
	s := newTestFileStore(t)
	ctx := context.Background()
	if _, err := s.Load(ctx, testAccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, testAccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: want ErrNotFound, got %v", err)
	}
}

func TestEncryptedFileWrongPassphrase(t *testing.T) {
	s := newTestFileStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, testAccountID, sampleCookies()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv(vaultPassEnv, "definitely-wrong-passphrase")
	_, err := s.Load(ctx, testAccountID)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("Load with wrong passphrase: want ErrDecrypt, got %v", err)
	}
}

func TestEncryptedFileCorrupt(t *testing.T) {
	header := blobMagic + string([]byte{blobVersion1}) + string(make([]byte, argonSaltLen)) + string(make([]byte, nonceLenX))
	cases := map[string][]byte{
		"garbage":     bytes64('A'),
		"bad-magic":   []byte("HHAUTO0\n" + header[len(blobMagic):]),
		"bad-version": []byte(blobMagic + string([]byte{0x02}) + header[len(blobMagic)+1:]),
		"truncated":   []byte(blobMagic),
		"empty":       {},
	}
	// json-garbage: decrypts cleanly with the right passphrase but is not JSON.
	blob, err := encryptTestBlob(t, []byte("this is not json at all"))
	if err != nil {
		t.Fatalf("encryptTestBlob: %v", err)
	}
	cases["json-garbage"] = blob

	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTestFileStore(t)
			blobPath := s.(*encryptedFileStore).blobPath(testAccountID)
			if err := os.MkdirAll(filepath.Dir(blobPath), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(blobPath, data, 0o600); err != nil {
				t.Fatalf("write corrupt fixture: %v", err)
			}
			if _, err := s.Load(context.Background(), testAccountID); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Load corrupt (%s): want ErrCorrupt, got %v", name, err)
			}
		})
	}
}

func TestEncryptedFileBadCiphertextIsDecryptError(t *testing.T) {
	s := newTestFileStore(t)
	// Structurally valid header, ciphertext that fails AEAD verification —
	// and the degenerate "header with no ciphertext" form. Both parse as
	// version-1 blobs and must therefore fail as ErrDecrypt, not ErrCorrupt.
	header := blobMagic + string([]byte{blobVersion1}) + string(make([]byte, argonSaltLen)) + string(make([]byte, nonceLenX))
	for name, blob := range map[string][]byte{
		"tampered-ciphertext": []byte(header + "definitely-not-authenticated-ciphertext"),
		"header-only":         []byte(header),
	} {
		t.Run(name, func(t *testing.T) {
			blobPath := s.(*encryptedFileStore).blobPath(testAccountID)
			if err := os.MkdirAll(filepath.Dir(blobPath), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(blobPath, blob, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := s.Load(context.Background(), testAccountID); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("Load bad ciphertext (%s): want ErrDecrypt, got %v", name, err)
			}
		})
	}
}

func TestEncryptedFileAtomicNoTmpLeft(t *testing.T) {
	s := newTestFileStore(t)
	dir := s.(*encryptedFileStore).path
	if err := s.Save(context.Background(), testAccountID, sampleCookies()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	blobSeen := false
	for _, e := range entries {
		// CreateTemp appends a random suffix after ".tmp"; match loosely.
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after Save: %s", e.Name())
		}
		if e.Name() == filepath.Base(s.(*encryptedFileStore).blobPath(testAccountID)) {
			blobSeen = true
		}
	}
	if !blobSeen {
		t.Errorf("blob file not found in %s", dir)
	}
}

func TestEncryptedFilePerms(t *testing.T) {
	s := newTestFileStore(t)
	st := s.(*encryptedFileStore)
	if err := s.Save(context.Background(), testAccountID, sampleCookies()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(st.blobPath(testAccountID))
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("blob perms = %o, want 600", got)
	}
	dirInfo, err := os.Stat(st.path)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("dir perms = %o, want 700", got)
	}
}

func TestEncryptedFilePromptFailureNotSilent(t *testing.T) {
	// Env present but empty => treated as unset => prompt path. Prompt is
	// stubbed to a non-TTY failure; Save/Load must surface the error and
	// NEVER silently fall back to a default passphrase.
	t.Setenv(vaultPassEnv, "")
	orig := stdinPasswordReader
	stdinPasswordReader = func(int) ([]byte, error) { return nil, errors.New("not a terminal") }
	defer func() { stdinPasswordReader = orig }()

	s, err := NewEncryptedFile(StorageConfig{Backend: BackendEncryptedFile, Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEncryptedFile: %v", err)
	}
	ctx := context.Background()
	if err := s.Save(ctx, testAccountID, sampleCookies()); err == nil {
		t.Fatal("Save with unavailable passphrase: want error, got nil")
	}

	// Missing file must be reported as ErrNotFound without ever prompting.
	if _, err := s.Load(ctx, testAccountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing with unavailable passphrase: want ErrNotFound, got %v", err)
	}
}

func TestEncryptedFileInvalidAccountID(t *testing.T) {
	s := newTestFileStore(t)
	ctx := context.Background()
	for _, id := range []string{"", "a/b", "..", "x y", "кириллица", "id\nnewline"} {
		if err := s.Save(ctx, id, sampleCookies()); err == nil {
			t.Errorf("Save(%q): want error, got nil", id)
		}
		if _, err := s.Load(ctx, id); err == nil {
			t.Errorf("Load(%q): want error, got nil", id)
		}
		if _, err := s.Load(ctx, id); err != nil && errors.Is(err, ErrNotFound) && id == "" {
			// empty id must be rejected by validation, not reported as missing
			t.Errorf("Load(\"\"): validation error expected, got ErrNotFound")
		}
	}
}

// encryptTestBlob produces a valid version-1 blob for plaintext using the
// standard test passphrase (white-box helper on top of encryptBlob).
func encryptTestBlob(t *testing.T, plaintext []byte) ([]byte, error) {
	t.Helper()
	t.Setenv(vaultPassEnv, testPass)
	pass, err := resolveVaultPassphrase()
	if err != nil {
		return nil, err
	}
	defer clear(pass)
	return encryptBlob(pass, plaintext)
}

func bytes64(b byte) []byte {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return out
}
