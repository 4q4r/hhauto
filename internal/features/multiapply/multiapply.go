// Package multiapply implements WF2 (discovery → filters → apply): it walks
// the configured run profiles, collects vacancies from the suitable-vacancies
// or search source, runs the filter cascade (F2 exclude words, F3 blacklist/
// whitelist with stage bypass, F4 employer rating, F5 dedup, F6 AI relevance,
// F6.5 apply-popup feasibility, F7 daily quotas) and applies (F8) with an
// AI cover letter, an idempotency intent per (account, vacancy), a captcha
// chain retry and an AMBIGUOUS reconcile through the negotiations list.
package multiapply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/console"
	"hhauto/internal/features/autoclone"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/queue"
	"hhauto/internal/review"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// Pipeline limits and policies.
const (
	// maxPages caps source pagination (contract: constant, not configurable).
	maxPages = 5
	// perEmployerDailyCap is the per-employer daily apply limit.
	perEmployerDailyCap = 2
	// defaultMinMatch is the AI relevance threshold when unset.
	defaultMinMatch = 60
	// defaultAccountCap backs an unset account daily_apply_cap.
	defaultAccountCap = 200
	// feature namespaces feature_state keys.
	feature = "multiapply"
)

// ErrDisabled reports a disabled feature or run profile.
var ErrDisabled = errors.New("multiapply: run disabled")

// Options wires the feature to its dependencies.
type Options struct {
	Config     config.Config
	DB         *store.DB
	APIFor     featurekit.APIFor
	ChainFor   featurekit.ChainFor
	Runner     ai.Runner // nil = AI tasks unavailable (ai_filter degrades to off)
	ProfileFor func(ctx context.Context, accountID string) (profile.Profile, error)
	Pacer      featurekit.Pacer
	Notifier   webhook.Notifier
	// Review is the review-queue fan-out (dedup + needs_human webhook).
	// nil = storage-only adds (unwired tests).
	Review review.Reviewer
	// Stopped reports the kill switch. nil = never stopped (unwired tests).
	// Checked between vacancies inside one run.
	Stopped func(ctx context.Context) bool
	Now     func() time.Time
	Log     *slog.Logger

	// T26 autoclone test/ops knobs (production values derive from config):
	// ClonePollInterval is the not_finished poll step (default 20s,
	// GET-only); CloneFinishTimeout overrides the config
	// finish_timeout_min-derived budget when positive.
	ClonePollInterval  time.Duration
	CloneFinishTimeout time.Duration

	// T29: Improver runs the per-field AI improvement of a fresh clone
	// (internal/features/autoclone). nil = improve disabled — the clone
	// runs unimproved.
	Improver AutoCloneImprover

	// T69: Console renders the daemon's console surface (applied/skip/
	// error/letter/verdict lines and the end-of-pass digest). nil = silent
	// (unwired tests, disabled console).
	Console *console.Surface

	// T80: solve-budget override for the vacancy-test solve (tests).
	// Zero = derive from [features.multiapply].testsolve_timeout_sec,
	// then the 120s default.
	TestSolveTimeout time.Duration
}

// AutoCloneImprover is the improve seam (implemented by the autoclone
// feature package); a fatal error keeps the original resume (the
// clone_original_action rides on clone+improve success).
type AutoCloneImprover interface {
	ImproveClone(ctx context.Context, accountID, parentHash, cloneHash string) (*autoclone.DiffArtifact, error)
}

// RunPayload is the typed queue payload for KindApply run tasks.
type RunPayload struct {
	RunID string `json:"run_id"`
}

// Feature is the WF2 implementation of features.Feature.
type Feature struct {
	opt     Options
	log     *slog.Logger
	q       queue.Queue
	console *console.Surface

	// T77: vacancies whose apply failed with a client-side validation error
	// (hh 400/422 — permanent). Keyed account:vacancy, entries expire after
	// one apply cadence. Guarded: passes for different accounts run
	// concurrently.
	terminalMu sync.Mutex
	terminal   map[string]time.Time
}

// New builds the feature. A nil logger falls back to slog.Default().
func New(o Options) *Feature {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Feature{opt: o, log: o.Log, console: o.Console, terminal: map[string]time.Time{}}
}

// Name implements features.Feature.
func (f *Feature) Name() string { return "multiapply" }

// Enabled implements features.Feature.
func (f *Feature) Enabled() bool { return f.opt.Config.Features.MultiApply.Enabled }

// runRotationKey is the feature_state key holding the run id seeded FIRST
// in the previous registration — the round-robin head (T68).
const runRotationKey = "run_seed_head"

