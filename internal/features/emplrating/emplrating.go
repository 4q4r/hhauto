// Package emplrating implements the WF2-F4 employer-rating filter: a cached
// read of the hh employer-reviews widget plus the min-rating policy
// (all-time vs recent window) with the no_reviews_policy routes.
package emplrating

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// defaultCacheHours is used when RunFilters.RatingCacheHours is unset.
const defaultCacheHours = 24

// Filter decides whether an employer passes the rating policy for a run.
// It is safe for concurrent use; the cache is the employer_ratings table.
type Filter struct {
	db  *store.DB
	api hhapi.HHAPI
	log *slog.Logger

	// now is injectable for cache-TTL tests.
	now func() time.Time
}

// New builds the filter. A nil logger falls back to slog.Default().
func New(db *store.DB, api hhapi.HHAPI, log *slog.Logger) *Filter {
	if log == nil {
		log = slog.Default()
	}
	return &Filter{db: db, api: api, log: log, now: time.Now}
}

// Pass applies the employer-rating policy from flt and reports whether the
// vacancy may proceed, with a short machine-readable reason:
//
//	rating_not_configured — no min_employer_rating set: pass, no API call;
//	rating_ok             — cached/fetched rating meets the threshold;
//	rating_low_all        — all-time score below threshold (period=all);
//	rating_low_recent     — recent-window score below threshold (period=N days);
//	no_reviews_allowed    — employer has no reviews, policy=allow (default);
//	no_reviews            — employer has no reviews, policy=skip;
//	no_reviews_human      — employer has no reviews, policy=human (a review
//	                        item is persisted and the vacancy is skipped);
//	rating_fetch_error    — the widget fetch failed: fail-open with a WARN
//	                        (a flaky widget must not block every apply;
//	                        the decision is logged for audit).
func (f *Filter) Pass(ctx context.Context, accountID string, employerID int64, flt config.RunFilters) (bool, string) {
	if flt.MinEmployerRating <= 0 {
		return true, "rating_not_configured"
	}

	r, reason := f.rating(ctx, employerID, flt.RatingCacheHours)
	if r == nil {
		// rating_fetch_error: fail-open (reason carries the outcome).
		return reason == "rating_fetch_error", reason
	}

	if r.ReviewsCount == 0 {
		switch flt.NoReviewsPolicy {
		case "skip":
			return false, "no_reviews"
		case "human":
			if _, err := f.db.ReviewItemsAdd(ctx, accountID, "emplrating_no_reviews",
				"employer has no reviews and no_reviews_policy=human",
				`{"employer_id":`+strconv.FormatInt(employerID, 10)+`}`); err != nil {
				f.log.Error("emplrating: review item add failed", "err", err)
			}
			return false, "no_reviews_human"
		default: // "allow" and unset
			return true, "no_reviews_allowed"
		}
	}

	score := r.ScoreAll
	what := "all"
	if days, ok := recentDays(flt.RatingPeriod); ok {
		score = r.ScoreRecent
		what = "recent"
		_ = days // the widget exposes one recent window (last3Months); N selects it
	}
	if score < flt.MinEmployerRating {
		return false, "rating_low_" + what
	}
	return true, "rating_ok"
}

// rating returns the fresh-enough snapshot, fetching through the API on a
// miss or stale cache and persisting it (write-through).
func (f *Filter) rating(ctx context.Context, employerID int64, cacheHours int) (*hhapi.EmployerRating, string) {
	ttl := time.Duration(cacheHours) * time.Hour
	if ttl <= 0 {
		ttl = defaultCacheHours * time.Hour
	}
	if cached, err := f.db.EmployerRatingsGet(ctx, employerID); err != nil {
		f.log.Warn("emplrating: cache read failed; fetching", "employer", employerID, "err", err)
	} else if cached != nil && !cached.FetchedAt.IsZero() && f.now().Sub(cached.FetchedAt) < ttl {
		return &hhapi.EmployerRating{
			EmployerID:   cached.EmployerID,
			ScoreAll:     cached.ScoreAll,
			ScoreRecent:  cached.ScoreRecent,
			ReviewsCount: cached.ReviewsCount,
			RecommendPct: cached.RecommendPct,
			FetchedAt:    cached.FetchedAt,
		}, "cache"
	}

	r, err := f.api.EmployerRating(ctx, employerID)
	if err != nil || r == nil {
		f.log.Warn("emplrating: widget fetch failed", "employer", employerID, "err", err)
		_ = f.db.MetricsIncr(ctx, "emplrating.fetch_error", featurekit.DayUTC(f.now()))
		return nil, "rating_fetch_error"
	}
	row := store.EmployerRating{
		EmployerID:   r.EmployerID,
		ScoreAll:     r.ScoreAll,
		ScoreRecent:  r.ScoreRecent,
		ReviewsCount: r.ReviewsCount,
		RecommendPct: r.RecommendPct,
		FetchedAt:    r.FetchedAt,
	}
	if row.FetchedAt.IsZero() {
		row.FetchedAt = f.now()
	}
	if err := f.db.EmployerRatingsPut(ctx, row); err != nil {
		f.log.Warn("emplrating: cache write failed", "employer", employerID, "err", err)
	}
	return r, "fetched"
}

// recentDays parses a RatingPeriod of "N" (days). "all"/"" selects the
// all-time score; any other non-numeric value is treated as "all".
func recentDays(period string) (int, bool) {
	p := strings.TrimSpace(period)
	if p == "" || strings.EqualFold(p, "all") {
		return 0, false
	}
	days, err := strconv.Atoi(p)
	if err != nil || days <= 0 {
		return 0, false
	}
	return days, true
}
