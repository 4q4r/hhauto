package store

import (
	"context"
	"fmt"
)

// MetricsIncr increments the counter for (key, day) by one.
func (db *DB) MetricsIncr(ctx context.Context, key, day string) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO metrics ("key", day, value) VALUES (?, ?, 1)
		ON CONFLICT ("key", day) DO UPDATE SET value = value + 1`,
		key, day)
	if err != nil {
		return fmt.Errorf("store: MetricsIncr: %w", err)
	}
	return nil
}

// MetricsSnapshot returns all counters for day ("YYYY-MM-DD") as key→value.
func (db *DB) MetricsSnapshot(ctx context.Context, day string) (map[string]int, error) {
	rows, err := db.conn.QueryContext(ctx,
		`SELECT "key", value FROM metrics WHERE day = ? ORDER BY "key"`, day)
	if err != nil {
		return nil, fmt.Errorf("store: MetricsSnapshot: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: MetricsSnapshot scan: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}