// RegisterTasks registers per-run window specs and seeds one run task per
// enabled profile (queue dedup keeps repeated registrations idempotent).
// T68 fairness: map iteration randomizes the seeding order on every process
// start, and a --once pass that spends its whole budget on the first run
// task would then process a random single profile while the rest starve.
// Each account's runs are seeded in a sorted order rotated past the previous
// head, so consecutive invocations walk the enabled profiles round-robin.
func (f *Feature) RegisterTasks(ctx context.Context, q queue.Queue, reg *scheduler.Registry) error {
	f.q = q
	perAccount := make(map[string][]string, len(f.opt.Config.Runs))
	for id, run := range f.opt.Config.Runs {
		if !run.Enabled {
			continue
		}
		perAccount[run.Account] = append(perAccount[run.Account], id)
	}
	accounts := make([]string, 0, len(perAccount))
	for id := range perAccount {
		accounts = append(accounts, id)
	}
	sort.Strings(accounts)
	for _, accountID := range accounts {
		ids := perAccount[accountID]
		sort.Strings(ids)
		for _, id := range f.rotateRunIDs(ctx, accountID, ids) {
			run := f.opt.Config.Runs[id]
			if len(run.Windows) > 0 {
				spec, err := f.windowSpec(id, run)
				if err != nil {
					return err
				}
				reg.Add("runs."+id, spec)
			}
			if err := q.Enqueue(ctx, queue.Task{
				AccountID: run.Account,
				Kind:      queue.KindApply,
				RunID:     id,
				DedupKey:  "multiapply:" + id,
				Priority:  queue.PrioApply,
				Payload:   RunPayload{RunID: id},
			}); err != nil {
				return fmt.Errorf("multiapply: seed run %s: %w", id, err)
			}
		}
	}
	return nil
}

// rotateRunIDs rotates the sorted run ids so seeding starts after the
// previously seeded head and persists the new head. On a lookup/persist
// failure it degrades to the plain sorted order — deterministic first-run,
// never random.
func (f *Feature) rotateRunIDs(ctx context.Context, accountID string, ids []string) []string {
	if len(ids) <= 1 {
		return ids
	}
	start := 0
	if last, ok, err := f.opt.DB.FeatureStateGet(ctx, feature, accountID, runRotationKey); err != nil {
		f.log.Warn("multiapply: run rotation lookup failed; seeding sorted", "err", err)
	} else if ok {
		if i := sort.SearchStrings(ids, last); i < len(ids) && ids[i] == last {
			start = (i + 1) % len(ids)
		}
	}
	out := append(append([]string(nil), ids[start:]...), ids[:start]...)
	if err := f.opt.DB.FeatureStateSet(ctx, feature, accountID, runRotationKey, out[0]); err != nil {
		f.log.Warn("multiapply: run rotation persist failed", "err", err)
	}
	return out
}

// windowSpec merges the run's windows into one scheduler spec (times and
// days union; the first window's TZ wins, falling back to general.timezone).
func (f *Feature) windowSpec(runID string, run config.Run) (scheduler.Spec, error) {
	tz := f.opt.Config.General.Timezone
	var times []string
	var days []string
	for i, w := range run.Windows {
		if i == 0 && w.TZ != "" {
			tz = w.TZ
		}
		times = append(times, w.Time...)
		days = append(days, w.Days...)
	}
	spec, err := scheduler.Parse(tz, times, days)
	if err != nil {
		return spec, fmt.Errorf("multiapply: runs.%s.windows: %w", runID, err)
	}
	return spec, nil
}

// HandleTask executes one queued run task.
func (f *Feature) HandleTask(ctx context.Context, t queue.Task) error {
	runID := t.RunID
	if p, ok := t.Payload.(RunPayload); ok && p.RunID != "" {
		runID = p.RunID
	}
	return f.RunProfile(ctx, runID)
}

// dayUTC is the feature clock's UTC day.
func (f *Feature) dayUTC() string { return featurekit.DayUTC(f.opt.Now()) }

func (f *Feature) incr(ctx context.Context, key string) {
	if err := f.opt.DB.MetricsIncr(ctx, key, f.dayUTC()); err != nil {
		f.log.Warn("multiapply: metric incr failed", "key", key, "err", err)
	}
}

// review records a review item via the wired reviewer (dedup + needs_human
// webhook fan-out); unwired (tests) it degrades to storage-only adds.
func (f *Feature) review(ctx context.Context, accountID, kind, reason, payload string) {
	if f.opt.Review != nil {
		if _, err := f.opt.Review.Add(ctx, accountID, kind, reason, payload); err != nil {
			f.log.Error("multiapply: review item add failed", "kind", kind, "err", err)
		}
		return
	}
	if _, err := f.opt.DB.ReviewItemsAdd(ctx, accountID, kind, reason, payload); err != nil {
		f.log.Error("multiapply: review item add failed", "kind", kind, "err", err)
	}
}

// stopAccount parks the account for human attention (session expired).
func (f *Feature) stopAccount(ctx context.Context, accountID, reason string) {
	if err := f.opt.DB.AccountsSetStatus(ctx, accountID, "logged_out"); err != nil {
		f.log.Error("multiapply: account status set failed", "account", accountID, "err", err)
	}
	f.review(ctx, accountID, "account_logged_out", reason, `{}`)
	f.incr(ctx, "apply.logged_out")
	f.log.Warn("multiapply: account stopped (logged out)", "account", accountID)
}

