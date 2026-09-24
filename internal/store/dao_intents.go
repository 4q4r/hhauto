package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// IntentsBegin claims an idempotency key. Behaviour on UNIQUE(key) conflict:
//   - existing intent still pending -> ErrIntentPending (never clobbered);
//   - existing intent done -> ErrIntentDone (hard barrier: a completed
//     intent is never replaced by Begin; re-arming the key is a feature-level
//     RECONCILE decision);
//   - existing intent failed/abandoned -> replaced in place with a fresh
//     intent so the key can be retried.
//
// state is CHECK-enforced (pending|done|failed|abandoned); callers normally
// pass "pending". Returns the new intent ID (ULID).
func (db *DB) IntentsBegin(ctx context.Context, key, state, payload string) (string, error) {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("store: IntentsBegin begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing sql.NullString
	err = tx.QueryRowContext(ctx, "SELECT state FROM intents WHERE key = ?", key).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: IntentsBegin lookup: %w", err)
	}
	if existing.Valid && existing.String == "pending" {
		return "", ErrIntentPending
	}
	if existing.Valid && existing.String == "done" {
		return "", ErrIntentDone
	}

	id := ulid.Make().String()
	now := fmtTime(time.Now())
	if existing.Valid {
		_, err = tx.ExecContext(ctx, `
			UPDATE intents SET intent_id = ?, state = ?, payload = ?, created_at = ?, updated_at = ?
			WHERE key = ?`,
			id, state, payload, now, now, key)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO intents (intent_id, key, state, payload, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, key, state, payload, now, now)
	}
	if err != nil {
		return "", fmt.Errorf("store: IntentsBegin upsert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: IntentsBegin commit: %w", err)
	}
	return id, nil
}

func (db *DB) markIntent(ctx context.Context, intentID, state string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx,
		"UPDATE intents SET state = ?, updated_at = ? WHERE intent_id = ?",
		state, fmtTime(time.Now()), intentID)
	if err != nil {
		return fmt.Errorf("store: IntentsMark%s: %w", state, err)
	}
	return nil
}

// IntentsMarkDone completes the intent.
func (db *DB) IntentsMarkDone(ctx context.Context, intentID string) error {
	return db.markIntent(ctx, intentID, "done")
}

// IntentsMarkFailed marks the intent failed.
func (db *DB) IntentsMarkFailed(ctx context.Context, intentID string) error {
	return db.markIntent(ctx, intentID, "failed")
}

// IntentsMarkAbandoned marks the intent abandoned.
func (db *DB) IntentsMarkAbandoned(ctx context.Context, intentID string) error {
	return db.markIntent(ctx, intentID, "abandoned")
}
