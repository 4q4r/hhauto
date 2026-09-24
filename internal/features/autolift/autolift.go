// Package autolift implements WF1 (resume auto-lift): every PerDay window
// (default 6/day = the hh 4-hour server timer) each published+searchable
// resume of the account is lifted through /applicant/resumes/touch, gated
// by an idempotency intent per (resume, 4h window), optionally solved
// captcha challenges through the account chain with a single sanctioned
// retry, and verified by reading back the resume updated_at.
//
// hh facts: hh.ru allows lifting a resume once per 4 hours
// (https://feedback.hh.ru/knowledge-base/article/0012145); the touch
// endpoint is /applicant/resumes/touch.
package autolift

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
)

// registryName is the schedule spec registered for the engine.
const registryName = "features.autolift"

// timer4h is the hh.ru server-side limit between resume lifts.
const timer4h = 4 * time.Hour

// ErrDisabled is returned by RunAccount when the feature is off.
var ErrDisabled = errors.New("autolift: feature disabled")

// Options wires the feature to its dependencies.
type Options struct {
	Config   config.Config
	DB       *store.DB
	APIFor   featurekit.APIFor   // per-account hh client
	ChainFor featurekit.ChainFor // per-account captcha chain; nil = none
	Pacer    featurekit.Pacer    // nil = no delay
	Now      func() time.Time
	Rand     *rand.Rand // jitter source; nil → time-seeded default
	Log      *slog.Logger
}

// Feature is the WF1 implementation of features.Feature.
type Feature struct {
	opt Options
	log *slog.Logger
}

// New builds the feature.
func New(o Options) *Feature {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return &Feature{opt: o, log: o.Log}
}

// Name implements features.Feature.
func (f *Feature) Name() string { return "autolift" }

// Enabled implements features.Feature.
func (f *Feature) Enabled() bool { return f.opt.Config.Features.AutoLift.Enabled }

// LiftPayload is the typed queue payload for KindLift tasks.
type LiftPayload struct {
	AccountID string `json:"account_id"`
	Window    string `json:"window"`
}

// RegisterTasks registers the schedule spec and seeds one lift task per
// enabled account for the current 4h window (queue dedup makes repeated
// registrations idempotent). The jitter range is sampled here — once, in
// [min,max] — and baked into the seeded deadline; Spec.Jitter stays 0 so
// the engine never adds a second layer on top (Wave 3 review fix 4).
func (f *Feature) RegisterTasks(ctx context.Context, q queue.Queue, reg *scheduler.Registry) error {
	cfg := f.opt.Config
	minJ, maxJ, err := featurekit.ParseJitterRange(cfg.Features.AutoLift.Jitter)
	if err != nil {
		return fmt.Errorf("autolift: %w", err)
	}
	reg.Add(registryName, scheduler.Spec{
		TZ:     cfg.General.Timezone,
		Times:  scheduleTimes(cfg.Features.AutoLift.PerDay),
		Days:   scheduler.All,
		Jitter: 0,
	})

	window := featurekit.LiftWindow(f.opt.Now())
	jitter := featurekit.JitterDuration(minJ, maxJ, f.opt.Rand)
	for id := range cfg.Accounts {
		err := q.Enqueue(ctx, queue.Task{
			AccountID: id,
			Kind:      queue.KindLift,
			DedupKey:  "lift:" + window,
			Priority:  queue.PrioLift,
			Deadline:  f.opt.Now().Add(timer4h + jitter),
			Payload:   LiftPayload{AccountID: id, Window: window},
		})
		if err != nil {
			return fmt.Errorf("autolift: seed lift task for %s: %w", id, err)
		}
	}
	return nil
}

// HandleTask executes one queued lift task.
func (f *Feature) HandleTask(ctx context.Context, t queue.Task) error {
	return f.RunAccount(ctx, t.AccountID)
}

