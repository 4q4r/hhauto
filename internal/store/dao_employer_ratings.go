package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EmployerRatingsPut upserts an employer rating snapshot.
func (db *DB) EmployerRatingsPut(ctx context.Context, r EmployerRating) error {
	db.writeMu.Lock()
	defer db.writeMu.Unlock()

	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO employer_ratings (employer_id, score_all, score_recent, period_days,
		                              reviews_count, recommend_pct, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (employer_id) DO UPDATE SET
			score_all     = excluded.score_all,
			score_recent  = excluded.score_recent,
			period_days   = excluded.period_days,
			reviews_count = excluded.reviews_count,
			recommend_pct = excluded.recommend_pct,
			fetched_at    = excluded.fetched_at`,
		r.EmployerID, r.ScoreAll, r.ScoreRecent, r.PeriodDays, r.ReviewsCount,
		r.RecommendPct, fmtTime(r.FetchedAt))
	if err != nil {
		return fmt.Errorf("store: EmployerRatingsPut: %w", err)
	}
	return nil
}

// EmployerRatingsGet returns nil (no error) when no snapshot exists.
func (db *DB) EmployerRatingsGet(ctx context.Context, employerID int64) (*EmployerRating, error) {
	var r EmployerRating
	var fetched sql.NullString
	err := db.conn.QueryRowContext(ctx, `
		SELECT employer_id, score_all, score_recent, period_days, reviews_count, recommend_pct, fetched_at
		FROM employer_ratings WHERE employer_id = ?`, employerID).
		Scan(&r.EmployerID, &r.ScoreAll, &r.ScoreRecent, &r.PeriodDays, &r.ReviewsCount, &r.RecommendPct, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: EmployerRatingsGet: %w", err)
	}
	if r.FetchedAt, err = timeFromNull(fetched); err != nil {
		return nil, err
	}
	return &r, nil
}
