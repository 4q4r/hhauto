package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ApplicationsCreate inserts a new application (dedup_key UNIQUE) and fills
// a.ID and a.CreatedAt (when zero).
func (db *DB) ApplicationsCreate(ctx context.Context, a *Application) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO applications (account_id, run_id, hh_resume_id, vacancy_id, employer_id,
		                          dedup_key, status, letter, fail_reason, correlation_id,
		                          created_at, applied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		a.AccountID, a.RunID, a.HhResumeID, a.VacancyID, a.EmployerID,
		a.DedupKey, a.Status, a.Letter, a.FailReason, a.CorrelationID,
		nowOr(a.CreatedAt), fmtTimeArg(a.AppliedAt),
	).Scan(&a.ID)
	if err != nil {
		return fmt.Errorf("store: ApplicationsCreate: %w", err)
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	return nil
}

// ApplicationsMarkStatus sets the status. failReason != nil overwrites
// fail_reason (pass a pointer to "" to clear it); status 'applied' also
// stamps applied_at.
func (db *DB) ApplicationsMarkStatus(ctx context.Context, id int64, status string, failReason *string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		UPDATE applications SET
			status      = ?,
			fail_reason = COALESCE(?, fail_reason),
			applied_at  = CASE WHEN ? = 'applied' THEN ? ELSE applied_at END
		WHERE id = ?`,
		status, failReason, status, fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: ApplicationsMarkStatus: %w", err)
	}
	return nil
}

// ApplicationsCountToday counts applied applications for account+run whose
// applied_at falls on day ("YYYY-MM-DD", UTC prefix match).
func (db *DB) ApplicationsCountToday(ctx context.Context, accountID, runID, day string) (int, error) {
	var n int
	err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM applications
		WHERE account_id = ? AND run_id = ? AND status = 'applied'
		  AND substr(applied_at, 1, 10) = ?`,
		accountID, runID, day).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: ApplicationsCountToday: %w", err)
	}
	return n, nil
}

// ApplicationsGetByDedupKey returns nil (no error) when not found.
func (db *DB) ApplicationsGetByDedupKey(ctx context.Context, dedupKey string) (*Application, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, account_id, run_id, hh_resume_id, vacancy_id, employer_id,
		       dedup_key, status, letter, fail_reason, correlation_id, created_at, applied_at
		FROM applications WHERE dedup_key = ?`, dedupKey)
	a, err := scanApplication(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: ApplicationsGetByDedupKey: %w", err)
	}
	return a, nil
}

func scanApplication(rs rowScanner) (*Application, error) {
	var a Application
	var letter, failReason, corr sql.NullString
	var created, applied sql.NullString
	if err := rs.Scan(&a.ID, &a.AccountID, &a.RunID, &a.HhResumeID, &a.VacancyID, &a.EmployerID,
		&a.DedupKey, &a.Status, &letter, &failReason, &corr, &created, &applied); err != nil {
		return nil, err
	}
	a.Letter = nullStr(letter)
	a.FailReason = nullStr(failReason)
	a.CorrelationID = nullStr(corr)
	var err error
	if a.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if a.AppliedAt, err = ptrTimeFromNull(applied); err != nil {
		return nil, err
	}
	return &a, nil
}
