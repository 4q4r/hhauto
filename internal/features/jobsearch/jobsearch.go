// Package jobsearch keeps the account's «Ищу работу» status alive
// (T113 §№5): once per 24h per account the daemon maintenance cycle calls
// Tick, which flips the status to looking_for_offers through
// hhapi.SetJobSearchStatus. DONOR-DERIVED contract (hh-auto-responses
// main.go:2092–2129) — the endpoint has never been captured live, so the
// [features.jobsearch] flag ships DISABLED and enabling it is the owner's
// decision after a live probe. Idempotence on the server side is unproven
// donor folklore; this package only guarantees client-level retry safety.
package jobsearch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// daily is the donor cadence (the status flip runs once per 24h).
const daily = 24 * time.Hour

const (
	featureName = "jobsearch"
	markerKey   = "last_set"
)

// Options wires the tick to its dependencies.
type Options struct {
	// Enabled is the [features.jobsearch] flag. False = dead code.
	Enabled bool
	// Now is the flow clock (nil → time.Now; injectable for tests).
	Now func() time.Time
	// DB persists the per-account daily marker (feature_state).
	DB *store.DB
	// APIFor resolves the per-account hh client.
	APIFor func(accountID string) (hhapi.HHAPI, error)
	// Accounts lists the account ids to serve (sorted by the caller).
	Accounts []string
	// Log receives per-account failures; nil → slog.Default().
	Log *slog.Logger
}

// Tick is the daily maintenance point: for every account whose
// last_set marker is older than 24h (or absent) it posts the status flip
// and stamps the marker on success only — a failed POST retries at the
// next maintenance run. A disabled feature is a no-op. Per-account
// failures are logged, never fail the whole tick.
func Tick(ctx context.Context, o Options) error {
	if !o.Enabled {
		return nil
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	for _, id := range o.Accounts {
		due, err := dueFor(ctx, o.DB, id, now())
		if err != nil {
			log.Warn("jobsearch: маркер ежедневного статуса недоступен", "account", id, "err", err)
			continue
		}
		if !due {
			continue
		}
		api, err := o.APIFor(id)
		if err != nil {
			log.Warn("jobsearch: клиент недоступен", "account", id, "err", err)
			continue
		}
		if err := api.SetJobSearchStatus(ctx); err != nil {
			// No stamp: captcha, logout and transport errors all retry
			// at the next maintenance run.
			log.Warn("jobsearch: статус «Ищу работу» не установлен — повтор при следующем обслуживании", "account", id, "err", err)
			continue
		}
		if err := o.DB.FeatureStateSet(ctx, featureName, id, markerKey, now().Format(time.RFC3339)); err != nil {
			log.Warn("jobsearch: маркер не сохранён — повтор при следующем обслуживании", "account", id, "err", err)
		}
	}
	return nil
}

// dueFor reports whether the account's daily point has come due: no
// marker yet, or the marker is 24h+ old.
func dueFor(ctx context.Context, db *store.DB, accountID string, now time.Time) (bool, error) {
	v, ok, err := db.FeatureStateGet(ctx, featureName, accountID, markerKey)
	if err != nil {
		return false, fmt.Errorf("jobsearch: feature_state: %w", err)
	}
	if !ok {
		return true, nil
	}
	last, err := time.Parse(time.RFC3339, v)
	if err != nil {
		// An unreadable marker is treated as absent — the flip re-runs.
		return true, nil
	}
	return now.Sub(last) >= daily, nil
}
