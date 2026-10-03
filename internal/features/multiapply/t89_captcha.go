package multiapply

// T89: the captcha postpone policy — a captcha challenge parks the vacancy
// (retry_at = now + retry_after, a captcha_postponed registry row) instead
// of skipping it; the limit-th challenge of a pass pauses the apply phase
// for the cooldown (the scoring/letters keep running, the ready letters
// apply first after the resume); due postpones are re-examined with fresh
// card data (archived → vacancy_archived skip, alive → re-attack). mode=
// "skip" keeps the pre-T89 day-breaker behavior untouched.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/queue"
	"hhauto/internal/store"
)

// Skip reasons and metrics of the postpone cycle.
const (
	skipCaptchaPostponed = "captcha_postponed"
	skipCaptchaExhausted = "captcha_exhausted"
	skipVacancyArchived  = "vacancy_archived"
	metricPostponedRetry = "apply.postponed_retry"
)

// PostponePayload is the typed queue payload for KindCaptchaPostponed wake
// tasks: the parked vacancy's identity, fired at retry_at.
type PostponePayload struct {
	RunID      string `json:"run_id"`
	ResumeHash string `json:"resume_hash"`
	VacancyID  int64  `json:"vacancy_id"`
}

// captchaCfg resolves the policy with the documented defaults — hand-built
// test configs bypass config.applyDefaults, so zero values must not change
// the owner behavior.
func (f *Feature) captchaCfg() config.MultiApplyCaptcha {
	c := f.opt.Config.Features.MultiApply.Captcha
	if c.Limit <= 0 {
		c.Limit = config.DefaultCaptchaLimit
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = config.DefaultCaptchaMaxRetries
	}
	if c.Cooldown == "" {
		c.Cooldown = config.DefaultCaptchaCooldown
	}
	if c.RetryAfter == "" {
		c.RetryAfter = config.DefaultCaptchaRetryAfter
	}
	return c
}

// captchaPostpone reports the postpone policy ("" and "postpone" — the
// owner default; only an explicit "skip" restores the old behavior).
func (f *Feature) captchaPostpone() bool {
	return f.captchaCfg().Mode != config.CaptchaModeSkip
}

// captchaCooldownDur parses [features.multiapply.captcha].cooldown; an
// unparseable value (Validate rejects it in production) degrades to the
// documented default with a warning.
func (f *Feature) captchaCooldownDur() time.Duration {
	return parseCaptchaDuration(f.log, f.captchaCfg().Cooldown, config.DefaultCaptchaCooldown)
}

// captchaRetryAfter parses [features.multiapply.captcha].retry_after.
func (f *Feature) captchaRetryAfter() time.Duration {
	return parseCaptchaDuration(f.log, f.captchaCfg().RetryAfter, config.DefaultCaptchaRetryAfter)
}

func parseCaptchaDuration(log *slog.Logger, raw, fallback string) time.Duration {
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	d, err := time.ParseDuration(fallback)
	if err != nil {
		return time.Hour // unreachable: the fallback is a compile-time literal
	}
	log.Warn("multiapply: bad captcha duration; using the default", "value", raw, "default", fallback)
	return d
}

// parkPostponed records a captcha challenge as a postpone: the registry row
// upserts (retry_at moves, retry_count grows), the console line renders and
// the wake task lands in the queue at retry_at. exhausted=true means the
// vacancy burned its max_retries — the row is dropped and the caller
// settles the vacancy as the captcha_exhausted skip.
func (f *Feature) parkPostponed(ctx context.Context, runID, accountID, resumeHash string,
	v hhapi.Vacancy, stats *passStats,
) bool {
	cfg := f.captchaCfg()
	stats.captchaHits++
	row := store.CaptchaPostponed{
		AccountID: accountID, ResumeHash: resumeHash, VacancyID: v.ID, RunID: runID,
		RetryAt: f.opt.Now().Add(f.captchaRetryAfter()),
	}
	count, err := f.opt.DB.CaptchaPostponedPark(ctx, &row)
	if err != nil {
		// The park row is the schedule, not the vacancy: a failed write
		// still skips this pass, and the fresh pool re-attacks the vacancy
		// later (a postpone never blocks an apply).
		f.log.Error("multiapply: postpone park failed", "vacancy", v.ID, "err", err)
		return false
	}
	if count >= cfg.MaxRetries {
		if derr := f.opt.DB.CaptchaPostponedDelete(ctx, accountID, resumeHash, v.ID); derr != nil {
			f.log.Warn("multiapply: postpone drop failed", "vacancy", v.ID, "err", derr)
		}
		return true
	}
	f.console.CaptchaPostponed(v, row.RetryAt)
	f.enqueuePostponedWake(ctx, accountID, runID, resumeHash, v.ID, row.RetryAt)
	return false
}

