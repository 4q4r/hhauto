package multiapply

// T26 autoclone: when a run's vacancy pool is exhausted (every vacancy that
// survives the static filters is already applied by the resume lineage), a
// bounded number of hh resume clones is created and driven through the same
// apply pipeline as implicit clone runs.
//
// Owner decisions (grilling round): reapply_mode gates which vacancies a
// clone may (re)apply to; max_clones_per_resume bounds direct children;
// max_clone_depth bounds clone-of-clone chains; clones are full resumes and
// inherit the parent run config (cascade, limits, letter_mode) with point
// overrides; lineage is tracked locally because hh does not expose the clone
// parent (no parentResumeId on live clones, live-verified 2026-09-25); a hh
// resume limit or a clone stuck in not_finished parks the account's
// autoclone for review.
import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/console"
	"hhauto/internal/features/autoclone"
	"hhauto/internal/features/relevance"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// cloneStopKey is the feature_state flag: once set, no more clones are
// created for the account until a human resolves the review item.
const cloneStopKey = "autoclone:stopped"

// defaultClonePollInterval is the bounded not_finished poll step (GET-only).
const defaultClonePollInterval = 20 * time.Second

// cloneRunState carries clone-run specifics through one pass; nil for parent
// runs (no reapply gating, the account-level intent fence stays as-is).
type cloneRunState struct {
	resume   string              // the clone resume hash this pass applies with
	mode     string              // reapply_mode (config vocabulary)
	touched  map[int64]bool      // vacancies already applied by the lineage
	rejected map[int64]time.Time // vacancy → latest in-window family rejection
}

// intentKey builds the idempotency fence key. Parent runs keep the
// account-level key (existing semantics); clone runs scope it per resume so
// a re-applied vacancy does not trip the parent's done intent.
func (rc *cloneRunState) intentKey(accountID string, vacancyID int64) string {
	if rc == nil || rc.resume == "" {
		return "apply:" + accountID + ":" + strconv.FormatInt(vacancyID, 10)
	}
	return "apply:" + accountID + ":" + strconv.FormatInt(vacancyID, 10) + ":" + rc.resume
}

// reapplySkip returns the skip reason when the reapply policy excludes the
// vacancy from this clone pass (T26 owner decision #1).
func (rc *cloneRunState) reapplySkip(v hhapi.Vacancy) string {
	if rc == nil {
		return ""
	}
	switch rc.mode {
	case config.ReapplyNewOnly:
		if rc.touched[v.ID] {
			return "reapply_touched"
		}
	case config.ReapplySkipRecentRejections:
		if _, ok := rc.rejected[v.ID]; ok {
			return "reapply_recent_rejection"
		}
	case config.ReapplyAll:
	}
	return ""
}

// deriveCloneRun copies the parent run for the implicit clone run: filters,
// limits and letter_mode are inherited wholesale; RunOverride carries the
// point overrides (zero fields inherit).
func deriveCloneRun(run config.Run, ov config.RunOverride) config.Run {
	out := run
	if ov.MaxPerDay > 0 {
		out.MaxPerDay = ov.MaxPerDay
	}
	if ov.LetterMode != "" {
		out.LetterMode = ov.LetterMode
	}
	if ov.LetterTemplate != "" {
		out.LetterTemplate = ov.LetterTemplate
	}
	return out
}

// executeRun performs one pass over the resume pool and evaluates the
// autoclone trigger afterwards. depth counts clone generations from the
// configured run's root resume (0 = the configured resume itself).
func (f *Feature) executeRun(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, rc *cloneRunState, depth int,
) error {
	vacancies, stopped, err := f.runPass(ctx, runID, run, accountID, api, prof, resumeHash, rc)
	if err != nil {
		return err
	}
	if stopped {
		return nil // kill switch or profile stop: the pool was not fully evaluated
	}
	return f.maybeAutoClone(ctx, runID, run, accountID, api, prof, resumeHash, vacancies, depth)
}

