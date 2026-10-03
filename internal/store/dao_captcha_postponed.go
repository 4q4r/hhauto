package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Captcha postpone registry DAOs (T89). A parked vacancy waits for
// retry_at, then the pass re-examines it; the UNIQUE (account, resume,
// vacancy) backstop keeps one row per vacancy.

// CaptchaPostponedPark upserts the parked vacancy: the first park inserts
// (retry_count 1), a re-park bumps retry_count and moves retry_at. Returns
// the retry count so far — the caller settles the vacancy as exhausted when
// it reaches the configured max.
func (db *DB) CaptchaPostponedPark(ctx context.Context, p *CaptchaPostponed) (int, error) {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	now := db.now().UTC()
	var id int64
	var count int
	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO captcha_postponed (account_id, resume_hash, vacancy_id, run_id,
		                               retry_at, retry_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (account_id, resume_hash, vacancy_id) DO UPDATE SET
			run_id      = excluded.run_id,
			retry_at    = excluded.retry_at,
			retry_count = captcha_postponed.retry_count + 1,
			updated_at  = excluded.updated_at
		RETURNING id, retry_count`,
		p.AccountID, p.ResumeHash, p.VacancyID, p.RunID, fmtTime(p.RetryAt), fmtTime(now), fmtTime(now),
	).Scan(&id, &count)
	if err != nil {
		return 0, fmt.Errorf("store: CaptchaPostponedPark: %w", err)
	}
	p.ID = id
	p.RetryCount = count
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	return count, nil
}

// CaptchaPostponedDue lists the account+resume rows whose retry_at has
// passed (oldest first) — the pass-start re-exam source.
func (db *DB) CaptchaPostponedDue(ctx context.Context, accountID, resumeHash string, now time.Time) ([]CaptchaPostponed, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, resume_hash, vacancy_id, run_id, retry_at, retry_count, created_at, updated_at
		FROM captcha_postponed
		WHERE account_id = ? AND resume_hash = ? AND retry_at <= ?
		ORDER BY retry_at`, accountID, resumeHash, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("store: CaptchaPostponedDue: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

	var out []CaptchaPostponed
	for rows.Next() {
		p, err := scanCaptchaPostponed(rows)
		if err != nil {
			return nil, fmt.Errorf("store: CaptchaPostponedDue scan: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// CaptchaPostponedGet returns the row for the (account, resume, vacancy)
// triple or nil (no error) when absent — the wake handler's gate.
func (db *DB) CaptchaPostponedGet(ctx context.Context, accountID, resumeHash string, vacancyID int64) (*CaptchaPostponed, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, account_id, resume_hash, vacancy_id, run_id, retry_at, retry_count, created_at, updated_at
		FROM captcha_postponed WHERE account_id = ? AND resume_hash = ? AND vacancy_id = ?`,
		accountID, resumeHash, vacancyID)
	p, err := scanCaptchaPostponed(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: CaptchaPostponedGet: %w", err)
	}
	return p, nil
}

// CaptchaPostponedDelete settles (or idempotently re-settles) a postpone.
func (db *DB) CaptchaPostponedDelete(ctx context.Context, accountID, resumeHash string, vacancyID int64) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		DELETE FROM captcha_postponed
		WHERE account_id = ? AND resume_hash = ? AND vacancy_id = ?`,
		accountID, resumeHash, vacancyID)
	if err != nil {
		return fmt.Errorf("store: CaptchaPostponedDelete: %w", err)
	}
	return nil
}

func scanCaptchaPostponed(rs rowScanner) (*CaptchaPostponed, error) {
	var p CaptchaPostponed
	var runID, retryAt, created, updated sql.NullString
	if err := rs.Scan(&p.ID, &p.AccountID, &p.ResumeHash, &p.VacancyID, &runID,
		&retryAt, &p.RetryCount, &created, &updated); err != nil {
		return nil, err
	}
	p.RunID = runID.String
	var err error
	if p.RetryAt, err = timeFromNull(retryAt); err != nil {
		return nil, err
	}
	if p.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = timeFromNull(updated); err != nil {
		return nil, err
	}
	return &p, nil
}
