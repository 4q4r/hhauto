// Package setka implements the T61 post-apply Setka DM feature (disabled by
// default): after an apply, the vacancy company's people are searched, the
// targets filtered (status / headline / blacklists), classified by the AI
// (hiring-relevant for THIS vacancy or not) and the top candidates receive
// one personalized DM carrying the apply context. mode=auto sends directly
// under daily/company/person caps and the active window; mode=approve parks
// every DM in the review queue (kind=setka_dm) and `hhauto review resolve`
// sends it.
//
// The Setka transport contract is not captured yet: the client surface is
// internal/setka (fail-fast, no invented endpoints) and this package runs
// entirely on the SetkaClient interface — real against fakes, loud against
// the uncaptured wire.
package setka

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features"
	"hhauto/internal/features/kit11"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/review"
	"hhauto/internal/scheduler"
	"hhauto/internal/setka"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// FeatureName is the feature_state namespace.
const FeatureName = "setka"

// ReviewKindSetkaDM is the review-queue kind of a parked DM awaiting the
// human's apply/discard decision (the approve mode).
const ReviewKindSetkaDM = "setka_dm"

// lookback bounds how far back the scan reaches for applied applications —
// without it the first enable would process every historical apply.
const lookback = 7 * 24 * time.Hour

// scanLimit caps the applications processed per scan pass.
const scanLimit = 50

// ScanPayload is the queue payload for the periodic setka scan.
type ScanPayload struct {
	AccountID string
}

// DMPayload is the queue payload for one application's DM flow.
type DMPayload struct {
	AccountID  string
	DedupKey   string // application dedup key
	VacancyID  int64
	EmployerID int64
	ResumeID   string
	RunID      string
}

// doneMarker is the feature_state key marking a fully processed application.
func doneMarker(dedupKey string) string { return "done:" + dedupKey }

// Feature runs the setka flow for one deployment.
type Feature struct {
	Cfg config.Config
	DB  *store.DB
	AI  ai.Runner
	// ClientFor resolves the Setka transport for the account (internal/setka
	// skeleton in production wiring; fakes in tests).
	ClientFor func(ctx context.Context, accountID string) (setka.SetkaClient, error)
	// VacancyFor resolves the full vacancy dump (the CLI wires the account's
	// VacancyCard).
	VacancyFor func(ctx context.Context, accountID string, vacancyID int64) (*hhapi.Vacancy, error)
	Notify     webhook.Notifier
	Review     review.Reviewer
	Log        *slog.Logger
	Now        func() time.Time
	q          queue.Queue
}

// Name implements features.Feature.
func (f *Feature) Name() string { return "setka" }

// Enabled implements features.Feature.
func (f *Feature) Enabled() bool { return f.Cfg.Features.Setka.Enabled }

// RegisterTasks implements features.Feature: an hourly scan consumes applied
// applications (after_apply_delay elapsed, lookback bounded) and seeds one
// DM job per unprocessed application.
func (f *Feature) RegisterTasks(ctx context.Context, q queue.Queue, reg *scheduler.Registry) error {
	f.q = q
	if !f.Enabled() {
		return nil
	}
	times := make([]string, 0, 24)
	for h := range 24 {
		times = append(times, fmt.Sprintf("%02d:00", h))
	}
	reg.Add("setka.scan", scheduler.Spec{TZ: f.Cfg.General.Timezone, Times: times})
	for accountID := range f.Cfg.Accounts {
		if err := q.Enqueue(ctx, queue.Task{
			AccountID: accountID,
			Kind:      queue.KindSetkaDM,
			Priority:  queue.PrioSetka,
			DedupKey:  "setka-boot",
			Payload:   ScanPayload{AccountID: accountID},
		}); err != nil {
			return fmt.Errorf("setka: seed scan: %w", err)
		}
	}
	return nil
}

