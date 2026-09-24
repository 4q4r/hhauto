package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AccountsUpsert inserts or updates an account by ID. On conflict only
// mutable fields are updated (created_at is preserved); an empty Status
// keeps the existing status on update and falls back to 'unknown' on insert.
func (db *DB) AccountsUpsert(ctx context.Context, a Account) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	now := fmtTime(time.Now())
	created := nowOr(a.CreatedAt)
	updated := now
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO accounts (id, display_name, hh_user_id, status, last_ok_at, created_at, updated_at)
		VALUES (?, ?, ?, COALESCE(NULLIF(?, ''), 'unknown'), ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			display_name = excluded.display_name,
			hh_user_id   = excluded.hh_user_id,
			status = CASE WHEN excluded.status = '' THEN accounts.status ELSE excluded.status END,
			last_ok_at   = excluded.last_ok_at,
			updated_at   = excluded.updated_at`,
		a.ID, a.DisplayName, a.HhUserID, a.Status, fmtTimeArg(a.LastOKAt), created, updated)
	if err != nil {
		return fmt.Errorf("store: AccountsUpsert: %w", err)
	}
	return nil
}

// AccountsGet returns nil (no error) when the account does not exist.
func (db *DB) AccountsGet(ctx context.Context, id string) (*Account, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, display_name, hh_user_id, status, last_ok_at, created_at, updated_at
		FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: AccountsGet: %w", err)
	}
	return a, nil
}

// AccountsList returns all accounts ordered by ID.
func (db *DB) AccountsList(ctx context.Context) ([]Account, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, display_name, hh_user_id, status, last_ok_at, created_at, updated_at
		FROM accounts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: AccountsList: %w", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: AccountsList scan: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// AccountsSetStatus transitions an account status (CHECK-enforced values).
func (db *DB) AccountsSetStatus(ctx context.Context, id, status string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx,
		"UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?",
		status, fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: AccountsSetStatus: %w", err)
	}
	return nil
}

func scanAccount(rs rowScanner) (*Account, error) {
	var a Account
	var hhID sql.NullInt64
	var lastOK, created, updated sql.NullString
	if err := rs.Scan(&a.ID, &a.DisplayName, &hhID, &a.Status, &lastOK, &created, &updated); err != nil {
		return nil, err
	}
	if hhID.Valid {
		v := hhID.Int64
		a.HhUserID = &v
	}
	var err error
	if a.LastOKAt, err = ptrTimeFromNull(lastOK); err != nil {
		return nil, err
	}
	if a.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if a.UpdatedAt, err = timeFromNull(updated); err != nil {
		return nil, err
	}
	return &a, nil
}
