package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// VacanciesSeenUpsert records a seen vacancy; the (account_id, vacancy_id)
// pair is deduplicated with ON CONFLICT DO NOTHING. Returns whether the row
// was newly inserted.
func (db *DB) VacanciesSeenUpsert(ctx context.Context, vs VacancySeen) (bool, error) {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	var id int64
	err := db.conn.QueryRowContext(ctx, `
		INSERT INTO vacancies_seen (account_id, vacancy_id, source, first_seen_at, meta)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (account_id, vacancy_id) DO NOTHING
		RETURNING id`,
		vs.AccountID, vs.VacancyID, vs.Source, fmtTime(vs.FirstSeenAt), vs.Meta,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: VacanciesSeenUpsert: %w", err)
	}
	return true, nil
}
