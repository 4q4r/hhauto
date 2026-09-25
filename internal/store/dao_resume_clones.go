package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResumeClonesRegister records a clone lineage edge. Idempotent: an existing
// row for clone_hash is never rewritten (lineage is immutable once recorded).
func (db *DB) ResumeClonesRegister(ctx context.Context, c *ResumeClone) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	created := c.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	res, err := db.conn.ExecContext(ctx, `
		INSERT INTO resume_clones (clone_hash, parent_hash, depth, account_id, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (clone_hash) DO NOTHING`,
		c.CloneHash, c.ParentHash, c.Depth, c.AccountID, fmtTime(created),
	)
	if err != nil {
		return fmt.Errorf("store: ResumeClonesRegister: %w", err)
	}
	c.CreatedAt = created
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		c.CreatedAt, _ = db.resumeCloneCreatedAt(ctx, c.CloneHash) // keep the recorded value
	}
	return nil
}

// resumeCloneCreatedAt reads the stored created_at (best effort; zero on
// miss). Used to keep Register side-effect-free for duplicates.
func (db *DB) resumeCloneCreatedAt(ctx context.Context, hash string) (time.Time, error) {
	var raw sql.NullString
	if err := db.conn.QueryRowContext(ctx,
		`SELECT created_at FROM resume_clones WHERE clone_hash = ?`, hash).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return timeFromNull(raw)
}

// ResumeClonesGet returns nil (no error) when the hash is not a tracked clone.
func (db *DB) ResumeClonesGet(ctx context.Context, cloneHash string) (*ResumeClone, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT clone_hash, parent_hash, depth, account_id, created_at
		FROM resume_clones WHERE clone_hash = ?`, cloneHash)
	c, err := scanResumeClone(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: ResumeClonesGet: %w", err)
	}
	return c, nil
}

// ResumeClonesListByAccount returns the account's tracked clones (insert order).
func (db *DB) ResumeClonesListByAccount(ctx context.Context, accountID string) ([]ResumeClone, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT clone_hash, parent_hash, depth, account_id, created_at
		FROM resume_clones WHERE account_id = ? ORDER BY rowid`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: ResumeClonesListByAccount: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

	var out []ResumeClone
	for rows.Next() {
		c, err := scanResumeClone(rows)
		if err != nil {
			return nil, fmt.Errorf("store: ResumeClonesListByAccount scan: %w", err)
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// ResumeClonesCountChildren counts the direct clones of parentHash for the
// account (the max_clones_per_resume gate unit).
func (db *DB) ResumeClonesCountChildren(ctx context.Context, accountID, parentHash string) (int, error) {
	var n int
	err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM resume_clones
		WHERE account_id = ? AND parent_hash = ?`, accountID, parentHash).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: ResumeClonesCountChildren: %w", err)
	}
	return n, nil
}

func scanResumeClone(rs rowScanner) (*ResumeClone, error) {
	var c ResumeClone
	var created sql.NullString
	if err := rs.Scan(&c.CloneHash, &c.ParentHash, &c.Depth, &c.AccountID, &created); err != nil {
		return nil, err
	}
	var err error
	if c.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	return &c, nil
}
