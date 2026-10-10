package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

// ResumesPruneAbsent deletes the account's stored resume rows whose hash
// is absent from keep (the live shard's hash set from a successful full
// fetch): the store mirrors the shard, so an absent row is a
// deleted-on-hh zombie. Returns the number of rows removed.
func (db *DB) ResumesPruneAbsent(ctx context.Context, accountID string, keep []string) (int64, error) {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	var (
		res sql.Result
		err error
	)
	if len(keep) == 0 {
		// An empty live shard mirrors to an empty store for this account.
		res, err = db.conn.ExecContext(ctx,
			`DELETE FROM resumes WHERE account_id = ?`, accountID)
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
		args := make([]any, 0, len(keep)+1)
		args = append(args, accountID)
		for _, h := range keep {
			args = append(args, h)
		}
		res, err = db.conn.ExecContext(ctx,
			`DELETE FROM resumes WHERE account_id = ? AND hh_resume_id NOT IN (`+placeholders+`)`,
			args...)
	}
	if err != nil {
		return 0, fmt.Errorf("store: ResumesPruneAbsent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: ResumesPruneAbsent rows: %w", err)
	}
	return n, nil
}

// ResumesListByAccount returns the account's resumes ordered by ID.
func (db *DB) ResumesListByAccount(ctx context.Context, accountID string) ([]Resume, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, hh_resume_id, title, status, raw, updated_at
		FROM resumes WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: ResumesListByAccount: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

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
