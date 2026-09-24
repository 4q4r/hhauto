package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AssessmentsAdd inserts a hh-test row and fills a.ID. An empty Status falls
// back to 'detected'.
func (db *DB) AssessmentsAdd(ctx context.Context, a *Assessment) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO tests (account_id, vacancy_id, kind, provider, external_url, status,
		                   answers, result, created_at, finished_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), 'detected'), ?, ?, ?, ?)
		RETURNING id`,
		a.AccountID, a.VacancyID, a.Kind, a.Provider, a.ExternalURL, a.Status,
		a.Answers, a.Result, nowOr(a.CreatedAt), fmtTimeArg(a.FinishedAt),
	).Scan(&a.ID)
	if err != nil {
		return fmt.Errorf("store: AssessmentsAdd: %w", err)
	}
	return nil
}

// AssessmentsUpdateStatus sets the status. result != nil also stores the
// result and stamps finished_at (terminal transition).
func (db *DB) AssessmentsUpdateStatus(ctx context.Context, id int64, status string, result *string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	var err error
	if result != nil {
		_, err = db.conn.ExecContext(ctx,
			"UPDATE tests SET status = ?, result = ?, finished_at = ? WHERE id = ?",
			status, result, fmtTime(time.Now()), id)
	} else {
		_, err = db.conn.ExecContext(ctx,
			"UPDATE tests SET status = ? WHERE id = ?", status, id)
	}
	if err != nil {
		return fmt.Errorf("store: AssessmentsUpdateStatus: %w", err)
	}
	return nil
}

// AssessmentsListByAccount returns the account's tests ordered by ID.
func (db *DB) AssessmentsListByAccount(ctx context.Context, accountID string) ([]Assessment, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, vacancy_id, kind, provider, external_url, status,
		       answers, result, created_at, finished_at
		FROM tests WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: AssessmentsListByAccount: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

	var out []Assessment
	for rows.Next() {
		a, err := scanAssessment(rows)
		if err != nil {
			return nil, fmt.Errorf("store: AssessmentsListByAccount scan: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func scanAssessment(rs rowScanner) (*Assessment, error) {
	var a Assessment
	var vacancy sql.NullInt64
	var provider, url, answers, result sql.NullString
	var created, finished sql.NullString
	if err := rs.Scan(&a.ID, &a.AccountID, &vacancy, &a.Kind, &provider, &url, &a.Status,
		&answers, &result, &created, &finished); err != nil {
		return nil, err
	}
	if vacancy.Valid {
		v := vacancy.Int64
		a.VacancyID = &v
	}
	a.Provider = nullStr(provider)
	a.ExternalURL = nullStr(url)
	a.Answers = nullStr(answers)
	a.Result = nullStr(result)
	var err error
	if a.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if a.FinishedAt, err = ptrTimeFromNull(finished); err != nil {
		return nil, err
	}
	return &a, nil
}