// enqueuePostponedWake arms the daemon's maintenance wake at retry_at (the
// T71 deadline rails: NextDeadline → Wake → drainDueTasks → the handler).
func (f *Feature) enqueuePostponedWake(ctx context.Context, accountID, runID, resumeHash string, vacancyID int64, retryAt time.Time) {
	if f.q == nil {
		f.log.Warn("multiapply: queue not installed; the postpone wakes with the next pass", "vacancy", vacancyID)
		return
	}
	err := f.q.Enqueue(ctx, queue.Task{
		AccountID: accountID,
		Kind:      queue.KindCaptchaPostponed,
		RunID:     runID,
		DedupKey:  "captcha_postponed:" + resumeHash + ":" + strconv.FormatInt(vacancyID, 10),
		Priority:  queue.PrioApply,
		Deadline:  retryAt,
		Payload:   PostponePayload{RunID: runID, ResumeHash: resumeHash, VacancyID: vacancyID},
	})
	if err != nil {
		f.log.Warn("multiapply: postpone wake enqueue failed", "vacancy", vacancyID, "err", err)
	}
}

// HandlePostponed is the KindCaptchaPostponed maintenance handler: one due
// parked vacancy re-attacked with fresh data. An early drain (the transient
// queue in a non-daemon run) is a no-op — the persisted row keeps the
// schedule and the pass-start re-exam catches it.
func (f *Feature) HandlePostponed(ctx context.Context, t queue.Task) error {
	if !f.captchaPostpone() {
		return nil
	}
	p, ok := t.Payload.(PostponePayload)
	if !ok {
		return fmt.Errorf("multiapply: bad postpone payload %T", t.Payload)
	}
	row, err := f.opt.DB.CaptchaPostponedGet(ctx, t.AccountID, p.ResumeHash, p.VacancyID)
	if err != nil {
		return err
	}
	if row == nil || row.RetryAt.After(f.opt.Now()) {
		return nil
	}
	run, runOK := f.opt.Config.Runs[row.RunID]
	if !runOK || !run.Enabled {
		f.log.Info("multiapply: postpone run gone — dropping the row", "run", row.RunID, "vacancy", row.VacancyID)
		return f.opt.DB.CaptchaPostponedDelete(ctx, t.AccountID, row.ResumeHash, row.VacancyID)
	}
	api, err := f.opt.APIFor(t.AccountID)
	if err != nil {
		return fmt.Errorf("multiapply: postpone api for %s: %w", t.AccountID, err)
	}
	prof, err := f.loadProfile(ctx, t.AccountID, api)
	if err != nil {
		return fmt.Errorf("multiapply: postpone profile for %s: %w", t.AccountID, err)
	}
	stats := newPassStats()
	batch := map[int64]bool{p.VacancyID: true}
	if stop := f.processPostponedRow(ctx, row.RunID, run, t.AccountID, api, prof, *row, nil, stats, batch); stop != "" {
		f.log.Warn("multiapply: postpone re-attack stopped", "vacancy", p.VacancyID, "stop", stop)
	}
	return nil
}

// processPostponedDue re-attacks every due parked vacancy of this pass's
// resume before the fresh pool (the "потом" of a captcha park). Returns the
// stop reason when the pass must halt.
func (f *Feature) processPostponedDue(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, rc *cloneRunState, stats *passStats, batch map[int64]bool,
) string {
	rows, err := f.opt.DB.CaptchaPostponedDue(ctx, accountID, resumeHash, f.opt.Now())
	if err != nil {
		f.log.Warn("multiapply: postpone listing failed", "err", err)
		return ""
	}
	for _, row := range rows {
		if f.opt.Stopped != nil && f.opt.Stopped(ctx) {
			return "" // the phase-1 kill check takes over
		}
		if stop := f.processPostponedRow(ctx, runID, run, accountID, api, prof, row, rc, stats, batch); stop != "" {
			return stop
		}
	}
	return ""
}

