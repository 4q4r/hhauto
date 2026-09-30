package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Setka DM ledger DAOs (T61). The table is the idempotency record (a repeat
// run never messages the same person twice for one vacancy — UNIQUE backstop
// included), the source of the per-day / per-company / per-person send
// counters and the approve-mode review binding.

// SetkaDMCreate inserts one ledger row and returns its id. A duplicate
// (account_id, person_id, vacancy_id) violates the UNIQUE index — callers
// treat that as "already messaged" and skip.
func (db *DB) SetkaDMCreate(ctx context.Context, dm *SetkaDM) (int64, error) {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	now := db.now().UTC()
	res, err := db.conn.ExecContext(ctx, `
		INSERT INTO setka_dms (account_id, application_dedup_key, vacancy_id, employer_id,
			person_id, person_name, company, mode, status, text, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		dm.AccountID, dm.ApplicationDedupKey, dm.VacancyID, dm.EmployerID,
		dm.PersonID, dm.PersonName, dm.Company, dm.Mode, dm.Status, dm.Text, fmtTime(now))
	if err != nil {
		return 0, fmt.Errorf("store: SetkaDMCreate: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: SetkaDMCreate: %w", err)
	}
	dm.ID = id
	dm.CreatedAt = now
	return id, nil
}

// SetkaDMExists reports whether a row already exists for the
// (account, person, vacancy) triple.
func (db *DB) SetkaDMExists(ctx context.Context, accountID, personID string, vacancyID int64) (bool, error) {
	var one int
	err := db.conn.QueryRowContext(ctx,
		"SELECT 1 FROM setka_dms WHERE account_id = ? AND person_id = ? AND vacancy_id = ? LIMIT 1",
		accountID, personID, vacancyID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: SetkaDMExists: %w", err)
	}
	return true, nil
}

// SetkaDMsCountDay counts SENT rows for the account on the UTC day — the
// max_dm_per_day budget.
func (db *DB) SetkaDMsCountDay(ctx context.Context, accountID, day string) (int, error) {
	return db.countSetka(ctx,
		"account_id = ? AND status = 'sent' AND substr(COALESCE(sent_at, created_at), 1, 10) = ?",
		accountID, day)
}

// SetkaDMsCountDayByEmployer counts SENT rows for one employer on the day —
// the max_dm_per_company budget.
func (db *DB) SetkaDMsCountDayByEmployer(ctx context.Context, accountID string, employerID int64, day string) (int, error) {
	return db.countSetka(ctx,
		"account_id = ? AND employer_id = ? AND status = 'sent' AND substr(COALESCE(sent_at, created_at), 1, 10) = ?",
		accountID, employerID, day)
}

// SetkaDMsCountDayByPerson counts SENT rows for one person on the day —
// the max_dm_per_person budget.
func (db *DB) SetkaDMsCountDayByPerson(ctx context.Context, accountID, personID, day string) (int, error) {
	return db.countSetka(ctx,
		"account_id = ? AND person_id = ? AND status = 'sent' AND substr(COALESCE(sent_at, created_at), 1, 10) = ?",
		accountID, personID, day)
}

func (db *DB) countSetka(ctx context.Context, where string, args ...any) (int, error) {
	var n int
	err := db.conn.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM setka_dms WHERE "+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: setka count: %w", err)
	}
	return n, nil
}

// SetkaDMMarkSent flips a queued_review row to sent and stamps sent_at.
func (db *DB) SetkaDMMarkSent(ctx context.Context, id int64) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()
	_, err := db.conn.ExecContext(ctx,
		"UPDATE setka_dms SET status = 'sent', sent_at = ? WHERE id = ?", fmtTime(db.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("store: SetkaDMMarkSent: %w", err)
	}
	return nil
}

// SetkaDMMarkSkipped flips a queued_review row to skipped with a reason
// (e.g. the human discarded the review).
func (db *DB) SetkaDMMarkSkipped(ctx context.Context, id int64, reason string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()
	_, err := db.conn.ExecContext(ctx,
		"UPDATE setka_dms SET status = 'skipped', reason = ? WHERE id = ?", reason, id)
	if err != nil {
		return fmt.Errorf("store: SetkaDMMarkSkipped: %w", err)
	}
	return nil
}

// SetkaDMSetReview binds the approve-mode review item id to the ledger row.
func (db *DB) SetkaDMSetReview(ctx context.Context, id, reviewID int64) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()
	_, err := db.conn.ExecContext(ctx,
		"UPDATE setka_dms SET review_id = ? WHERE id = ?", reviewID, id)
	if err != nil {
		return fmt.Errorf("store: SetkaDMSetReview: %w", err)
	}
	return nil
}

// SetkaDMGet returns one row by id, or nil when unknown.
func (db *DB) SetkaDMGet(ctx context.Context, id int64) (*SetkaDM, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, account_id, application_dedup_key, vacancy_id, employer_id,
			person_id, person_name, company, mode, status, text, review_id, reason,
			created_at, sent_at
		FROM setka_dms WHERE id = ?`, id)
	dm, err := scanSetkaDM(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: SetkaDMGet: %w", err)
	}
	return dm, nil
}

func scanSetkaDM(rs rowScanner) (*SetkaDM, error) {
	var dm SetkaDM
	var reviewID sql.NullInt64
	var reasonStr sql.NullString
	var created, sent sql.NullString
	if err := rs.Scan(&dm.ID, &dm.AccountID, &dm.ApplicationDedupKey, &dm.VacancyID, &dm.EmployerID,
		&dm.PersonID, &dm.PersonName, &dm.Company, &dm.Mode, &dm.Status, &dm.Text,
		&reviewID, &reasonStr, &created, &sent); err != nil {
		return nil, err
	}
	if reviewID.Valid {
		dm.ReviewID = &reviewID.Int64
	}
	if reasonStr.Valid {
		s := reasonStr.String
		dm.Reason = &s
	}
	var err error
	if dm.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if dm.SentAt, err = ptrTimeFromNull(sent); err != nil {
		return nil, err
	}
	return &dm, nil
}