// RunProfile executes one run profile end-to-end.
func (f *Feature) RunProfile(ctx context.Context, runID string) error {
	cfg := f.opt.Config
	if !f.Enabled() {
		return ErrDisabled
	}
	run, ok := cfg.Runs[runID]
	if !ok {
		return fmt.Errorf("multiapply: unknown run %q", runID)
	}
	if !run.Enabled {
		return ErrDisabled
	}
	accountID := run.Account
	api, err := f.opt.APIFor(accountID)
	if err != nil {
		return fmt.Errorf("multiapply: api for %s: %w", accountID, err)
	}
	resumeHash, err := f.resolveResumeHash(ctx, accountID, api, run)
	if err != nil {
		return fmt.Errorf("multiapply: resolve resume for %s: %w", runID, err)
	}
	prof, err := f.loadProfile(ctx, accountID, api)
	if err != nil {
		return fmt.Errorf("multiapply: profile for %s: %w", accountID, err)
	}
	return f.executeRun(ctx, runID, run, accountID, api, prof, resumeHash, nil, 0)
}

// stopLoggedOut marks a profile stop that must not be rescheduled.
const stopLoggedOut = "account_logged_out"

// prioritizeOnline stably moves employer-online vacancies to the front.
func prioritizeOnline(in []hhapi.Vacancy) []hhapi.Vacancy {
	out := make([]hhapi.Vacancy, 0, len(in))
	for _, v := range in {
		if v.EmployerOnline {
			out = append(out, v)
		}
	}
	for _, v := range in {
		if !v.EmployerOnline {
			out = append(out, v)
		}
	}
	return out
}

// queueTomorrow re-arms the run for the next day (on_limit=queue_tomorrow).
func (f *Feature) queueTomorrow(ctx context.Context, accountID, runID string) {
	if f.q == nil {
		f.log.Warn("multiapply: queue not installed; cannot re-arm run for tomorrow", "run", runID)
		return
	}
	tomorrow := f.opt.Now().Add(24 * time.Hour)
	err := f.q.Enqueue(ctx, queue.Task{
		AccountID: accountID,
		Kind:      queue.KindApply,
		RunID:     runID,
		DedupKey:  "multiapply:" + runID + ":" + featurekit.DayUTC(tomorrow),
		Priority:  queue.PrioApply,
		Deadline:  tomorrow,
		Payload:   RunPayload{RunID: runID},
	})
	if err != nil {
		f.log.Warn("multiapply: tomorrow enqueue failed", "run", runID, "err", err)
	}
}

// collect walks the run's source with the maxPages pagination cap: a
// non-empty run.Query uses the search source, otherwise the per-resume
// suitable-vacancies source.
func (f *Feature) collect(ctx context.Context, run config.Run, api hhapi.HHAPI, resumeHash string) ([]hhapi.Vacancy, error) {
	var out []hhapi.Vacancy
	if run.Query == "" {
		for page := 0; page < maxPages; page++ {
			vp, err := api.SuitableVacancies(ctx, resumeHash, page)
			if err != nil {
				return out, err
			}
			out = append(out, vp.Items...)
			if page+1 >= vp.Pages || len(vp.Items) == 0 {
				break
			}
		}
		return out, nil
	}
	q := f.searchQuery(run)
	for page := 0; page < maxPages; page++ {
		q.Page = page
		vp, err := api.SearchVacancies(ctx, q)
		if err != nil {
			return out, err
		}
		out = append(out, vp.Items...)
		if page+1 >= vp.Pages || len(vp.Items) == 0 {
			break
		}
	}
	return out, nil
}

// searchQuery maps the run profile onto the native hh search filters (F1).
func (f *Feature) searchQuery(run config.Run) hhapi.SearchQuery {
	flt := run.Filters
	return hhapi.SearchQuery{
		Text:              run.Query,
		Areas:             flt.Areas,
		Experience:        flt.Experience,
		Schedule:          flt.Schedule,
		Employment:        flt.Employment,
		Industries:        flt.Industries,
		SalaryFrom:        flt.SalaryFrom,
		Currency:          flt.SalaryCurrency,
		LabelNoAgencies:   flt.ExcludeAgencies,
		SearchFields:      run.SearchFields,
		PeriodDays:        flt.SearchPeriodDays,
		ProfessionalRoles: flt.ProfessionalRoles,
		Metros:            flt.Metros,
		WorkFormats:       flt.WorkFormats,
		Labels:            flt.Labels,
		Part:              flt.Part,
		OnlyWithSalary:    flt.OnlyWithSalary,
		OrderBy:           flt.OrderBy,
		PerPage:           50,
	}
}
