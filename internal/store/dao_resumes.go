package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ResumesUpsert inserts or updates a resume keyed by
// (account_id, hh_resume_id); the ID is assigned on insert and preserved on
// conflict. r.ID receives the row ID.
func (db *DB) ResumesUpsert(ctx context.Context, r *Resume) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO resumes (account_id, hh_resume_id, title, status, raw, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, hh_resume_id) DO UPDATE SET
			title      = excluded.title,
			status     = excluded.status,
			raw        = excluded.raw,
			updated_at = excluded.updated_at
		RETURNING id`,
		r.AccountID, r.HhResumeID, r.Title, r.Status, r.Raw, fmtTime(r.UpdatedAt),
	).Scan(&r.ID)
	if err != nil {
		return fmt.Errorf("store: ResumesUpsert: %w", err)
	}
	return nil
}

// ResumesListByAccount returns the account's resumes ordered by ID.
func (db *DB) ResumesListByAccount(ctx context.Context, accountID string) ([]Resume, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, hh_resume_id, title, status, raw, updated_at
		FROM resumes WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: ResumesListByAccount: %w", err)
	}
	defer rows.Close()

	var out []Resume
	for rows.Next() {
		var r Resume
		var updated sql.NullString
		if err := rows.Scan(&r.ID, &r.AccountID, &r.HhResumeID, &r.Title, &r.Status, &r.Raw, &updated); err != nil {
			return nil, fmt.Errorf("store: ResumesListByAccount scan: %w", err)
		}
		if r.UpdatedAt, err = timeFromNull(updated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