// processPostponedRow re-examines one parked vacancy: refreshed card data
// (archived → vacancy_archived), the applied-since dedup (the leftover row
// settles silently), otherwise the full apply cascade. Any settled outcome
// leaves the registry; only a fresh park keeps the row.
func (f *Feature) processPostponedRow(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, row store.CaptchaPostponed, rc *cloneRunState, stats *passStats,
	batch map[int64]bool,
) string {
	settle := func() {
		if err := f.opt.DB.CaptchaPostponedDelete(ctx, accountID, row.ResumeHash, row.VacancyID); err != nil {
			f.log.Warn("multiapply: postpone settle failed", "vacancy", row.VacancyID, "err", err)
		}
	}
	if app, err := f.opt.DB.ApplicationsGetByDedupKeyLive(ctx,
		featurekit.DedupKey(accountID, row.ResumeHash, row.VacancyID)); err == nil && app != nil {
		settle() // applied since parking — the dedup owns the vacancy now
		return ""
	}
	batch[row.VacancyID] = true // the fresh pool must not double-process it this pass
	card, err := api.VacancyCard(ctx, row.VacancyID)
	if err != nil {
		f.log.Warn("multiapply: postponed card refresh failed", "vacancy", row.VacancyID, "err", err)
		return "" // stays parked; the next due pass re-reads it
	}
	if card.Archived {
		settle()
		f.incr(ctx, "apply.skip."+skipVacancyArchived)
		stats.skips[skipVacancyArchived]++
		f.console.Skip(accountID, skipVacancyArchived, *card)
		return ""
	}
	f.incr(ctx, metricPostponedRetry)
	cand := candidate{v: *card}
	stop, skip := f.applyCandidate(ctx, runID, run, accountID, api, prof, row.ResumeHash, &cand, rc, stats)
	switch {
	case stop != "":
		return stop
	case skip != "":
		f.incr(ctx, "apply.skip."+skip)
		stats.skips[skip]++
		if skip != skipCaptchaPostponed {
			// The park renders its own line; the exhausted skip deleted the
			// row in parkPostponed; every other skip settles the vacancy.
			f.console.Skip(accountID, skip, *card)
		}
		if skip != skipCaptchaPostponed && skip != skipCaptchaExhausted {
			settle()
		}
	default:
		settle() // applied (or a dry-run would-apply)
	}
	return ""
}

// cooldownSleep waits out the apply-phase pause in small steps, honoring
// the kill switch and the context. Wall-clock on purpose: the feature clock
// is frozen in tests, the pause must elapse everywhere.
func (f *Feature) cooldownSleep(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if f.opt.Stopped != nil && f.opt.Stopped(ctx) {
			return false
		}
		if err := ctx.Err(); err != nil {
			return false
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return true
		}
		step := remain
		if step > time.Second {
			step = time.Second
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(step):
		}
	}
}

// captchaCooldown is the paused apply phase (T89): the remaining candidates
// prepare their letters (AI work — no apply POST), the pause elapses and
// the ready letters apply first after the resume, in queue order. Returns
// true when the pass must halt (kill switch or a stop outcome).
func (f *Feature) captchaCooldown(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, rest []candidate, rc *cloneRunState, stats *passStats,
) bool {
	cooldown := f.captchaCooldownDur()
	f.console.CaptchaCooldown(f.opt.Now().Add(cooldown))
	type readyApply struct {
		cand   candidate
		letter string
		tests  *hhapi.VacancyTest
	}
	ready := make([]readyApply, 0, len(rest))
	for _, c := range rest {
		if f.opt.Stopped != nil && f.opt.Stopped(ctx) {
			return true
		}
		cand := c
		letter, tests, stop, skip := f.prepareDeferred(ctx, runID, run, accountID, api, prof, resumeHash, &cand, stats)
		switch {
		case stop != "":
			f.stopPass(ctx, runID, accountID, stop, stats)
			return true
		case skip != "":
			f.incr(ctx, "apply.skip."+skip)
			stats.skips[skip]++
			f.console.Skip(accountID, skip, cand.v)
		default:
			ready = append(ready, readyApply{cand: cand, letter: letter, tests: tests})
		}
	}
	if !f.cooldownSleep(ctx, cooldown) {
		return true
	}
	f.console.CaptchaResume(len(ready))
	for _, p := range ready {
		cand := p.cand
		stop, skip := f.submitApply(ctx, runID, run, accountID, api, prof, resumeHash, p.cand.v, p.letter, p.tests, rc, stats)
		switch {
		case stop != "":
			f.stopPass(ctx, runID, accountID, stop, stats)
			return true
		case skip != "":
			f.incr(ctx, "apply.skip."+skip)
			stats.skips[skip]++
			f.console.Skip(accountID, skip, cand.v)
		}
	}
	return false
}
