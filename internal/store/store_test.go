package store

import (
	"context"
	"database/sql"
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

// TestMigration0002CostUSDOnLegacyDB verifies the expand-only 0002 migration
// against a database built by 0001 only (ai_decisions without cost_usd):
// Open must ALTER the column in, backfill existing rows with the default,
// and a second Open must stay a no-op.
func TestMigration0002CostUSDOnLegacyDB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	// Exact 0001-shape legacy database: ai_decisions without cost_usd plus
	// the schema_migrations audit table 0001 always creates.
	const legacyDDL = `
		CREATE TABLE schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at TEXT NOT NULL
		);
		CREATE TABLE ai_decisions (
			id           TEXT NOT NULL PRIMARY KEY,
			created_at   TEXT,
			account_id   TEXT,
			task         TEXT,
			entity_type  TEXT,
			entity_id    TEXT,
			run_id       TEXT,
			model        TEXT,
			prompt_ver   TEXT,
			input_digest TEXT,
			verdict      TEXT,
			score        REAL,
			confidence   REAL,
			needs_human  INTEGER,
			injection    INTEGER,
			latency_ms   INTEGER,
			tokens_in    INTEGER,
			tokens_out   INTEGER,
			cache_hit    INTEGER,
			outcome      TEXT NOT NULL DEFAULT 'pending'
		);`
	if _, err := legacy.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (1, 'init', ?)",
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert legacy audit row: %v", err)
	}
	if _, err := legacy.ExecContext(ctx,
		"INSERT INTO ai_decisions (id, created_at, account_id, task, tokens_in, tokens_out) VALUES ('legacy1', ?, 'acc-old', 'letter', 10, 5)",
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := legacy.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("stamp legacy user_version: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	db, err := Open(ctx, path, Options{BusyTimeoutMS: 1000})
	if err != nil {
		t.Fatalf("Open on legacy 0001 db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var userVersion int
	if err := db.conn.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if userVersion != 7 {
		t.Fatalf("user_version = %d, want 7 after 0007", userVersion)
	}
	var count int
	if err := db.conn.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != 7 {
		t.Fatalf("schema_migrations rows = %d, want 7", count)
	}

	// Pre-existing rows are backfilled with the NOT NULL DEFAULT 0.
	var legacyCost float64
	if err := db.conn.QueryRow("SELECT cost_usd FROM ai_decisions WHERE id = 'legacy1'").Scan(&legacyCost); err != nil {
		t.Fatalf("read cost_usd of legacy row: %v", err)
	}
	if legacyCost != 0 {
		t.Fatalf("legacy cost_usd = %v, want 0 (default backfill)", legacyCost)
	}

	// Reopen: idempotent, still v3, data intact.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := Open(ctx, path, Options{BusyTimeoutMS: 1000})
	if err != nil {
		t.Fatalf("reopen legacy db: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	if err := db2.conn.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read user_version after reopen: %v", err)
	}
	if userVersion != 7 {
		t.Fatalf("user_version after reopen = %d, want 7", userVersion)
	}
	var legacyTokensIn int
	if err := db2.conn.QueryRow("SELECT tokens_in FROM ai_decisions WHERE id = 'legacy1'").Scan(&legacyTokensIn); err != nil {
		t.Fatalf("read legacy row after reopen: %v", err)
	}
	if legacyTokensIn != 10 {
		t.Fatalf("legacy row corrupted by migration: tokens_in = %d, want 10", legacyTokensIn)
	}
}

func TestOpenAppliesMigrations(t *testing.T) {
	db := openTestDB(t)

	var userVersion int
	if err := db.conn.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if userVersion != 7 {
		t.Fatalf("user_version = %d, want 7 (0001..0007)", userVersion)
	}

	var count int
	if err := db.conn.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != 7 {
		t.Fatalf("schema_migrations rows = %d, want 7", count)
	}

	var name, appliedAt string
	if err := db.conn.QueryRow("SELECT name, applied_at FROM schema_migrations WHERE version = 1").Scan(&name, &appliedAt); err != nil {
		t.Fatalf("read schema_migrations v1: %v", err)
	}
	if name != "init" || appliedAt == "" {
		t.Fatalf("schema_migrations v1 = (%q, %q), want (init, <non-empty>)", name, appliedAt)
	}
	if err := db.conn.QueryRow("SELECT name FROM schema_migrations WHERE version = 2").Scan(&name); err != nil {
		t.Fatalf("read schema_migrations v2: %v", err)
	}
	if name != "ai_decisions_cost" {
		t.Fatalf("schema_migrations v2 name = %q, want ai_decisions_cost", name)
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
	if userVersion != 7 {
		t.Fatalf("user_version after reopen = %d, want 7", userVersion)
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
