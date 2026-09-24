package store

import (
	"context"
	"database/sql"
	"fmt"
)

// InterviewLinksUpsert inserts or updates a detected interview link keyed by
// (account_id, url); l.ID receives the row ID. On conflict chat_id, provider
// and detection time are refreshed; status changes go through
// InterviewLinksUpdateStatus.
func (db *DB) InterviewLinksUpsert(ctx context.Context, l *InterviewLink) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO interview_links (account_id, chat_id, url, provider, status, detected_at)
		VALUES (?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'detected'), ?)
		ON CONFLICT (account_id, url) DO UPDATE SET
			chat_id     = excluded.chat_id,
			provider    = excluded.provider,
			detected_at = excluded.detected_at
		RETURNING id`,
		l.AccountID, l.ChatID, l.URL, l.Provider, l.Status, nowOr(l.DetectedAt),
	).Scan(&l.ID)
	if err != nil {
		return fmt.Errorf("store: InterviewLinksUpsert: %w", err)
	}
	return nil
}

// InterviewLinksUpdateStatus transitions a link's status.
func (db *DB) InterviewLinksUpdateStatus(ctx context.Context, id int64, status string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx,
		"UPDATE interview_links SET status = ? WHERE id = ?", status, id)
	if err != nil {
		return fmt.Errorf("store: InterviewLinksUpdateStatus: %w", err)
	}
	return nil
}

// InterviewLinksListByAccount returns the account's links ordered by ID.
func (db *DB) InterviewLinksListByAccount(ctx context.Context, accountID string) ([]InterviewLink, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, chat_id, url, provider, status, detected_at
		FROM interview_links WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: InterviewLinksListByAccount: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

	var out []InterviewLink
	for rows.Next() {
		var l InterviewLink
		var chatID sql.NullInt64
		var detected sql.NullString
		if err := rows.Scan(&l.ID, &l.AccountID, &chatID, &l.URL, &l.Provider, &l.Status, &detected); err != nil {
			return nil, fmt.Errorf("store: InterviewLinksListByAccount scan: %w", err)
		}
		if chatID.Valid {
			v := chatID.Int64
			l.ChatID = &v
		}
		if l.DetectedAt, err = timeFromNull(detected); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
