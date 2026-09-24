package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ProfileFactsSet upserts a profile fact (account_id, question).
func (db *DB) ProfileFactsSet(ctx context.Context, accountID, question, answer string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO profile_facts (account_id, question, answer, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (account_id, question) DO UPDATE SET
			answer     = excluded.answer,
			updated_at = excluded.updated_at`,
		accountID, question, answer, fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store: ProfileFactsSet: %w", err)
	}
	return nil
}

// ProfileFactsAll returns the account's facts ordered by question.
func (db *DB) ProfileFactsAll(ctx context.Context, accountID string) ([]ProfileFact, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT account_id, question, answer, updated_at
		FROM profile_facts WHERE account_id = ?
		ORDER BY question`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: ProfileFactsAll: %w", err)
	}
	defer rows.Close()

	var out []ProfileFact
	for rows.Next() {
		var f ProfileFact
		var answer, updated sql.NullString
		if err := rows.Scan(&f.AccountID, &f.Question, &answer, &updated); err != nil {
			return nil, fmt.Errorf("store: ProfileFactsAll scan: %w", err)
		}
		f.Answer = answer.String
		if f.UpdatedAt, err = timeFromNull(updated); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