// passStats accumulates one pass's console digest counters (T69). The
// struct is local to a runPass invocation and threaded through the
// pipeline call chain — never a Feature field, since passes for different
// accounts run concurrently.
type passStats struct {
	applied int
	letters int
	errors  int
	skips   map[string]int
	// gray is the current vacancy's gray-band verdict (T75): set when the
	// gray-band policy routed a needs_human verdict to apply, consumed by
	// the apply-record points (console note + ledger origin flag).
	gray *relevance.Verdict
}

func newPassStats() *passStats {
	return &passStats{skips: map[string]int{}}
}

// skipped totals the per-reason skip counts.
func (ps *passStats) skipped() int {
	total := 0
	for _, n := range ps.skips {
		total += n
	}
	return total
}

// runPass is one collect → filter → apply pass with the given resume; it
// returns the collected pool so the autoclone trigger can judge exhaustion.
func (f *Feature) runPass(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, rc *cloneRunState,
) (vacancies []hhapi.Vacancy, stopped bool, err error) {
	cfg := f.opt.Config
	vacancies, err = f.collect(ctx, run, api, resumeHash)
	if err != nil {
		f.incr(ctx, "apply.error")
		return nil, false, fmt.Errorf("multiapply: collect for %s: %w", runID, err)
	}
	if cfg.Features.MultiApply.PrioritizeOnlineHR {
		vacancies = prioritizeOnline(vacancies)
	}
	if rc != nil {
		f.prepareReapplyState(ctx, accountID, resumeHash, vacancies, rc)
	}

	batch := map[int64]bool{}
	stats := newPassStats()
	for _, v := range vacancies {
		if f.opt.Stopped != nil && f.opt.Stopped(ctx) {
			// Kill switch: the vacancy processed last has fully settled
			// (its intents are closed); the rest of the batch is not
			// started. The engine shuts down gracefully around this.
			f.log.Info("multiapply: kill switch tripped — stopping the vacancy batch",
				"run", runID, "account", accountID)
			return vacancies, true, nil
		}
		stop, skip := f.processVacancy(ctx, runID, run, accountID, api, prof, resumeHash, v, batch, rc, stats)
		switch {
		case stop != "":
			f.incr(ctx, "apply.stop."+stop)
			if stop != stopLoggedOut { // a logged-out account needs a human, not a tomorrow retry
				f.queueTomorrow(ctx, accountID, runID)
			}
			// T69: a stopped pass still reports the counters it gathered.
			f.console.Digest(console.DigestLine{
				Applied: stats.applied, Letters: stats.letters,
				Skipped: stats.skipped(), SkipBreakdown: console.SkipBreakdownByReason(stats.skips),
				Errors: stats.errors,
			})
			return vacancies, true, nil
		case skip != "":
			f.incr(ctx, "apply.skip."+skip)
			stats.skips[skip]++
			f.console.Skip(accountID, skip, v)
		}
	}
	f.console.Digest(console.DigestLine{
		Applied: stats.applied, Letters: stats.letters,
		Skipped: stats.skipped(), SkipBreakdown: console.SkipBreakdownByReason(stats.skips),
		Errors: stats.errors,
	})
	return vacancies, false, nil
}

// maybeAutoClone evaluates pool exhaustion and, when the gates allow it,
// clones the resume and executes the implicit clone run.
func (f *Feature) maybeAutoClone(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, vacancies []hhapi.Vacancy, depth int,
) error {
	ac := f.opt.Config.Features.AutoClone
	if !ac.Enabled {
		return nil
	}
	if f.cloneStopped(ctx, accountID) {
		f.incr(ctx, "autoclone.skip_stopped")
		return nil
	}
	family, err := f.lineageFamily(ctx, accountID, resumeHash)
	if err != nil {
		return err
	}
	exhausted, err := f.lineageExhausted(ctx, run, accountID, family, vacancies)
	if err != nil {
		return err
	}
	if !exhausted {
		return nil
	}
	f.incr(ctx, "autoclone.exhausted")
	f.log.Info("multiapply: vacancy pool exhausted for the resume lineage",
		"run", runID, "account", accountID, "resume", resumeHash)

	if depth+1 > ac.MaxCloneDepth {
		f.incr(ctx, "autoclone.skip_depth")
		return nil
	}
	children, err := f.opt.DB.ResumeClonesCountChildren(ctx, accountID, resumeHash)
	if err != nil {
		return err
	}
	if children >= ac.MaxClonesPerResume {
		f.incr(ctx, "autoclone.skip_limit")
		return nil
	}
	if f.opt.Config.General.DryRun {
		// Dry-run: evaluate and surface the decision, mutate nothing.
		f.log.Info("multiapply: dry-run autoclone",
			"account", accountID, "run", runID, "resume", resumeHash, "depth", depth+1)
		f.incr(ctx, "autoclone.dry_run")
		return nil
	}

	newHash, err := f.doClone(ctx, runID, accountID, api, resumeHash)
	if err != nil || newHash == "" {
		return err // handled inside doClone (review/stop) or a real failure
	}
	return f.spawnCloneRun(ctx, runID, run, accountID, api, prof, resumeHash, newHash, depth)
}

