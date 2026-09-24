package store

import (
	"context"
	"embed"
	"fmt"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	script  string
}

// migrations is the ordered, expand-only migration list. Applied migrations
// are never edited; new versions are appended and 0002+ files added under
// migrations/.
var migrations = []migration{
	{version: 1, name: "init", script: readMigration("migrations/0001_init.sql")},
}

func readMigration(name string) string {
	b, err := migrationFS.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("store: embedded migration %s: %v", name, err))
	}
	return string(b)
}

// migrate applies pending migrations. The applied version is tracked in
// PRAGMA user_version; schema_migrations keeps the human-readable audit
// trail. Each migration's script, audit row, and user_version bump commit
// atomically inside one transaction, so a failed migration cannot leave a
// half-applied schema. Re-running Open is a no-op once user_version is
// current (idempotent).
func (db *DB) migrate(ctx context.Context) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	var current int
	if err := db.conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := db.applyMigration(ctx, m); err != nil {
			return err
		}
		current = m.version
	}
	return nil
}

func (db *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migration %d begin: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.script); err != nil {
		return fmt.Errorf("store: migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		m.version, m.name, fmtTime(time.Now()),
	); err != nil {
		return fmt.Errorf("store: migration %d audit row: %w", m.version, err)
	}
	// PRAGMA user_version is transactional in SQLite and commits with the tx.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("store: migration %d set user_version: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %d commit: %w", m.version, err)
	}
	return nil
}