// scheduleTimes spreads PerDay evenly over 24h with a rounded step so
// non-divisor values leave no tail hole ("00:00","04:48",… for 5).
func scheduleTimes(perDay int) []string {
	if perDay <= 0 {
		perDay = 6
	}
	if perDay > 24 {
		perDay = 24
	}
	stepMin := (24*60 + perDay/2) / perDay
	times := make([]string, 0, perDay)
	for k := 0; k < perDay; k++ {
		m := k * stepMin
		if m >= 24*60 {
			break // never spill past midnight
		}
		times = append(times, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return times
}

// RunAccount runs one lift pass for the account's resumes.
func (f *Feature) RunAccount(ctx context.Context, accountID string) error {
	if !f.Enabled() {
		return ErrDisabled
	}
	api, err := f.opt.APIFor(accountID)
	if err != nil {
		return fmt.Errorf("autolift: api for %s: %w", accountID, err)
	}

	got, err := api.ListResumes(ctx)
	if err != nil {
		f.handleError(ctx, accountID, "list_resumes", err)
		return fmt.Errorf("autolift: list resumes for %s: %w", accountID, err)
	}
	stored := f.writeThrough(ctx, accountID, got)

	respectTimer := f.opt.Config.Features.AutoLift.Respect4hTimer == nil ||
		*f.opt.Config.Features.AutoLift.Respect4hTimer
	dryRun := f.opt.Config.General.DryRun
	day := featurekit.DayUTC(f.opt.Now())
	window := featurekit.LiftWindow(f.opt.Now())

	for _, r := range got {
		if !isLiftable(r) {
			f.incr(ctx, day, "lift.skip_unpublished")
			continue
		}
		row, ok := byHash(stored, r.HHID)
		if respectTimer && ok && !row.UpdatedAt.IsZero() && f.opt.Now().Sub(row.UpdatedAt) < timer4h {
			f.log.Debug("autolift: 4h timer not elapsed", "account", accountID, "resume", r.HHID)
			f.incr(ctx, day, "lift.skip_timer")
			continue
		}
		if dryRun {
			f.log.Info("autolift: dry-run lift", "account", accountID, "resume", r.HHID)
			f.incr(ctx, day, "lift.dry_run")
			continue
		}
		if err := f.liftOne(ctx, accountID, api, r.HHID, day, window); err != nil {
			if errors.Is(err, errAccountStopped) {
				return nil // account stopped: do not process further resumes
			}
			f.log.Warn("autolift: lift failed", "account", accountID, "resume", r.HHID, "err", err)
		}
	}
	return nil
}

// errAccountStopped signals that the account was disabled mid-run.
var errAccountStopped = errors.New("autolift: account stopped")

// liftOne runs the intent-gated touch + verify for one resume.
func (f *Feature) liftOne(ctx context.Context, accountID string, api hhapi.HHAPI, hash, day, window string) error {
	if f.opt.Pacer != nil {
		if err := f.opt.Pacer.Wait(ctx, accountID); err != nil {
			return err
		}
	}

	key := "lift:" + hash + ":" + window
	claim, err := featurekit.BeginIntent(ctx, f.opt.DB, key, featurekit.IntentPayload(LiftPayload{
		AccountID: accountID, Window: window,
	}))
	if err != nil {
		return err
	}
	switch claim.Step {
	case featurekit.IntentSkipPending, featurekit.IntentReconcileFirst:
		// A pending intent means a lift is already in flight; a done intent
		// is the hard barrier of the current window. Either way: skip.
		f.incr(ctx, day, "lift.skip_intent")
		return nil
	case featurekit.IntentProceed:
	}

	err = f.touchWithCaptchaRetry(ctx, accountID, api, hash)
	if err != nil {
		if markErr := featurekit.FinishIntent(ctx, f.opt.DB, claim, err); markErr != nil {
			f.log.Error("autolift: intent mark failed", "err", markErr)
		}
		if errors.Is(err, hhapi.ErrLoggedOut) {
			f.stopAccount(ctx, accountID, day, "touch: "+err.Error())
			return errAccountStopped
		}
		f.incr(ctx, day, "lift.error")
		return err
	}

	// Verify by reading: hh must report a fresh updated_at for the resume.
	// The baseline is the store value written before the touch.
	baseline := f.lastWritten(ctx, accountID, hash)
	freshRows, verr := api.ListResumes(ctx)
	if verr == nil {
		f.writeThrough(ctx, accountID, freshRows)
	}
	fresh := verr == nil && touchedAfter(freshRows, hash, baseline)
	if !fresh {
		if err := featurekit.AbandonIntent(ctx, f.opt.DB, claim); err != nil {
			f.log.Error("autolift: intent abandon failed", "err", err)
		}
		if _, aerr := f.opt.DB.ReviewItemsAdd(ctx, accountID, "lift_verify_failed",
			"touch accepted but resume updated_at did not change",
			fmt.Sprintf(`{"resume":%q,"window":%q}`, hash, window)); aerr != nil {
			f.log.Error("autolift: review item add failed", "err", aerr)
		}
		f.incr(ctx, day, "lift.verify_failed")
		return nil
	}

	if err := featurekit.FinishIntent(ctx, f.opt.DB, claim, nil); err != nil {
		f.log.Error("autolift: intent mark failed", "err", err)
	}
	f.incr(ctx, day, "lift.ok")
	return nil
}

// lastWritten is the pre-touch updated_at baseline for the verify step.
func (f *Feature) lastWritten(ctx context.Context, accountID, hash string) time.Time {
	rows, err := f.opt.DB.ResumesListByAccount(ctx, accountID)
	if err != nil {
		return time.Time{}
	}
	if row, ok := byHash(rows, hash); ok {
		return row.UpdatedAt
	}
	return time.Time{}
}

// touchedAfter reports whether the resume now carries a strictly fresher
// updated_at than the recorded baseline.
func touchedAfter(rows []hhapi.Resume, hash string, before time.Time) bool {
	for _, r := range rows {
		if r.HHID == hash {
			return r.UpdatedAt.After(before)
		}
	}
	return false
}

// touchWithCaptchaRetry performs the sanctioned flow: on ErrCaptcha solve
// through the account chain (when configured) and retry exactly once.
func (f *Feature) touchWithCaptchaRetry(ctx context.Context, accountID string, api hhapi.HHAPI, hash string) error {
	err := api.TouchResume(ctx, hash)
	if !errors.Is(err, hhapi.ErrCaptcha) {
		return err
	}
	day := featurekit.DayUTC(f.opt.Now())
	chain, ok := f.opt.ChainFor(accountID)
	if !ok || chain == nil {
		f.log.Warn("autolift: captcha challenge and no chain configured", "account", accountID)
		return err
	}
	if _, serr := chain.Solve(ctx, captcha.Challenge{
		Type:    captcha.TypeImage,
		PageURL: "/applicant/resumes",
	}); serr != nil {
		f.log.Warn("autolift: captcha solve failed", "account", accountID, "err", serr)
		return err
	}
	f.incr(ctx, day, "lift.captcha_retry")
	return api.TouchResume(ctx, hash) // the single sanctioned repeat
}

// isLiftable filters published+searchable resumes (shared status helper).
func isLiftable(r hhapi.Resume) bool {
	return r.Searchable && featurekit.ResumeLiftable(r.Status)
}

func byHash(rows []store.Resume, hash string) (store.Resume, bool) {
	for _, r := range rows {
		if r.HhResumeID == hash {
			return r, true
		}
	}
	return store.Resume{}, false
}

// writeThrough persists the API snapshot (the store row is the 4h-timer
// source of truth) and returns the stored rows. resumes.account_id carries
// a FOREIGN KEY to accounts(id), so the account row is ensured first (an
// empty status keeps the existing one — status ownership stays with the
// session-health feature).
func (f *Feature) writeThrough(ctx context.Context, accountID string, got []hhapi.Resume) []store.Resume {
	if err := f.opt.DB.AccountsUpsert(ctx, store.Account{ID: accountID}); err != nil {
		f.log.Warn("autolift: account ensure failed", "account", accountID, "err", err)
	}
	for _, r := range got {
		if r.HHID == "" {
			continue
		}
		row := store.Resume{
			AccountID:  accountID,
			HhResumeID: r.HHID,
			Title:      r.Title,
			Status:     r.Status,
			Raw:        string(r.Raw),
			UpdatedAt:  r.UpdatedAt,
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = f.opt.Now()
		}
		if err := f.opt.DB.ResumesUpsert(ctx, &row); err != nil {
			f.log.Warn("autolift: resume write-through failed", "account", accountID, "resume", r.HHID, "err", err)
		}
	}
	rows, err := f.opt.DB.ResumesListByAccount(ctx, accountID)
	if err != nil {
		f.log.Warn("autolift: resume store read failed", "account", accountID, "err", err)
		return nil
	}
	return rows
}

// stopAccount parks the account for human attention.
func (f *Feature) stopAccount(ctx context.Context, accountID, day, reason string) {
	if err := f.opt.DB.AccountsSetStatus(ctx, accountID, "logged_out"); err != nil {
		f.log.Error("autolift: account status set failed", "account", accountID, "err", err)
	}
	if _, err := f.opt.DB.ReviewItemsAdd(ctx, accountID, "account_logged_out", reason, `{}`); err != nil {
		f.log.Error("autolift: review item add failed", "err", err)
	}
	f.incr(ctx, day, "lift.logged_out")
	f.log.Warn("autolift: account stopped (logged out)", "account", accountID, "reason", reason)
}

// handleError records a pass-level failure; logged-out stops the account.
func (f *Feature) handleError(ctx context.Context, accountID, op string, err error) {
	day := featurekit.DayUTC(f.opt.Now())
	if errors.Is(err, hhapi.ErrLoggedOut) {
		f.stopAccount(ctx, accountID, day, op+": "+err.Error())
		return
	}
	f.incr(ctx, day, "lift.error")
}

func (f *Feature) incr(ctx context.Context, day, key string) {
	if err := f.opt.DB.MetricsIncr(ctx, key, day); err != nil {
		f.log.Warn("autolift: metric incr failed", "key", key, "err", err)
	}
}