// doClone performs the guarded clone mutation and records the local lineage.
// err == nil with an empty hash means the failure was already handled
// (review + clone stop / account stop); the run stays clean.
func (f *Feature) doClone(ctx context.Context, runID, accountID string, api hhapi.HHAPI, resumeHash string) (string, error) {
	cloner, ok := api.(hhapi.ResumeCloner)
	if !ok {
		f.review(ctx, accountID, "autoclone_unavailable",
			"autoclone enabled but the hh client does not support CloneResume",
			fmt.Sprintf(`{"target":"autoclone:%s","account_id":%q}`, accountID, accountID))
		f.incr(ctx, "autoclone.error")
		return "", nil
	}
	f.incr(ctx, "autoclone.attempt")
	newHash, err := cloner.CloneResume(ctx, resumeHash)
	if err == nil {
		f.incr(ctx, "autoclone.cloned")
		f.log.Info("multiapply: resume cloned",
			"account", accountID, "run", runID, "parent", resumeHash, "clone", newHash)
		return newHash, nil
	}
	switch {
	case errors.Is(err, hhapi.ErrResumeLimit):
		// hh account resume quota: park the account's autoclone for review.
		f.stopClones(ctx, accountID, "clone_resume hit the hh resume limit: "+err.Error())
	case errors.Is(err, hhapi.ErrLoggedOut):
		f.stopAccount(ctx, accountID, "clone_resume: "+err.Error())
	case errors.Is(err, hhapi.ErrCloneMaterialize), errors.Is(err, hhapi.ErrCloneIncomplete):
		// T27: the shell was created but is not a usable duplicate (the
		// builder update is gated for off-browser clients as of
		// 2026-09-25). Park for manual completion; retrying would only
		// stack more shells on the account.
		f.stopClones(ctx, accountID, "clone materialization incomplete: "+err.Error())
		f.incr(ctx, "autoclone.incomplete")
		return "", nil
	default:
		var amb *hhapi.AmbiguousError
		if errors.As(err, &amb) {
			// The mutation may have landed; resolve by reading, never by
			// repeating — and only a human can match an unparented new
			// resume to this lineage.
			f.review(ctx, accountID, "autoclone_ambiguous",
				"clone outcome unknown: "+err.Error(),
				fmt.Sprintf(`{"target":"autoclone:%s","account_id":%q,"parent":%q}`, accountID, accountID, resumeHash))
			f.incr(ctx, "autoclone.ambiguous")
			return "", nil
		}
		f.incr(ctx, "autoclone.error")
		f.log.Warn("multiapply: clone failed", "account", accountID, "resume", resumeHash, "err", err)
		return "", nil
	}
	return "", nil
}

