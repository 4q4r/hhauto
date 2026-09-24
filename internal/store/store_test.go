package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), Options{BusyTimeoutMS: 2000})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func truncSec(t time.Time) time.Time { return t.Truncate(time.Second).UTC() }

func TestOpenAppliesMigrations(t *testing.T) {
	db := openTestDB(t)

	var userVersion int
	if err := db.conn.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if userVersion != 1 {
		t.Fatalf("user_version = %d, want 1", userVersion)
	}

	var version int
	var name, appliedAt string
	if err := db.conn.QueryRow("SELECT version, name, applied_at FROM schema_migrations").Scan(&version, &name, &appliedAt); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	if version != 1 || name != "init" || appliedAt == "" {
		t.Fatalf("schema_migrations row = (%d, %q, %q), want (1, init, <non-empty>)", version, name, appliedAt)
	}
}

func TestOpenSetsPragmasOnPooledConns(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		conn, err := db.conn.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}

		var mode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("PRAGMA journal_mode: %v", err)
		}
		if mode != "wal" {
			t.Fatalf("journal_mode = %q, want wal", mode)
		}

		var fk int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("PRAGMA foreign_keys: %v", err)
		}
		if fk != 1 {
			t.Fatalf("foreign_keys = %d, want 1", fk)
		}

		var busy int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("PRAGMA busy_timeout: %v", err)
		}
		if busy != 2000 {
			t.Fatalf("busy_timeout = %d, want 2000", busy)
		}

		var sync int
		if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatalf("PRAGMA synchronous: %v", err)
		}
		if sync != 1 { // 1 == NORMAL
			t.Fatalf("synchronous = %d, want 1 (NORMAL)", sync)
		}

		if err := conn.Close(); err != nil {
			t.Fatalf("close conn: %v", err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reuse.db")

	db1, err := Open(ctx, path, Options{BusyTimeoutMS: 1000})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db1.AccountsUpsert(ctx, Account{ID: "acc-1", DisplayName: "first"}); err != nil {
		t.Fatalf("AccountsUpsert: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	db2, err := Open(ctx, path, Options{BusyTimeoutMS: 1000})
	if err != nil {
		t.Fatalf("second Open (must be idempotent): %v", err)
	}
	defer func() { _ = db2.Close() }()

	accounts, err := db2.AccountsList(ctx)
	if err != nil {
		t.Fatalf("AccountsList after reopen: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "acc-1" {
		t.Fatalf("accounts after reopen = %+v, want single acc-1", accounts)
	}

	var userVersion int
	if err := db2.conn.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read user_version after reopen: %v", err)
	}
	if userVersion != 1 {
		t.Fatalf("user_version after reopen = %d, want 1", userVersion)
	}
}

func TestAccountsRoundtrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	hhID := int64(12345)
	lastOK := truncSec(time.Now())
	acc := Account{ID: "acc-1", DisplayName: "Ivan", HhUserID: &hhID, Status: "ok", LastOKAt: &lastOK}
	if err := db.AccountsUpsert(ctx, acc); err != nil {
		t.Fatalf("AccountsUpsert: %v", err)
	}

	got, err := db.AccountsGet(ctx, "acc-1")
	if err != nil {
		t.Fatalf("AccountsGet: %v", err)
	}
	if got == nil {
		t.Fatal("AccountsGet(acc-1) = nil, want account")
	}
	if got.DisplayName != "Ivan" || got.HhUserID == nil || *got.HhUserID != hhID || got.Status != "ok" {
		t.Fatalf("account roundtrip mismatch: %+v", got)
	}
	if got.LastOKAt == nil || !got.LastOKAt.Equal(lastOK) {
		t.Fatalf("last_ok_at = %v, want %v", got.LastOKAt, lastOK)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("created_at/updated_at must be filled: %+v", got)
	}

	missing, err := db.AccountsGet(ctx, "nope")
	if err != nil || missing != nil {
		t.Fatalf("AccountsGet(missing) = (%v, %v), want (nil, nil)", missing, err)
	}

	firstCreated := got.CreatedAt
	acc.Status = "paused"
	acc.HhUserID = nil
	if err := db.AccountsUpsert(ctx, acc); err != nil {
		t.Fatalf("AccountsUpsert (update): %v", err)
	}
	upd, _ := db.AccountsGet(ctx, "acc-1")
	if upd.Status != "paused" || upd.HhUserID != nil {
		t.Fatalf("update mismatch: %+v", upd)
	}
	if !upd.CreatedAt.Equal(firstCreated) {
		t.Fatalf("created_at changed on update: %v -> %v", firstCreated, upd.CreatedAt)
	}

	list, err := db.AccountsList(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("AccountsList = (%v, %v), want 1 account", list, err)
	}

	if err := db.AccountsSetStatus(ctx, "acc-1", "blocked"); err != nil {
		t.Fatalf("AccountsSetStatus: %v", err)
	}
	blocked, _ := db.AccountsGet(ctx, "acc-1")
	if blocked.Status != "blocked" {
		t.Fatalf("status after AccountsSetStatus = %q, want blocked", blocked.Status)
	}
}
