package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReviewItemsAdd creates an open review item and returns its ID.
func (db *DB) ReviewItemsAdd(ctx context.Context, accountID, kind, reason, payload string) (int64, error) {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	var id int64
	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO review_items (account_id, kind, reason, payload, status, created_at)
		VALUES (?, ?, ?, ?, 'open', ?)
		RETURNING id`,
		accountID, kind, reason, payload, fmtTime(time.Now()),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: ReviewItemsAdd: %w", err)
	}
	return id, nil
}

// ReviewItemsOpen returns the account's open items, oldest first.
func (db *DB) ReviewItemsOpen(ctx context.Context, accountID string, limit int) ([]ReviewItem, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, kind, reason, payload, status, resolution, created_at, resolved_at
		FROM review_items
		WHERE account_id = ? AND status = 'open'
		ORDER BY created_at, id
		LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: ReviewItemsOpen: %w", err)
	}
	defer rows.Close()

	var out []ReviewItem
	for rows.Next() {
		r, err := scanReviewItem(rows)
		if err != nil {
			return nil, fmt.Errorf("store: ReviewItemsOpen scan: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ReviewItemsResolve closes an item as resolved with a resolution note.
func (db *DB) ReviewItemsResolve(ctx context.Context, id int64, resolution string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx,
		"UPDATE review_items SET status = 'resolved', resolution = ?, resolved_at = ? WHERE id = ?",
		resolution, fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: ReviewItemsResolve: %w", err)
	}
	return nil
}

// reviewByID is an in-package lookup used by tests.
func (db *DB) reviewByID(ctx context.Context, id int64) (*ReviewItem, error) {
	rs := db.conn.QueryRowContext(ctx, `
		SELECT id, account_id, kind, reason, payload, status, resolution, created_at, resolved_at
		FROM review_items WHERE id = ?`, id)
	r, err := scanReviewItem(rs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reviewByID: %w", err)
	}
	return r, nil
}

func scanReviewItem(rs rowScanner) (*ReviewItem, error) {
	var r ReviewItem
	var resolution sql.NullString
	var created, resolved sql.NullString
	if err := rs.Scan(&r.ID, &r.AccountID, &r.Kind, &r.Reason, &r.Payload, &r.Status, &resolution, &created, &resolved); err != nil {
		return nil, err
	}
	r.Resolution = nullStr(resolution)
	var err error
	if r.CreatedAt, err = timeFromNull(created); err != nil {
		return nil, err
	}
	if r.ResolvedAt, err = ptrTimeFromNull(resolved); err != nil {
		return nil, err
	}
	return &r, nil
}