// spawnCloneRun registers the lineage, waits for the clone to finish
// (bounded, GET-only) and executes the implicit clone run.
func (f *Feature) spawnCloneRun(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, parentHash, newHash string, depth int,
) error {
	parent, err := f.opt.DB.ResumeClonesGet(ctx, parentHash)
	if err != nil {
		return err
	}
	parentDepth := 0
	if parent != nil {
		parentDepth = parent.Depth
	}
	if err := f.opt.DB.ResumeClonesRegister(ctx, &store.ResumeClone{
		CloneHash: newHash, ParentHash: parentHash, Depth: parentDepth + 1, AccountID: accountID,
	}); err != nil {
		return err
	}

	// T29: improve the fresh clone first (per-field AI values through the
	// builder update walk; the diff artifact lands on the lineage row) and
	// only then apply the source-resume policy — the action rides on
	// clone+improve success.
	improved := f.improveClone(ctx, runID, accountID, parentHash, newHash)
	if improved {
		f.applyOriginAction(ctx, accountID, api, parentHash)
	}

	ready, err := f.awaitCloneReady(ctx, accountID, api, newHash)
	if err != nil {
		return err
	}
	if !ready {
		return nil // review + clone stop already recorded
	}

	ac := f.opt.Config.Features.AutoClone
	cloneRun := deriveCloneRun(run, ac.RunOverride)
	cloneRun.ResumeID = newHash
	rc := &cloneRunState{resume: newHash, mode: ac.ReapplyMode}
	return f.executeRun(ctx, runID, cloneRun, accountID, api, prof, newHash, rc, depth+1)
}

// lineageFamily returns the resume hashes of the resume's lineage: itself,
// its ancestors up to the root, and all descendants. hh exposes no clone
// parent, so this is built from the local resume_clones table.
func (f *Feature) lineageFamily(ctx context.Context, accountID, resumeHash string) (map[string]bool, error) {
	clones, err := f.opt.DB.ResumeClonesListByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	parent := make(map[string]string, len(clones))
	children := make(map[string][]string, len(clones))
	for _, c := range clones {
		parent[c.CloneHash] = c.ParentHash
		children[c.ParentHash] = append(children[c.ParentHash], c.CloneHash)
	}
	family := map[string]bool{resumeHash: true}
	for h, ok := parent[resumeHash]; ok; h, ok = parent[h] {
		family[h] = true
	}
	frontier := []string{resumeHash}
	for len(frontier) > 0 {
		h := frontier[0]
		frontier = frontier[1:]
		for _, ch := range children[h] {
			if !family[ch] {
				family[ch] = true
				frontier = append(frontier, ch)
			}
		}
	}
	return family, nil
}

// lineageExhausted reports whether nothing is left for the lineage: within
// the collected pool, every vacancy that survives the pure static filters
// (F2 exclude words, F3 black/whitelist) is already recorded as applied
// (status applied or dry_run) by some family resume. Vacancies dropped by
// remote/filter verdicts (rating, AI) are conservatively counted against
// exhaustion — a false negative costs one clone opportunity, a false
// positive would create pointless clones.
func (f *Feature) lineageExhausted(ctx context.Context, run config.Run, accountID string,
	family map[string]bool, vacancies []hhapi.Vacancy,
) (bool, error) {
	var considered []hhapi.Vacancy
	ids := make([]int64, 0, len(vacancies))
	for _, v := range vacancies {
		if excluded(v, run.Filters) {
			continue
		}
		if matchEmployer(run.Filters.CompanyBlacklist, v.EmployerID, v.EmployerName) {
			continue
		}
		if len(run.Filters.CompanyWhitelist) > 0 && !matchEmployer(run.Filters.CompanyWhitelist, v.EmployerID, v.EmployerName) {
			continue
		}
		considered = append(considered, v)
		ids = append(ids, v.ID)
	}
	if len(considered) == 0 {
		return false, nil // no exhaustion signal from an empty pool
	}
	apps, err := f.opt.DB.ApplicationsListByAccountAndVacancies(ctx, accountID, ids)
	if err != nil {
		return false, err
	}
	touched := make(map[int64][]string, len(apps))
	for _, a := range apps {
		if a.Status != "applied" && a.Status != "dry_run" {
			continue
		}
		touched[a.VacancyID] = append(touched[a.VacancyID], a.HhResumeID)
	}
	for _, v := range considered {
		familyTouched := false
		for _, h := range touched[v.ID] {
			if family[h] {
				familyTouched = true
				break
			}
		}
		if !familyTouched {
			return false, nil
		}
	}
	return true, nil
}

