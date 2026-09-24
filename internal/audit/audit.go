// Package audit centralizes the metric-key contract and the daily funnel
// summary for `hhauto status`. Counters live in the store's metrics table
// as (key, day) → value; every writer (engine, features) goes through the
// constants here so operator dashboards can rely on stable key names.
package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"hhauto/internal/store"
)

// Stable metric keys (grep- and dashboard-facing contract).
const (
	KeyApplicationsSent = "applications_sent"
	KeyHTTP429          = "http_429"
	KeyCaptchasSolved   = "captchas_solved"
	KeyAITokensIn       = "ai_tokens_in"
	KeyAITokensOut      = "ai_tokens_out"
	KeyPingsSent        = "pings_sent"
	KeyEmailsSent       = "emails_sent"
	KeyTasksFailed      = "tasks_failed"

	// Funnel inputs written by the apply pipeline (feature + engine side).
	KeyVacanciesScanned = "vacancies_scanned"
	KeyVacanciesPassed  = "vacancies_passed"

	// KeyReviewSLABreach counts SLA-breaching open review items per day
	// (written by the review package's SLA scan).
	KeyReviewSLABreach = "review_sla_breach"

	// KeySkippedPrefix marks per-stage skips: "skipped_<stage>" where the
	// stage names the funnel gate ("skipped_low_score", "skipped_agency",
	// "skipped_dry_run", ...). Snapshot strips the prefix into
	// Funnel.SkippedByStage.
	KeySkippedPrefix = "skipped_"
)

// Day returns today's metrics day key (UTC — the store stores all
// timestamps as UTC RFC3339 and slices days by prefix).
func Day() string { return DayAt(time.Now()) }

// DayAt formats t's UTC date as the metrics day key ("YYYY-MM-DD").
func DayAt(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Incr bumps a metric counter for today (UTC).
func Incr(ctx context.Context, db *store.DB, key string) error {
	return IncrDay(ctx, db, key, Day())
}

// IncrDay bumps a metric counter for an explicit day.
func IncrDay(ctx context.Context, db *store.DB, key, day string) error {
	if db == nil {
		return fmt.Errorf("audit: nil store")
	}
	if key == "" || day == "" {
		return fmt.Errorf("audit: empty key or day")
	}
	return db.MetricsIncr(ctx, key, day)
}

// Funnel is the daily pipeline summary for `hhauto status`.
//
// Data source note: the store contract exposes no read DAO aggregating
// application statuses or vacancies_seen rows, so the funnel is computed
// from the metrics counters that the engine and features write through
// this package (applications_sent, vacancies_scanned, vacancies_passed,
// skipped_*). Those counters are written exactly once per pipeline event,
// which makes the funnel complete as long as producers use audit.Incr.
type Funnel struct {
	Scanned        int
	Passed         int
	Applied        int
	SkippedByStage map[string]int // "skipped_<stage>" minus prefix → count
}

// Snapshot summarizes one metrics day ("YYYY-MM-DD").
func Snapshot(ctx context.Context, db *store.DB, day string) (Funnel, error) {
	if db == nil {
		return Funnel{}, fmt.Errorf("audit: nil store")
	}
	m, err := db.MetricsSnapshot(ctx, day)
	if err != nil {
		return Funnel{}, fmt.Errorf("audit: snapshot %s: %w", day, err)
	}
	f := Funnel{SkippedByStage: map[string]int{}}
	for k, v := range m {
		switch {
		case k == KeyVacanciesScanned:
			f.Scanned = v
		case k == KeyVacanciesPassed:
			f.Passed = v
		case k == KeyApplicationsSent:
			f.Applied = v
		case strings.HasPrefix(k, KeySkippedPrefix):
			f.SkippedByStage[strings.TrimPrefix(k, KeySkippedPrefix)] = v
		}
	}
	return f, nil
}
