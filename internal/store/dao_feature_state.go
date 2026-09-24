package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// FeatureStateGet returns the stored value and whether it exists.
func (db *DB) FeatureStateGet(ctx context.Context, feature, accountID, key string) (string, bool, error) {
	var v sql.NullString
	err := db.conn.QueryRowContext(ctx,
		`SELECT value FROM feature_state WHERE feature = ? AND account_id = ? AND key = ?`,
		feature, accountID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: FeatureStateGet: %w", err)
	}
	if !v.Valid {
		return "", true, nil
	}
	return v.String, true, nil
}

// FeatureStateSet upserts a feature-state value.
func (db *DB) FeatureStateSet(ctx context.Context, feature, accountID, key, value string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO feature_state (feature, account_id, key, value, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (feature, account_id, key) DO UPDATE SET
			value      = excluded.value,
			updated_at = excluded.updated_at`,
		feature, accountID, key, value, fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store: FeatureStateSet: %w", err)
	}
	return nil
}