// HandleScan processes one scan task: enqueue DM jobs for due applications.
func (f *Feature) HandleScan(ctx context.Context, task queue.Task) error {
	p, ok := task.Payload.(ScanPayload)
	if !ok {
		return fmt.Errorf("setka: bad payload type %T", task.Payload)
	}
	cfg := f.Cfg.Features.Setka
	if !cfg.Enabled || f.q == nil {
		return nil
	}
	delay, err := time.ParseDuration(cfg.AfterApplyDelay)
	if err != nil {
		return fmt.Errorf("setka: after_apply_delay %q: %w", cfg.AfterApplyDelay, err)
	}
	now := f.now()
	to := now.Add(-delay) // delay fully elapsed
	from := now.Add(-lookback)
	apps, err := f.DB.ApplicationsAppliedDue(ctx, p.AccountID, from, to, scanLimit)
	if err != nil {
		return fmt.Errorf("setka: scan applications: %w", err)
	}
	for _, app := range apps {
		if marker, ok, err := f.DB.FeatureStateGet(ctx, FeatureName, p.AccountID, doneMarker(app.DedupKey)); err != nil {
			return fmt.Errorf("setka: marker read: %w", err)
		} else if ok && marker != "" {
			continue // fully processed
		}
		if err := f.q.Enqueue(ctx, queue.Task{
			AccountID: p.AccountID,
			Kind:      queue.KindSetkaDM,
			Priority:  queue.PrioSetka,
			DedupKey:  "setka:" + app.DedupKey,
			Payload: DMPayload{
				AccountID: p.AccountID, DedupKey: app.DedupKey,
				VacancyID: app.VacancyID, EmployerID: app.EmployerID,
				ResumeID: app.HhResumeID, RunID: app.RunID,
			},
		}); err != nil {
			return fmt.Errorf("setka: enqueue dm: %w", err)
		}
	}
	return nil
}

// HandleDM runs the full flow for one application (see the package docs).
func (f *Feature) HandleDM(ctx context.Context, task queue.Task) error {
	p, ok := task.Payload.(DMPayload)
	if !ok {
		return fmt.Errorf("setka: bad payload type %T", task.Payload)
	}
	cfg := f.Cfg.Features.Setka
	if !cfg.Enabled {
		return nil
	}
	if marker, ok, err := f.DB.FeatureStateGet(ctx, FeatureName, p.AccountID, doneMarker(p.DedupKey)); err != nil {
		return fmt.Errorf("setka: marker read: %w", err)
	} else if ok && marker != "" {
		return nil // idempotency fast path
	}
	client, err := f.ClientFor(ctx, p.AccountID)
	if err != nil {
		return err // loud: the factory's fail-fast (uncaptured endpoints)
	}
	vacancy, err := f.VacancyFor(ctx, p.AccountID, p.VacancyID)
	if err != nil {
		return fmt.Errorf("setka: vacancy %d: %w", p.VacancyID, err) // transient: redelivery
	}
	if vacancy == nil {
		return f.park(ctx, p.AccountID, "setka_vacancy_gone",
			fmt.Sprintf("setka: вакансия %d недоступна, ЛС не готовится", p.VacancyID),
			map[string]any{"vacancy_id": p.VacancyID, "application": p.DedupKey}, p, true)
	}
	// Human-hours gate: outside the window the job comes back with the
	// next scan (no done marker, nothing sent).
	if !kit11.InActiveWindow(f.now().Local(), cfg.ActiveWindow) {
		f.log().Info("setka: вне окна отправки — перенос на следующий проход", "application", p.DedupKey)
		return nil
	}

	people, err := client.SearchPeople(ctx, vacancy.EmployerName)
	if err != nil {
		return fmt.Errorf("setka: search %q: %w", vacancy.EmployerName, err)
	}
	candidates, err := f.filterAndGate(ctx, client, &cfg, vacancy.EmployerName, people)
	if err != nil {
		return err
	}
	scored, err := f.classifyAll(ctx, p, &cfg, vacancy, candidates)
	if err != nil {
		return err
	}
	if err := f.messageTop(ctx, client, &cfg, p, vacancy, scored); err != nil {
		return err
	}
	if err := f.DB.FeatureStateSet(ctx, FeatureName, p.AccountID, doneMarker(p.DedupKey), "1"); err != nil {
		return fmt.Errorf("setka: marker write: %w", err)
	}
	return nil
}

func (f *Feature) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

func (f *Feature) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// park records one terminal flow outcome in the review queue (deduped) and
// optionally marks the application done.
func (f *Feature) park(ctx context.Context, accountID, kind, reason string, payload any, p DMPayload, markDone bool) error {
	b, err := jsonMarshal(payload)
	if err != nil {
		return fmt.Errorf("setka: payload: %w", err)
	}
	if f.Review != nil {
		if _, err := f.Review.Add(ctx, accountID, kind, reason, string(b)); err != nil {
			return fmt.Errorf("setka: review add: %w", err)
		}
	} else if _, err := f.DB.ReviewItemsAdd(ctx, accountID, kind, reason, string(b)); err != nil {
		return fmt.Errorf("setka: review add: %w", err)
	}
	if markDone {
		if err := f.DB.FeatureStateSet(ctx, FeatureName, accountID, doneMarker(p.DedupKey), "1"); err != nil {
			return fmt.Errorf("setka: marker write: %w", err)
		}
	}
	return nil
}

// Compile-time conformance with the feature-module contract.
var _ features.Feature = (*Feature)(nil)
