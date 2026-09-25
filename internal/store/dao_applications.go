package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// ApplicationsListByAccountAndVacancies returns the account's application
// rows for the given vacancy ids (any status). An empty id list returns an
// empty slice without touching the table. The T26 lineage/exhaustion and
// reapply-policy source.
func (db *DB) ApplicationsListByAccountAndVacancies(ctx context.Context, accountID string, vacancyIDs []int64) ([]Application, error) {
	if len(vacancyIDs) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(vacancyIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(vacancyIDs)+1)
	args = append(args, accountID)
	for _, id := range vacancyIDs {
		args = append(args, id)
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, run_id, hh_resume_id, vacancy_id, employer_id,
		       dedup_key, status, letter, fail_reason, correlation_id, created_at, applied_at
		FROM applications
		WHERE account_id = ? AND vacancy_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: ApplicationsListByAccountAndVacancies: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

	var out []Application
	for rows.Next() {
		a, err := scanApplication(rows)
		if err != nil {
			return nil, fmt.Errorf("store: ApplicationsListByAccountAndVacancies scan: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
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

// ApplicationsGetByDedupKeyLive behaves like ApplicationsGetByDedupKey but
// ignores dry_run survey rows: a dry-run record must never block a real
// application (Wave 3 review, T10). Returns nil (no error) when the only
// record is a dry_run row or nothing exists.
func (db *DB) ApplicationsGetByDedupKeyLive(ctx context.Context, dedupKey string) (*Application, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, account_id, run_id, hh_resume_id, vacancy_id, employer_id,
		       dedup_key, status, letter, fail_reason, correlation_id, created_at, applied_at
		FROM applications WHERE dedup_key = ? AND status != 'dry_run'`, dedupKey)
	a, err := scanApplication(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: ApplicationsGetByDedupKeyLive: %w", err)
	}
	return a, nil
}
