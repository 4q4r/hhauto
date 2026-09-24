package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PingQueueSchedule schedules a warmup ping: a pending row with attempts=0.
// UNIQUE(chat_id, attempts) makes it idempotent — when a row already exists
// for this chat at attempts=0 the call is a no-op and returns (0, false, nil).
func (db *DB) PingQueueSchedule(ctx context.Context, accountID string, chatID int64, dueAt time.Time, maxAttempts int) (int64, bool, error) {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	var id int64
	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO ping_queue (account_id, chat_id, due_at, attempts, max_attempts, state, created_at)
		VALUES (?, ?, ?, 0, ?, 'pending', ?)
		ON CONFLICT (chat_id, attempts) DO NOTHING
		RETURNING id`,
		accountID, chatID, fmtTime(dueAt), maxAttempts, fmtTime(time.Now()),
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: PingQueueSchedule: %w", err)
	}
	return id, true, nil
}

// PingQueueDue returns pending pings whose due_at has passed, oldest first.
func (db *DB) PingQueueDue(ctx context.Context, now time.Time, limit int) ([]PingJob, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, chat_id, due_at, attempts, max_attempts, state, created_at, sent_at
		FROM ping_queue
		WHERE state = 'pending' AND due_at <= ?
		ORDER BY due_at
		LIMIT ?`, fmtTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: PingQueueDue: %w", err)
	}
	defer rows.Close()

	var out []PingJob
	for rows.Next() {
		j, err := scanPingJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: PingQueueDue scan: %w", err)
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// PingQueueMarkSent marks the ping sent and stamps sent_at.
func (db *DB) PingQueueMarkSent(ctx context.Context, id int64) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx,
		"UPDATE ping_queue SET state = 'sent', sent_at = ? WHERE id = ?",
		fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: PingQueueMarkSent: %w", err)
	}
	return nil
}

// pingByID is an in-package lookup used by tests.
func (db *DB) pingByID(ctx context.Context, id int64) (*PingJob, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, account_id, chat_id, due_at, attempts, max_attempts, state, created_at, sent_at
		FROM ping_queue WHERE id = ?`, id)
	j, err := scanPingJob(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: pingByID: %w", err)
	}
	return j, nil
}

func scanPingJob(rs rowScanner) (*PingJob, error) {
	var j PingJob
	var due, created sql.NullString
	var sent sql.NullString
	if err := rs.Scan(&j.ID, &j.AccountID, &j.ChatID, &due, &j.Attempts, &j.MaxAttempts, &j.State, &created, &sent); err != nil {
		return nil, err
	}
	var err error
	if j.DueAt, err = timeFromNull(due); err != nil {
		return nil, err
	}
	if j.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if j.SentAt, err = ptrTimeFromNull(sent); err != nil {
		return nil, err
	}
	return &j, nil
}
