package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Rejections DAO (T123): one analyzed rejection per (account, vacancy);
// the UNIQUE backstop makes a re-analysis an in-place replace, so repeated
// `rejections analyze` runs stay idempotent.

// RejectionsPut upserts the analysis verdict for (account, vacancy).
func (db *DB) RejectionsPut(ctx context.Context, r *Rejection) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	skills, err := json.Marshal(r.MissingSkills)
	if err != nil {
		return fmt.Errorf("store: RejectionsPut: %w", err)
	}
	if skills == nil {
		skills = []byte("[]")
	}
	now := db.now().UTC()
	res, err := db.conn.ExecContext(ctx, `
		INSERT INTO rejections (account_id, vacancy_id, vacancy_name, employer_name,
		                        negotiation_id, rejected_at, root_cause, evidence_based,
		                        improved_letter, missing_skills, takeaway, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (account_id, vacancy_id) DO UPDATE SET
			vacancy_name    = excluded.vacancy_name,
			employer_name   = excluded.employer_name,
			negotiation_id  = excluded.negotiation_id,
			rejected_at     = excluded.rejected_at,
			root_cause      = excluded.root_cause,
			evidence_based  = excluded.evidence_based,
			improved_letter = excluded.improved_letter,
			missing_skills  = excluded.missing_skills,
			takeaway        = excluded.takeaway,
			created_at      = excluded.created_at`,
		r.AccountID, r.VacancyID, r.VacancyName, r.EmployerName,
		r.NegotiationID, fmtTime(r.RejectedAt), r.RootCause, boolInt(r.EvidenceBased),
		r.ImprovedLetter, string(skills), r.Takeaway, fmtTime(now),
	)
	if err != nil {
		return fmt.Errorf("store: RejectionsPut: %w", err)
	}
	if id, ierr := res.LastInsertId(); ierr == nil {
		r.ID = id
	}
	r.CreatedAt = now
	return nil
}

// RejectionsGet returns the stored analysis for (account, vacancy) or nil
// (no error) when absent — the analyzer's skip gate.
func (db *DB) RejectionsGet(ctx context.Context, accountID string, vacancyID int64) (*Rejection, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT `+rejectionCols+` FROM rejections WHERE account_id = ? AND vacancy_id = ?`,
		accountID, vacancyID)
	r, err := scanRejection(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: RejectionsGet: %w", err)
	}
	return r, nil
}

// RejectionsList returns all stored analyses for the account, newest
// rejection first — the report source.
func (db *DB) RejectionsList(ctx context.Context, accountID string) ([]Rejection, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+rejectionCols+` FROM rejections WHERE account_id = ?
		ORDER BY rejected_at DESC, id DESC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: RejectionsList: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully iterated; close error is not actionable

	var out []Rejection
	for rows.Next() {
		r, err := scanRejection(rows)
		if err != nil {
			return nil, fmt.Errorf("store: RejectionsList scan: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

const rejectionCols = `id, account_id, vacancy_id, vacancy_name, employer_name,
	negotiation_id, rejected_at, root_cause, evidence_based, improved_letter,
	missing_skills, takeaway, created_at`

func scanRejection(rs rowScanner) (*Rejection, error) {
	var r Rejection
	var rejectedAt, created sql.NullString
	var skills string
	if err := rs.Scan(&r.ID, &r.AccountID, &r.VacancyID, &r.VacancyName, &r.EmployerName,
		&r.NegotiationID, &rejectedAt, &r.RootCause, &r.EvidenceBased, &r.ImprovedLetter,
		&skills, &r.Takeaway, &created); err != nil {
		return nil, err
	}
	var err error
	if r.RejectedAt, err = timeFromNull(rejectedAt); err != nil {
		return nil, err
	}
	if r.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if skills != "" {
		if err := json.Unmarshal([]byte(skills), &r.MissingSkills); err != nil {
			return nil, fmt.Errorf("store: missing_skills: %w", err)
		}
	}
	return &r, nil
}
