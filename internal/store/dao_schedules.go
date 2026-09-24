package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SchedulesUpsert inserts or replaces a schedule row by ID (full-row
// semantics: the struct is the source of truth). Empty UpdatedAt is filled.
func (db *DB) SchedulesUpsert(ctx context.Context, s *Schedule) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO schedules (id, account_id, spec, enabled, last_run_at, next_run_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			account_id  = excluded.account_id,
			spec        = excluded.spec,
			enabled     = excluded.enabled,
			last_run_at = excluded.last_run_at,
			next_run_at = excluded.next_run_at,
			updated_at  = excluded.updated_at`,
		s.ID, s.AccountID, s.Spec, boolInt(s.Enabled), fmtTimeArg(s.LastRunAt),
		fmtTimeArg(s.NextRunAt), nowOr(s.UpdatedAt))
	if err != nil {
		return fmt.Errorf("store: SchedulesUpsert: %w", err)
	}
	return nil
}

// SchedulesDue returns enabled schedules whose next_run_at has passed,
// oldest first.
func (db *DB) SchedulesDue(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, spec, enabled, last_run_at, next_run_at, updated_at
		FROM schedules
		WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at
		LIMIT ?`, fmtTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("store: SchedulesDue: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: SchedulesDue scan: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// SchedulesMarkRun stamps the last run and the computed next run.
func (db *DB) SchedulesMarkRun(ctx context.Context, id string, lastRun, nextRun time.Time) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx,
		"UPDATE schedules SET last_run_at = ?, next_run_at = ?, updated_at = ? WHERE id = ?",
		fmtTime(lastRun), fmtTime(nextRun), fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: SchedulesMarkRun: %w", err)
	}
	return nil
}

func scanSchedule(rs rowScanner) (*Schedule, error) {
	var s Schedule
	var enabled sql.NullInt64
	var lastRun, nextRun, updated sql.NullString
	if err := rs.Scan(&s.ID, &s.AccountID, &s.Spec, &enabled, &lastRun, &nextRun, &updated); err != nil {
		return nil, err
	}
	s.Enabled = enabled.Int64 != 0
	var err error
	if s.LastRunAt, err = ptrTimeFromNull(lastRun); err != nil {
		return nil, err
	}
	if s.NextRunAt, err = ptrTimeFromNull(nextRun); err != nil {
		return nil, err
	}
	if s.UpdatedAt, err = timeFromNull(updated); err != nil {
		return nil, err
	}
	return &s, nil
}
