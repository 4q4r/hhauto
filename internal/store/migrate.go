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
// are never edited (0001 stays byte-identical to what shipped — comments in
// applied migrations are not rewritten either); changes land as new 0002+
// files under migrations/.
//
// Deliberate schema decisions (recorded here because applied migration
// files are immutable):
//
//   - FK asymmetry is intentional. Only resumes, messages and ping_queue
//     carry REFERENCES ... ON DELETE CASCADE. applications, chats, contacts,
//     tests and interview_links reference accounts/vacancies by bare
//     columns: their rows are audit/observability records whose lifecycle
//     must survive the account row (nothing in store may silently destroy
//     history when an account is deleted), and the referenced identifiers
//     originate from external hh.ru ids rather than store-internal keys.
//     Retroactively adding FKs would change deletion semantics for
//     existing consumers, so the choice is documented here instead.
//
//   - NOT NULL is asserted on every UNIQUE/PK-bearing column even where the
//     original contract wrote a bare type: SQLite UNIQUE treats NULLs as
//     distinct, so nullable keys would silently break the dedup contract
//     (see 0001).
var migrations = []migration{
	{version: 1, name: "init", script: readMigration("migrations/0001_init.sql")},
	{version: 2, name: "ai_decisions_cost", script: readMigration("migrations/0002_ai_decisions_cost.sql")},
	{version: 3, name: "resume_clones", script: readMigration("migrations/0003_resume_clones.sql")},
	{version: 4, name: "resume_improve_diff", script: readMigration("migrations/0004_resume_improve_diff.sql")},
	{version: 5, name: "setka_dms", script: readMigration("migrations/0005_setka_dms.sql")},
	{version: 6, name: "captcha_postpone", script: readMigration("migrations/0006_captcha_postpone.sql")},
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
