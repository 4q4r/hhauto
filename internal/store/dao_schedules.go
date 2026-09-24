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

// SchedulesListAll returns every schedule row (enabled and disabled),
// ordered by ID. The engine's startup materialization uses it to preserve
// persisted last_run_at/next_run_at across restarts (full-row Upsert would
// re-roll the one-shot jitter and erase last-run bookkeeping).
func (db *DB) SchedulesListAll(ctx context.Context) ([]Schedule, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, account_id, spec, enabled, last_run_at, next_run_at, updated_at
		FROM schedules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: SchedulesListAll: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: SchedulesListAll scan: %w", err)
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// SchedulesSetEnabled flips only the enabled column — last_run_at and
// next_run_at are never touched (partial update: the persisted schedule
// plan belongs to the scheduler, not to enable toggles).
func (db *DB) SchedulesSetEnabled(ctx context.Context, id string, enabled bool) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	res, err := db.conn.ExecContext(ctx,
		"UPDATE schedules SET enabled = ?, updated_at = ? WHERE id = ?",
		boolInt(enabled), fmtTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("store: SchedulesSetEnabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: SchedulesSetEnabled: schedule %q not found", id)
	}
	return nil
}