// prepareReapplyState bulk-loads the per-vacancy facts the reapply policy
// needs: lineage-touched vacancies and in-window family rejections. On a
// lookup failure the state degrades to "everything touched" — a broken read
// must never widen the apply blast radius.
func (f *Feature) prepareReapplyState(ctx context.Context, accountID, resumeHash string,
	vacancies []hhapi.Vacancy, rc *cloneRunState,
) {
	rc.touched = make(map[int64]bool, len(vacancies))
	family, err := f.lineageFamily(ctx, accountID, resumeHash)
	if err != nil {
		f.log.Warn("multiapply: lineage lookup failed; reapply policy degrades to skip-all", "err", err)
		for _, v := range vacancies {
			rc.touched[v.ID] = true
		}
		return
	}
	ids := make([]int64, 0, len(vacancies))
	for _, v := range vacancies {
		ids = append(ids, v.ID)
	}
	apps, err := f.opt.DB.ApplicationsListByAccountAndVacancies(ctx, accountID, ids)
	if err != nil {
		f.log.Warn("multiapply: applications lookup failed; reapply policy degrades to skip-all", "err", err)
		for _, v := range vacancies {
			rc.touched[v.ID] = true
		}
		return
	}
	for _, a := range apps {
		if (a.Status == "applied" || a.Status == "dry_run") && family[a.HhResumeID] {
			rc.touched[a.VacancyID] = true
		}
	}
	if rc.mode != config.ReapplySkipRecentRejections {
		return
	}
	days := f.opt.Config.Features.AutoClone.SkipRejectionDays
	if days <= 0 {
		days = 30
	}
	cutoff := f.opt.Now().AddDate(0, 0, -days)
	chats, err := f.opt.DB.ChatsListRejected(ctx, accountID)
	if err != nil {
		f.log.Warn("multiapply: rejected chats lookup failed; rejection window ignored", "err", err)
		return
	}
	rc.rejected = make(map[int64]time.Time)
	for _, c := range chats {
		if c.VacancyID == nil || c.LastMessageAt == nil {
			continue
		}
		if c.LastMessageAt.Before(cutoff) {
			continue
		}
		if rc.rejected[*c.VacancyID].Before(*c.LastMessageAt) {
			rc.rejected[*c.VacancyID] = *c.LastMessageAt
		}
	}
}

