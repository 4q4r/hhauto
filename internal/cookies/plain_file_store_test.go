package cookies

// T48: PlainFileStore — the [storage].cookie_store = "plain" backend. Same
// per-account blob path as the encrypted file store, but plain JSON with
// 0600 permissions; HHAUTO_VAULT_PASS is never read and never prompted.

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

func TestT48PlainFileStoreRoundTrip(t *testing.T) {
	// Prove the passphrase is not involved: an empty env value would make
	// the encrypted store refuse ("empty vault passphrase refused").
	t.Setenv(vaultPassEnv, "")

	s, err := NewPlainFile(StorageConfig{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPlainFile: %v", err)
	}
	if got := s.Backend(); got != "plain-file" {
		t.Fatalf("Backend() = %q, want plain-file", got)
	}

	ctx := context.Background()
	want := []*http.Cookie{
		{Name: "hhuid", Value: "v1", Domain: ".hh.ru", HttpOnly: true},
		{Name: "_xsrf", Value: "v2", Secure: true, Expires: time.Now().Add(time.Hour)},
	}
	if err := s.Save(ctx, "main", want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path := filepath.Join(s.(*plainFileStore).path, "main.bin")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if strings.HasPrefix(string(data), blobMagic) {
		t.Fatal("plain store wrote the vault magic — file is not plain JSON")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat blob: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("blob perms = %o, want 600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir perms = %o, want 700", got)
	}

	got, err := s.Load(ctx, "main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Load = %d cookies, want %d", len(got), len(want))
	}
	for i, c := range got {
		if c.Name != want[i].Name || c.Value != want[i].Value || c.Domain != want[i].Domain || c.HttpOnly != want[i].HttpOnly {
			t.Errorf("cookie[%d] = %+v, want %+v", i, c, want[i])
		}
	}

	if err := s.Delete(ctx, "main"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(ctx, "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after Delete = %v, want ErrNotFound", err)
	}
}

func TestT48PlainFileStoreLoadMissing(t *testing.T) {
	t.Setenv(vaultPassEnv, "")
	s, err := NewPlainFile(StorageConfig{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPlainFile: %v", err)
	}
	if _, err := s.Load(context.Background(), "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load missing = %v, want ErrNotFound", err)
	}
}

// A vault blob left over from a previous cookie_store setting must fail
// loudly with a corruption error, not decode as garbage JSON.
func TestT48PlainFileStoreRejectsVaultBlob(t *testing.T) {
	t.Setenv(vaultPassEnv, "")
	dir := t.TempDir()
	s, err := NewPlainFile(StorageConfig{Path: dir})
	if err != nil {
		t.Fatalf("NewPlainFile: %v", err)
	}
	blob := append([]byte(blobMagic), make([]byte, blobHeaderLen()-len(blobMagic)+16)...)
	if err := os.WriteFile(filepath.Join(dir, "main.bin"), blob, 0o600); err != nil {
		t.Fatalf("write fake vault blob: %v", err)
	}
	if _, err := s.Load(context.Background(), "main"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load vault blob = %v, want ErrCorrupt", err)
	}
}

func TestT48PlainFileStoreValidatesAccountID(t *testing.T) {
	t.Setenv(vaultPassEnv, "")
	s, err := NewPlainFile(StorageConfig{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewPlainFile: %v", err)
	}
	if err := s.Save(context.Background(), "../evil", nil); err == nil {
		t.Fatal("Save with path-traversal id must fail")
	}
	if _, err := NewPlainFile(StorageConfig{Path: "  "}); err == nil {
		t.Fatal("NewPlainFile with empty path must fail")
	}
}
