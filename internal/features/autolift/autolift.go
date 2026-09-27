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
	"strconv"
	"sync"
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

	// Queue is retained by RegisterTasks (chatops pattern): server_timer
	// mode re-arms itself by enqueueing the next lift at the plan moment.
	Queue queue.Queue
	// AfterFunc is the wake-up timer seam (tests capture the arm); nil →
	// time.AfterFunc.
	AfterFunc func(d time.Duration, fn func()) *time.Timer
}

// Feature is the WF1 implementation of features.Feature.
type Feature struct {
	opt   Options
	log   *slog.Logger
	q     queue.Queue
	after func(d time.Duration, fn func()) *time.Timer

	// planMu guards the server_timer state: the local lift plan (account →
	// resume hash → next lift moment) and the pending arm timer per account.
	planMu    sync.Mutex
	plans     map[string]map[string]time.Time
	armTimers map[string]*time.Timer
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
	after := o.AfterFunc
	if after == nil {
		after = time.AfterFunc
	}
	return &Feature{
		opt: o, log: o.Log, q: o.Queue, after: after,
		plans:     map[string]map[string]time.Time{},
		armTimers: map[string]*time.Timer{},
	}
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
	f.q = q
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

// RunAccount runs one lift pass for the account's resumes. In server_timer
// mode (T36) the first pass computes a local lift plan from the shard
// (nextTouchAt, or updated_at + 4h when empty) and later passes lift by the
// plan, re-counting 4h + jitter locally after each lift — the shard is not
// re-fetched between lifts (owner ruling); the plan is recomputed only on
// process start or after a logout/relogin invalidates it.
func (f *Feature) RunAccount(ctx context.Context, accountID string) error {
	if !f.Enabled() {
		return ErrDisabled
	}
	if plan := f.currentPlan(accountID); plan != nil {
		return f.runPlanned(ctx, accountID, plan)
	}
	return f.runDiscover(ctx, accountID)
}

// runDiscover lists the resumes, builds the lift plan from the snapshot and
// runs one server-timer-gated pass (start pass and post-relogin recompute).
func (f *Feature) runDiscover(ctx context.Context, accountID string) error {
	api, err := f.opt.APIFor(accountID)
	if err != nil {
		return fmt.Errorf("autolift: api for %s: %w", accountID, err)
	}

	got, err := api.ListResumes(ctx)
	if err != nil {
		f.handleError(ctx, accountID, "list_resumes", err)
		return fmt.Errorf("autolift: list resumes for %s: %w", accountID, err)
	}
	f.writeThrough(ctx, accountID, got)
	exclude := f.cloneExcludes(ctx, accountID)

	dryRun := f.opt.Config.General.DryRun
	day := featurekit.DayUTC(f.opt.Now())
	window := featurekit.LiftWindow(f.opt.Now())

	// T36: gating is by nextTouchAt (empty → updated_at + 4h); the plan is
	// built from the same snapshot.
	lifted := map[string]bool{}

	for _, r := range got {
		if !isLiftable(r) {
			f.incr(ctx, day, "lift.skip_unpublished")
			continue
		}
		if exclude[r.HHID] {
			f.log.Debug("autolift: clone excluded by autoclone.autolift=false", "account", accountID, "resume", r.HHID)
			f.incr(ctx, day, "lift.skip_clone")
			continue
		}
		if f.gateServerTimer(ctx, accountID, day, r, dryRun) {
			continue
		}
		if dryRun {
			f.log.Info("autolift: dry-run lift", "account", accountID, "resume", r.HHID)
			f.incr(ctx, day, "lift.dry_run")
			continue
		}
		if err := f.liftOne(ctx, accountID, api, r.HHID, day, window); err != nil {
			if errors.Is(err, errAccountStopped) {
				f.dropPlan(accountID) // relogin → recompute from the server
				return nil            // account stopped: do not process further resumes
			}
			f.log.Warn("autolift: lift failed", "account", accountID, "resume", r.HHID, "err", err)
			continue
		}
		lifted[r.HHID] = true
	}

	if !dryRun {
		f.setPlan(accountID, f.buildPlan(got, exclude, lifted))
		f.arm(accountID)
	}
	return nil
}

// cloneExcludes resolves the tracked clone pool excluded from lifting by
// [features.autoclone] autolift=false (T26: clones lift like base resumes
// unless the gate is off). A pool lookup failure lifts everything.
func (f *Feature) cloneExcludes(ctx context.Context, accountID string) map[string]bool {
	exclude := map[string]bool{}
	if f.opt.Config.Features.AutoClone.Autolift == nil || *f.opt.Config.Features.AutoClone.Autolift {
		return exclude
	}
	clones, err := f.opt.DB.ResumeClonesListByAccount(ctx, accountID)
	if err != nil {
		f.log.Warn("autolift: clone pool lookup failed; lifting everything", "account", accountID, "err", err)
		return exclude
	}
	for _, c := range clones {
		exclude[c.CloneHash] = true
	}
	return exclude
}

// gateServerTimer applies the T36 server timer for one resume: dry-run
// logs the planned lift moment, an unelapsed timer is counted and logged.
// true = hold the lift back.
func (f *Feature) gateServerTimer(ctx context.Context, accountID, day string, r hhapi.Resume, dryRun bool) bool {
	at := r.NextTouchAt
	if at.IsZero() {
		at = r.UpdatedAt.Add(timer4h)
	}
	if dryRun {
		f.log.Info("autolift: dry-run planned lift", "account", accountID, "resume", r.HHID, "at", at.UTC().Format(time.RFC3339))
		f.incr(ctx, day, "lift.dry_run")
		return true
	}
	if at.After(f.opt.Now()) {
		f.log.Debug("autolift: server timer not elapsed", "account", accountID, "resume", r.HHID, "at", at.UTC().Format(time.RFC3339))
		f.incr(ctx, day, "lift.skip_timer")
		return true
	}
	return false
}

// runPlanned executes one local plan pass: every resume whose plan moment
// has elapsed is lifted (no shard read); lifted resumes are re-counted
// locally as now + 4h + jitter and the next wake is armed.
func (f *Feature) runPlanned(ctx context.Context, accountID string, plan map[string]time.Time) error {
	api, err := f.opt.APIFor(accountID)
	if err != nil {
		return fmt.Errorf("autolift: api for %s: %w", accountID, err)
	}
	nowT := f.opt.Now()
	dryRun := f.opt.Config.General.DryRun
	day := featurekit.DayUTC(nowT)
	window := featurekit.LiftWindow(nowT)

	for hash, at := range plan {
		if dryRun {
			f.log.Info("autolift: dry-run planned lift", "account", accountID, "resume", hash, "at", at.UTC().Format(time.RFC3339))
			f.incr(ctx, day, "lift.dry_run")
			continue
		}
		if at.After(nowT) {
			continue
		}
		if err := f.liftOne(ctx, accountID, api, hash, day, window); err != nil {
			if errors.Is(err, errAccountStopped) {
				f.dropPlan(accountID) // relogin → recompute from the server
				return nil
			}
			f.log.Warn("autolift: lift failed", "account", accountID, "resume", hash, "err", err)
			continue
		}
	}
	if dryRun {
		return nil // no plan mutation, no arm: dry-run is read-only
	}
	for hash, at := range plan {
		if !at.After(nowT) {
			plan[hash] = nowT.Add(timer4h).Add(f.sampleJitter())
		}
	}
	f.setPlan(accountID, plan)
	f.arm(accountID)
	return nil
}

// buildPlan computes the local lift plan from the shard snapshot: the
// server nextTouchAt when present, updated_at + 4h when empty (both + one
// jitter sample); resumes lifted in this pass are re-counted from now.
func (f *Feature) buildPlan(got []hhapi.Resume, exclude, lifted map[string]bool) map[string]time.Time {
	nowT := f.opt.Now()
	plan := make(map[string]time.Time, len(got))
	for _, r := range got {
		if r.HHID == "" || !isLiftable(r) || exclude[r.HHID] {
			continue
		}
		switch {
		case lifted[r.HHID]:
			plan[r.HHID] = nowT.Add(timer4h).Add(f.sampleJitter())
		case r.NextTouchAt.IsZero():
			plan[r.HHID] = r.UpdatedAt.Add(timer4h)
		default:
			plan[r.HHID] = r.NextTouchAt.Add(f.sampleJitter())
		}
	}
	return plan
}

// sampleJitter draws one jitter sample from the configured range (0 when
// unset) — the antibot delay applied after the plan moment.
func (f *Feature) sampleJitter() time.Duration {
	minJ, maxJ, err := featurekit.ParseJitterRange(f.opt.Config.Features.AutoLift.Jitter)
	if err != nil {
		return 0
	}
	return featurekit.JitterDuration(minJ, maxJ, f.opt.Rand)
}

// arm schedules the next lift task at the earliest plan moment. The queue
// is transient and Dequeue does not wait for Deadline, so the wake-up is a
// one-shot timer that enqueues at fire time; re-arming replaces the pending
// timer. Restart recovery is the boot seed + fresh plan computation.
func (f *Feature) arm(accountID string) {
	if f.q == nil {
		return
	}
	nowT := f.opt.Now()
	wake := time.Time{}
	for _, at := range f.currentPlan(accountID) {
		if at.After(nowT) && (wake.IsZero() || at.Before(wake)) {
			wake = at
		}
	}
	if wake.IsZero() {
		return
	}
	delay := wake.Sub(nowT)
	f.planMu.Lock()
	defer f.planMu.Unlock()
	if prev := f.armTimers[accountID]; prev != nil {
		prev.Stop()
	}
	f.armTimers[accountID] = f.after(delay, func() {
		f.planMu.Lock()
		delete(f.armTimers, accountID)
		f.planMu.Unlock()
		err := f.q.Enqueue(context.Background(), queue.Task{
			AccountID: accountID,
			Kind:      queue.KindLift,
			DedupKey:  "lift:timer:" + strconv.FormatInt(wake.Unix(), 10),
			Priority:  queue.PrioLift,
			Deadline:  wake,
			Payload:   LiftPayload{AccountID: accountID, Window: featurekit.LiftWindow(wake)},
		})
		if err != nil {
			f.log.Debug("autolift: timer re-arm dropped", "account", accountID, "err", err)
		}
	})
}

func (f *Feature) currentPlan(accountID string) map[string]time.Time {
	f.planMu.Lock()
	defer f.planMu.Unlock()
	return f.plans[accountID]
}

func (f *Feature) setPlan(accountID string, plan map[string]time.Time) {
	f.planMu.Lock()
	defer f.planMu.Unlock()
	f.plans[accountID] = plan
}

func (f *Feature) dropPlan(accountID string) {
	f.planMu.Lock()
	defer f.planMu.Unlock()
	delete(f.plans, accountID)
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

// writeThrough persists the API snapshot (the store row is the verify
// baseline) and returns the stored rows. resumes.account_id carries
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