// awaitCloneReady refreshes the account resumes and waits (bounded, GET-only
// polls) for the clone to leave the not_finished state. ok=false means the
// clone never became drivable: review + clone stop are recorded here.
// The poll budget is wall-clock (time.Now) by design: the injected feature
// clock may be frozen in tests, and a frozen clock must never turn this
// bounded wait into an unbounded one.
func (f *Feature) awaitCloneReady(ctx context.Context, accountID string, api hhapi.HHAPI, cloneHash string) (bool, error) {
	timeout := f.opt.CloneFinishTimeout
	if timeout <= 0 {
		minutes := f.opt.Config.Features.AutoClone.FinishTimeoutMin
		if minutes <= 0 {
			minutes = 10
		}
		timeout = time.Duration(minutes) * time.Minute
	}
	poll := f.opt.ClonePollInterval
	if poll <= 0 {
		poll = defaultClonePollInterval
	}
	deadline := time.Now().Add(timeout)
	for {
		status, err := f.refreshResumeStatus(ctx, accountID, api, cloneHash)
		if err != nil {
			if errors.Is(err, hhapi.ErrLoggedOut) {
				f.stopAccount(ctx, accountID, "clone verify: "+err.Error())
				return false, nil
			}
			return false, err
		}
		switch {
		case status == "":
			// The clone did not surface in the mine shard: without the
			// resume in the account list it cannot be driven — park.
			f.stopClones(ctx, accountID,
				fmt.Sprintf("clone %s missing from the account resume list after clone", cloneHash))
			return false, nil
		case status != "not_finished":
			return true, nil
		}
		if !time.Now().Add(poll).Before(deadline) {
			f.stopClones(ctx, accountID,
				fmt.Sprintf("clone %s stuck in not_finished for %s", cloneHash, timeout))
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// refreshResumeStatus write-throughs the live resume list and returns the
// stored status of hash ("" when absent).
func (f *Feature) refreshResumeStatus(ctx context.Context, accountID string, api hhapi.HHAPI, hash string) (string, error) {
	if err := f.opt.DB.AccountsUpsert(ctx, store.Account{ID: accountID}); err != nil {
		f.log.Warn("multiapply: account ensure failed", "account", accountID, "err", err)
	}
	got, err := api.ListResumes(ctx)
	if err != nil {
		return "", err
	}
	status := ""
	for _, r := range got {
		if r.HHID == "" {
			continue
		}
		row := store.Resume{
			AccountID: accountID, HhResumeID: r.HHID, Title: r.Title,
			Status: r.Status, Raw: string(r.Raw), UpdatedAt: r.UpdatedAt,
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = f.opt.Now()
		}
		if uerr := f.opt.DB.ResumesUpsert(ctx, &row); uerr != nil {
			return "", uerr
		}
		if r.HHID == hash {
			status = r.Status
		}
	}
	return status, nil
}

// stopClones parks the account's autoclone for human attention (hh resume
// limit, stuck not_finished, missing clone).
func (f *Feature) stopClones(ctx context.Context, accountID, reason string) {
	if err := f.opt.DB.FeatureStateSet(ctx, feature, accountID, cloneStopKey, "1"); err != nil {
		f.log.Error("multiapply: clone stop flag write failed", "account", accountID, "err", err)
	}
	f.review(ctx, accountID, "autoclone_stopped", reason,
		fmt.Sprintf(`{"target":"autoclone:%s","account_id":%q}`, accountID, accountID))
	f.incr(ctx, "autoclone.stopped")
	f.log.Warn("multiapply: autoclone stopped for account", "account", accountID, "reason", reason)
}

// cloneStopped reports whether the account's autoclone is parked.
func (f *Feature) cloneStopped(ctx context.Context, accountID string) bool {
	v, ok, err := f.opt.DB.FeatureStateGet(ctx, feature, accountID, cloneStopKey)
	return err == nil && ok && v == "1"
}

// improveClone runs the T29 per-field improvement of the fresh clone. A
// fatal improve error is logged and counted, never propagated: the clone
// is a verified, usable duplicate either way (unimproved), and the origin
// action checks the outcome.
func (f *Feature) improveClone(ctx context.Context, runID, accountID, parentHash, cloneHash string) bool {
	if f.opt.Improver == nil {
		return true
	}
	f.incr(ctx, "autoclone.improve.attempt")
	if _, err := f.opt.Improver.ImproveClone(ctx, accountID, parentHash, cloneHash); err != nil {
		f.incr(ctx, "autoclone.improve.error")
		f.log.Warn("multiapply: clone improve failed; the clone stays unimproved",
			"account", accountID, "run", runID, "parent", parentHash, "clone", cloneHash, "err", err)
		return false
	}
	f.incr(ctx, "autoclone.improve.ok")
	f.log.Info("multiapply: clone improved", "account", accountID, "run", runID, "clone", cloneHash)
	return true
}

// applyOriginAction executes the configured source-resume policy (T29
// clone_original_action) after a successful clone+improve. keep/default is
// a no-op; unknown or failed actions degrade to keep inside the helper.
func (f *Feature) applyOriginAction(ctx context.Context, accountID string, api hhapi.HHAPI, parentHash string) {
	action := f.opt.Config.Features.AutoClone.CloneOriginalAction
	if action == "" || action == config.CloneActionKeep {
		return
	}
	mutator, ok := api.(autoclone.OriginMutator)
	if !ok {
		f.log.Warn("multiapply: clone_original_action configured but the client does not support resume management; keeping the original",
			"account", accountID, "resume", parentHash, "action", action)
		f.incr(ctx, "autoclone.origin.unsupported")
		return
	}
	f.incr(ctx, "autoclone.origin."+action)
	autoclone.ApplyOriginAction(ctx, mutator, action, parentHash, f.log)
}
