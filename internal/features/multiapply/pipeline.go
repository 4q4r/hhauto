package multiapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/features/emplrating"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/relevance"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// Stage names of the filter cascade (used in whitelist_bypass entries).
const (
	stageF4           = "F4"
	stageF6Threshold  = "F6_threshold"
	maxReconcilePages = 3
)

// candidate is a vacancy that survived F2-F6 (static filters + scoring)
// and is queued for the apply phase. verdict is nil only when the AI
// filter is disabled — nothing in the pass was scored, so the queue keeps
// pool order (T84); gray marks a verdict the gray-band policy routed to
// apply (T75).
type candidate struct {
	v       hhapi.Vacancy
	verdict *relevance.Verdict
	gray    bool
}

// processVacancy runs the F2..F8 cascade for one vacancy: evaluate then
// apply back to back. runPass splits the two halves around the T84
// best-first sort; this composition keeps the per-vacancy cascade in one
// place (and serves the benchmark).
func (f *Feature) processVacancy(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, v hhapi.Vacancy, batch map[int64]bool, rc *cloneRunState,
	stats *passStats,
) (stop, skip string) {
	skip, cand := f.evaluateVacancy(ctx, runID, run, accountID, api, prof, resumeHash, v, batch, rc)
	if skip != "" {
		return "", skip
	}
	return f.applyCandidate(ctx, runID, run, accountID, api, prof, resumeHash, cand, rc, stats)
}

// evaluateVacancy is the read-only half of the cascade: in-batch dedup,
// the T77 terminal registry, the reapply policy, F2-F5 static filters and
// the F6 AI relevance gate. A non-empty skip settles the vacancy without
// an apply attempt; otherwise the scored candidate joins the T84 queue.
func (f *Feature) evaluateVacancy(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, v hhapi.Vacancy, batch map[int64]bool, rc *cloneRunState,
) (skip string, cand *candidate) {
	// In-batch dedup: the same vacancy from two sources is processed once.
	if batch[v.ID] {
		return "dup_batch", nil
	}
	batch[v.ID] = true

	// T77: a client-side validation failure (hh 400/422) is terminal for
	// the pass — another run in the same cycle must not re-POST it.
	if f.terminalFailed(accountID, v.ID) {
		return "apply_failed", nil
	}

	// T26 reapply policy (clone runs only): which vacancies may this clone
	// (re)apply to.
	if skip := rc.reapplySkip(v); skip != "" {
		return skip, nil
	}

	// F2-F5: static filters, rating and dedup (the seen row is written
	// through as audit trail, never as a skip condition).
	skip, whitelisted := f.prefilter(ctx, runID, run, accountID, api, resumeHash, v)
	if skip != "" {
		return skip, nil
	}

	// F6: AI relevance (threshold bypassed for whitelisted employers
	// listing "F6_threshold"; needs_human/injection still gate).
	return f.aiFilterSkip(ctx, runID, run, accountID, prof, resumeHash, v, whitelisted,
		bypassSet(run.Filters.WhitelistBypass))
}

// applyCandidate is the mutating half of the cascade: F7 daily quotas,
// the F6.5 popup feasibility probe and the F8 apply with letter, captcha
// retry and ambiguous reconcile. The gray-band verdict (T75) rides the
// pass stats from here to the apply-record points.
func (f *Feature) applyCandidate(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, cand *candidate, rc *cloneRunState,
	stats *passStats,
) (stop, skip string) {
	if cand.gray {
		stats.gray = cand.verdict
	}

	// F7: daily quotas — cheap DB checks run BEFORE the expensive popup
	// request (Wave 3 review fix 1).
	if stop, skip := f.quotaCheck(ctx, runID, run, accountID, cand.v); stop != "" || skip != "" {
		return stop, skip
	}

	// F6.5: feasibility from the apply popup (T80: a vacancy with tests
	// also yields its fetched VacancyTest — nil means none or unreadable).
	stop, skip, letterRequired, tests := f.popupGate(ctx, accountID, api, cand.v)
	if stop != "" || skip != "" {
		return stop, skip
	}

	// F8: apply.
	return f.applyOne(ctx, runID, run, accountID, api, prof, resumeHash, cand.v, letterRequired, tests, rc, stats)
}

// sortByScoreDesc is the T84 apply queue, best-first: score descending
// (the scorer's verdict); within one score tier the prioritize_online_hr
// preference holds (gated on the config flag — EmployerOnline parses from
// the search item regardless of it); vacancy id ascending closes the
// order into a total deterministic sequence. Candidates without a verdict
// (AI filter off — nothing was scored) have no score to rank by: they
// keep pool order after the scored ones.
func sortByScoreDesc(cands []candidate, online bool) {
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := &cands[i], &cands[j]
		switch {
		case a.verdict == nil && b.verdict == nil:
			return false
		case a.verdict == nil:
			return false // b (scored) first
		case b.verdict == nil:
			return true // a (scored) first
		case a.verdict.Score != b.verdict.Score:
			return a.verdict.Score > b.verdict.Score
		case online && a.v.EmployerOnline != b.v.EmployerOnline:
			return a.v.EmployerOnline
		default:
			return a.v.ID < b.v.ID
		}
	})
}

// applyOne performs the mutation with intent, letter, captcha retry and
// ambiguous reconcile. rc (clone run state) scopes the intent fence and the
// retry path to the clone resume. tests (T80) is the fetched vacancy test:
// non-nil means the apply must carry the AI-solved answers, and any solve
// failure falls back to the T77 test_required skip.
func (f *Feature) applyOne(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, v hhapi.Vacancy, letterRequired bool,
	tests *hhapi.VacancyTest, rc *cloneRunState,
	stats *passStats,
) (stop, skip string) {
	// Cover letter: required by the popup or configured by the run.
	letter, skip := f.prepareLetter(ctx, runID, run, accountID, api, prof, v, resumeHash, letterRequired, stats)
	if skip != "" {
		return "", skip
	}

	if f.dryRunApply(ctx, runID, accountID, resumeHash, v, letter, stats) {
		return "", ""
	}

	if f.opt.Pacer != nil {
		if err := f.opt.Pacer.Wait(ctx, accountID); err != nil {
			return "", "pacer_cancelled"
		}
	}

	claim, err := featurekit.BeginIntent(ctx, f.opt.DB,
		rc.intentKey(accountID, v.ID),
		featurekit.IntentPayload(RunPayload{RunID: runID}))
	if err != nil {
		f.log.Error("multiapply: begin intent failed", "vacancy", v.ID, "err", err)
		return "", "intent_error"
	}
	switch claim.Step {
	case featurekit.IntentSkipPending:
		return "", "intent_pending"
	case featurekit.IntentReconcileFirst:
		return "", "intent_done"
	}

	// T80: solve the test inside the intent fence (the letter is already
	// built and rides the same POST). Any solve failure abandons the fence
	// and falls back to the T77 skip — the vacancy is never half-submitted.
	var applyTest *hhapi.ApplyTest
	if tests != nil {
		solveCtx, cancel := f.solveContext(ctx)
		applyTest = f.solveTest(solveCtx, runID, accountID, v, tests, prof)
		cancel()
		if applyTest == nil {
			serr := errors.New("multiapply: vacancy test could not be solved honestly")
			_ = featurekit.FinishIntent(ctx, f.opt.DB, claim, serr)
			f.log.Warn("multiapply: test solve failed — falling back to test_required", "vacancy", v.ID)
			return "", "test_required"
		}
	}

	skip, err = f.sendWithCaptcha(ctx, accountID, api, v, resumeHash, letter, applyTest, claim)
	if skip != "" {
		return "", skip
	}
	if err == nil {
		metric := "apply.ok"
		if applyTest != nil {
			metric = "apply.test_solved"
		}
		f.recordApplied(ctx, runID, accountID, resumeHash, v, claim, letter, metric, stats)
		return "", ""
	}

	// Already responded: hh's validation carries the marker — mark applied
	// silently (no webhook, no apply.ok metric).
	var se *hhapi.StatusError
	if errors.As(err, &se) && errors.Is(err, hhapi.ErrValidation) && alreadyMarker(err) {
		_ = featurekit.FinishIntent(ctx, f.opt.DB, claim, nil)
		f.createApplication(ctx, runID, accountID, resumeHash, v, "applied", letter, "", stats)
		f.incr(ctx, "apply.already")
		f.bumpEmployer(ctx, accountID, v.EmployerID)
		return "", ""
	}

	if errors.Is(err, hhapi.ErrLoggedOut) {
		_ = featurekit.FinishIntent(ctx, f.opt.DB, claim, err)
		f.stopAccount(ctx, accountID, "apply: "+err.Error())
		return stopLoggedOut, ""
	}

	// AMBIGUOUS: the mutation may or may not have landed — resolve by
	// reading, never by repeating.
	var amb *hhapi.AmbiguousError
	if errors.As(err, &amb) {
		handled, _ := f.resolveAmbiguous(ctx, runID, accountID, api, resumeHash, v, letter, applyTest, claim, err, rc, stats)
		if handled {
			return "", ""
		}
	}

	// Plain failure.
	_ = featurekit.FinishIntent(ctx, f.opt.DB, claim, err)
	if skip := f.failApply(ctx, runID, accountID, resumeHash, v, letter, err, stats); skip != "" {
		return "", skip
	}
	return "", ""
}

// failApply records a plain (non-ambiguous, non-stopping) apply failure:
// the T77 terminal registry, the failed ledger row, the apply.error metric
// and the T69 console line. Returns a skip reason when the failure settles
// the vacancy as a skip instead — the T83 last line: hh itself refusing
// with test-required (the popup detection missed the flag) is the T77
// terminal skip, never the console error line, never an apply.error metric.
func (f *Feature) failApply(ctx context.Context, runID, accountID, resumeHash string,
	v hhapi.Vacancy, letter string, err error, stats *passStats,
) string {
	// T77: hh's 400/422 is a permanent client-side answer — mark the
	// vacancy terminal so no other run in this pass repeats the POST.
	if errors.Is(err, hhapi.ErrValidation) {
		f.markTerminalFailed(accountID, v.ID)
		if testRequiredMarker(err) {
			return "test_required"
		}
	}
	f.createApplication(ctx, runID, accountID, resumeHash, v, "failed", letter, err.Error(), stats)
	f.incr(ctx, "apply.error")
	// T69: the console surface renders the failure line; the structured
	// record stays visible at debug level (one banner per event on the
	// default console — T52).
	f.log.Debug("multiapply: apply failed", "vacancy", v.ID, "err", err)
	return ""
}

// sendApply performs one Apply round trip; applyTest (T80, may be nil)
// carries the solved vacancy test.
func (f *Feature) sendApply(ctx context.Context, api hhapi.HHAPI, v hhapi.Vacancy,
	resumeHash, letter string, applyTest *hhapi.ApplyTest,
) (hhapi.NegotiationRef, error) {
	return api.Apply(ctx, hhapi.ApplyRequest{VacancyID: v.ID, ResumeHash: resumeHash, Letter: letter, Test: applyTest})
}

// markTerminalFailed records a client-side validation failure (hh 400/422)
// for (account, vacancy): the answer is permanent, so re-POSTing the vacancy
// within the pass — another run profile, a clone run — only repeats the
// error (T77). Entries expire after one apply cadence; a later daily pass
// may retry.
func (f *Feature) markTerminalFailed(accountID string, vacancyID int64) {
	f.terminalMu.Lock()
	defer f.terminalMu.Unlock()
	now := f.opt.Now()
	for k, exp := range f.terminal {
		if exp.Before(now) {
			delete(f.terminal, k)
		}
	}
	f.terminal[fmt.Sprintf("%s:%d", accountID, vacancyID)] = now.Add(24 * time.Hour)
}

// terminalFailed reports whether the vacancy failed apply with a client-side
// validation error during this pass window (T77).
func (f *Feature) terminalFailed(accountID string, vacancyID int64) bool {
	f.terminalMu.Lock()
	defer f.terminalMu.Unlock()
	exp, ok := f.terminal[fmt.Sprintf("%s:%d", accountID, vacancyID)]
	return ok && exp.After(f.opt.Now())
}

// retryApplyOnce re-arms the key and repeats the apply a single time (the
// test payload rides along: a retried test apply must replay the same
// answers, never an untested plain POST).
func (f *Feature) retryApplyOnce(ctx context.Context, runID, accountID string, api hhapi.HHAPI,
	v hhapi.Vacancy, resumeHash, letter string, applyTest *hhapi.ApplyTest, rc *cloneRunState,
) (featurekit.IntentClaim, error) {
	claim, err := featurekit.BeginIntent(ctx, f.opt.DB,
		rc.intentKey(accountID, v.ID),
		featurekit.IntentPayload(RunPayload{RunID: runID}))
	if err != nil {
		return claim, fmt.Errorf("multiapply: re-arm intent: %w", err)
	}
	if claim.Step != featurekit.IntentProceed {
		return claim, fmt.Errorf("multiapply: re-arm intent blocked (step %d)", claim.Step)
	}
	ref, err := f.sendApply(ctx, api, v, resumeHash, letter, applyTest)
	_ = ref
	return claim, err
}

// reconcileNegotiations searches the applicant negotiations for the vacancy.
func (f *Feature) reconcileNegotiations(ctx context.Context, api hhapi.HHAPI, vacancyID int64) (bool, error) {
	for page := 1; page <= maxReconcilePages; page++ {
		negs, err := api.Negotiations(ctx, page)
		if err != nil {
			return false, err
		}
		for _, n := range negs {
			if n.VacancyID == vacancyID {
				return true, nil
			}
		}
		if len(negs) == 0 {
			break
		}
	}
	return false, nil
}

// recordApplied closes the flow for a successful (or reconciled) apply.
func (f *Feature) recordApplied(ctx context.Context, runID, accountID, resumeHash string,
	v hhapi.Vacancy, claim featurekit.IntentClaim, letter, metric string, stats *passStats,
) {
	if err := featurekit.FinishIntent(ctx, f.opt.DB, claim, nil); err != nil {
		f.log.Error("multiapply: intent mark failed", "err", err)
	}
	f.createApplication(ctx, runID, accountID, resumeHash, v, "applied", letter, "", stats)
	f.incr(ctx, metric)
	f.bumpEmployer(ctx, accountID, v.EmployerID)
	// Reconciled applies land on the wire as real responses too — the
	// funnel must see them: EvApplySent fires for both, reconciled carries
	// the reconciled:true flag.
	if f.opt.Notifier != nil && (metric == "apply.ok" || metric == "apply.reconciled") {
		ev := webhookEvent(accountID, runID, v)
		if metric == "apply.reconciled" {
			ev.Payload["reconciled"] = true
		}
		f.opt.Notifier.Emit(ev)
	}
}

func (f *Feature) createApplication(ctx context.Context, runID, accountID, resumeHash string,
	v hhapi.Vacancy, status, letter, failReason string, stats *passStats,
) {
	appKey := featurekit.DedupKey(accountID, resumeHash, v.ID)
	// T69: the console surface renders the terminal vacancy line at the
	// record point — one line per outcome, counters ride along.
	switch status {
	case "applied":
		stats.applied++
		f.console.Applied(false, accountID, resumeHash, v, grayNote(stats))
	case "failed":
		stats.errors++
		f.console.Error(accountID, v, failReason)
	}
	// A dry_run survey row for the same vacancy is upgraded in place —
	// ApplicationsCreate would fail on the UNIQUE dedup_key (Wave 3 fix 2).
	if existing, err := f.opt.DB.ApplicationsGetByDedupKey(ctx, appKey); err == nil && existing != nil {
		var reason *string
		if failReason != "" {
			reason = &failReason
		}
		if err := f.opt.DB.ApplicationsMarkStatus(ctx, existing.ID, status, reason); err != nil {
			f.log.Error("multiapply: application upgrade failed", "vacancy", v.ID, "status", status, "err", err)
		}
		return
	}
	app := store.Application{
		AccountID: accountID, RunID: runID, HhResumeID: resumeHash,
		VacancyID: v.ID, EmployerID: v.EmployerID,
		DedupKey: appKey,
		Status:   status,
		Letter:   strPtr(letter),
	}
	if stats.gray != nil {
		// T75: the ledger row marks a gray-band origin so the owner can
		// tell which responses came from the borderline zone. The flag
		// survives the dry_run→applied upgrade (COALESCE keeps it).
		app.FailReason = strPtr(grayBandFlag)
	}
	if status == "applied" {
		t := f.opt.Now()
		app.AppliedAt = &t
	}
	if failReason != "" {
		app.FailReason = &failReason
	}
	if err := f.opt.DB.ApplicationsCreate(ctx, &app); err != nil {
		f.log.Error("multiapply: application record failed", "vacancy", v.ID, "status", status, "err", err)
	}
}

// --- letters ---

// errLetterNeedsHuman marks a cover-letter verdict that must not be sent.
var errLetterNeedsHuman = errors.New("multiapply: cover letter needs human")

// buildLetter produces the outgoing cover letter (T26 modes; T60 resolves
// the effective mode/template from the run fields and the cover_letter
// section in prepareLetter): template sends the rendered static template
// verbatim, template_ai polishes it with the AI (falling back to the
// rendered template), ai_company composes for the company/vacancy through
// the AI runner with the template as context and fallback.
func (f *Feature) buildLetter(ctx context.Context, runID string, mode, tmpl, extraInstructions, accountID string,
	prof profile.Profile, v *hhapi.Vacancy, details *vacancyDetails, stats *passStats,
) (string, error) {
	switch mode {
	case config.LetterModeTemplate:
		if strings.TrimSpace(tmpl) == "" {
			return "", errors.New("multiapply: letter_mode=template requires letter_template")
		}
		return renderLetterTemplate(tmpl, letterTemplateDataOf(*v, details))
	case config.LetterModeTemplateAI:
		if strings.TrimSpace(tmpl) == "" {
			return "", errors.New("multiapply: letter_mode=template_ai requires letter_template")
		}
		if f.opt.Runner != nil {
			letter, aerr := f.aiLetter(ctx, runID, mode, tmpl, extraInstructions, accountID, prof, v, details, stats)
			if aerr == nil {
				return letter, nil
			}
			f.log.Warn("multiapply: cover letter polish failed; using the raw template", "vacancy", v.ID, "err", aerr)
		}
		return renderLetterTemplate(tmpl, letterTemplateDataOf(*v, details))
	default: // config.LetterModeAICompany
		if f.opt.Runner != nil {
			letter, aerr := f.aiLetter(ctx, runID, mode, tmpl, extraInstructions, accountID, prof, v, details, stats)
			if aerr == nil {
				return letter, nil
			}
			f.log.Warn("multiapply: cover letter task failed", "vacancy", v.ID, "err", aerr)
		}
		if tmpl != "" {
			return renderLetterTemplate(tmpl, letterTemplateDataOf(*v, details))
		}
		return "", errors.New("multiapply: no cover letter available")
	}
}

// aiLetter asks the runner for the cover-letter verdict; needs_human,
// injection-flagged or empty letters are refused (errLetterNeedsHuman).
// T86: a model hesitation (needs_human=true) with letter text and no
// injection is NOT refused — the outcome is a deterministic function of
// (injection, empty letter): the owner consciously answers past formal
// requirement gaps (language levels, experience thresholds, missing stack
// items), so hesitation on those must never flip the outcome.
func (f *Feature) aiLetter(ctx context.Context, runID, mode, tmpl, extraInstructions, accountID string,
	prof profile.Profile, v *hhapi.Vacancy, details *vacancyDetails, stats *passStats) (string, error) {
	// The prompt carries the slim vacancy summary; the full field set rides
	// in vacancy_details (raw JSON and the HTML description would duplicate
	// the dump and burn the token budget).
	summary := *v
	summary.Raw = nil
	summary.Description = ""
	raw, _, err := f.opt.Runner.Run(ctx, accountID, "vacancy",
		strconv.FormatInt(v.ID, 10), runID, ai.TaskCoverLetter, letterInput{
			Vacancy:           summary,
			Details:           details,
			ResumeDigest:      prof.ResumeDigest,
			ProfileText:       prof.ProfileText,
			Facts:             prof.Facts,
			Template:          tmpl,
			LetterMode:        mode,
			ExtraInstructions: extraInstructions,
		})
	if err != nil {
		return "", err
	}
	var lv struct {
		Letter     string `json:"letter"`
		NeedsHuman bool   `json:"needs_human"`
		Injection  bool   `json:"injection_suspected"`

		NeedsHumanReason string `json:"needs_human_reason"`
	}
	if jerr := json.Unmarshal(raw, &lv); jerr != nil {
		return "", jerr
	}
	if lv.NeedsHuman || lv.Injection || strings.TrimSpace(lv.Letter) == "" {
		if !lv.Injection && strings.TrimSpace(lv.Letter) != "" {
			// T86: hesitation without critique — accept the letter, keep
			// the model's reason visible in the log (no silent swallow).
			f.log.Warn("multiapply: cover letter hesitation ignored (formal requirement gaps do not block)",
				"vacancy", v.ID, "reason", lv.NeedsHumanReason)
			return strings.TrimSpace(lv.Letter), nil
		}
		f.review(ctx, accountID, "letter_needs_human",
			"cover letter verdict needs human or is empty",
			fmt.Sprintf(`{"vacancy_id":%d,"run":%q}`, v.ID, runID))
		// T69: the needs-human letter is a first-class console event.
		stats.letters++
		f.console.Letter(accountID, *v)
		return "", errLetterNeedsHuman
	}
	return strings.TrimSpace(lv.Letter), nil
}

type letterInput struct {
	Vacancy           hhapi.Vacancy     `json:"vacancy"`
	Details           *vacancyDetails   `json:"vacancy_details,omitempty"`
	ResumeDigest      string            `json:"resume_digest"`
	ProfileText       string            `json:"profile_text"`
	Facts             map[string]string `json:"facts"`
	Template          string            `json:"template,omitempty"`
	LetterMode        string            `json:"letter_mode,omitempty"` // T26: template | template_ai | ai_company
	ExtraInstructions string            `json:"extra_instructions,omitempty"`
}

// --- small helpers ---

// excluded implements F2: case-insensitive substring scan over the name
// (always) and the description (when ExcludeInDesc is set).
func excluded(v hhapi.Vacancy, flt config.RunFilters) bool {
	name := strings.ToLower(v.Name)
	desc := ""
	if flt.ExcludeInDesc {
		desc = strings.ToLower(v.Description)
	}
	for _, w := range flt.ExcludeWords {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if strings.Contains(name, w) || (desc != "" && strings.Contains(desc, w)) {
			return true
		}
	}
	return false
}

// matchEmployer matches a config list entry against the employer id
// (numeric string) or name (case-insensitive).
func matchEmployer(list []string, empID int64, empName string) bool {
	id := strconv.FormatInt(empID, 10)
	for _, entry := range list {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == id || strings.EqualFold(entry, empName) {
			return true
		}
	}
	return false
}

func bypassSet(entries []string) map[string]bool {
	m := make(map[string]bool, len(entries))
	for _, e := range entries {
		m[strings.TrimSpace(e)] = true
	}
	return m
}

// rawAcceptsAutoResponse reads acceptAutoResponse from the vacancy raw JSON
// (top-level or nested under autoResponse). found=false when the field is
// absent either way.
func rawAcceptsAutoResponse(raw json.RawMessage) (accepts, found bool) {
	if len(raw) == 0 {
		return true, false
	}
	var probe struct {
		AcceptAutoResponse *bool `json:"acceptAutoResponse"`
		AutoResponse       *struct {
			AcceptAutoResponse *bool `json:"acceptAutoResponse"`
		} `json:"autoResponse"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return true, false
	}
	switch {
	case probe.AcceptAutoResponse != nil:
		return *probe.AcceptAutoResponse, true
	case probe.AutoResponse != nil && probe.AutoResponse.AcceptAutoResponse != nil:
		return *probe.AutoResponse.AcceptAutoResponse, true
	default:
		return true, false
	}
}

// alreadyMarker detects hh's "you already responded" validation body.
func alreadyMarker(err error) bool {
	var se *hhapi.StatusError
	if !errors.As(err, &se) {
		return false
	}
	body := strings.ToLower(se.BodySnippet)
	return strings.Contains(body, "уже") || strings.Contains(body, "already")
}

// testRequiredMarker reports hh's structured test-required refusal on the
// apply POST (the T83 live form: {"error":"test-required"}).
func testRequiredMarker(err error) bool {
	var se *hhapi.StatusError
	if !errors.As(err, &se) {
		return false
	}
	return strings.Contains(strings.ToLower(se.BodySnippet), "test-required")
}

// countToday counts today's applications for account+run. Live mode counts
// applied rows only — a dry_run survey row never burns real quota. Dry-run
// counts today's survey rows too: the pass shows what WOULD have been sent
// today, so run.max_per_day caps the proposals (T68).
func (f *Feature) countToday(ctx context.Context, accountID, runID string) int {
	count := f.opt.DB.ApplicationsCountToday
	if f.opt.Config.General.DryRun {
		count = f.opt.DB.ApplicationsCountTodayInclDryRun
	}
	n, err := count(ctx, accountID, runID, f.dayUTC())
	if err != nil {
		f.log.Warn("multiapply: count today failed", "run", runID, "err", err)
		return 0
	}
	return n
}

// accountCountToday sums today's applications across the account's runs.
func (f *Feature) accountCountToday(ctx context.Context, accountID string) int {
	total := 0
	for id, run := range f.opt.Config.Runs {
		if run.Account != accountID {
			continue
		}
		total += f.countToday(ctx, accountID, id)
	}
	return total
}

func (f *Feature) accountCap(accountID string) int {
	if a, ok := f.opt.Config.Accounts[accountID]; ok && a.DailyApplyCap > 0 {
		return a.DailyApplyCap
	}
	return defaultAccountCap
}

// employerToday reads the per-employer daily counter (feature_state).
func (f *Feature) employerToday(ctx context.Context, accountID string, employerID int64) int {
	key := employerKey(accountID, employerID, f.dayUTC())
	v, ok, err := f.opt.DB.FeatureStateGet(ctx, feature, accountID, key)
	if err != nil || !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// bumpEmployer increments the per-employer daily counter after a recorded
// apply (ok, already-responded or reconciled all consumed employer quota).
func (f *Feature) bumpEmployer(ctx context.Context, accountID string, employerID int64) {
	key := employerKey(accountID, employerID, f.dayUTC())
	if err := f.opt.DB.FeatureStateSet(ctx, feature, accountID, key,
		strconv.Itoa(f.employerToday(ctx, accountID, employerID)+1)); err != nil {
		f.log.Warn("multiapply: employer counter write failed", "employer", employerID, "err", err)
	}
}

func employerKey(_ string, employerID int64, day string) string {
	return "emp:" + strconv.FormatInt(employerID, 10) + ":" + day
}

// --- captcha circuit breaker (Wave 3 review fix 3) ---

// captchaBreakerKey is the day-scoped feature_state flag: once set, further
// captcha challenges in the same task are skipped without Solve calls until
// the next day.
func captchaBreakerKey(accountID, day string) string {
	return "captcha:exhausted:" + accountID + ":" + day
}

// captchaExhausted reports whether the breaker is open for today.
func (f *Feature) captchaExhausted(ctx context.Context, accountID string) bool {
	v, ok, err := f.opt.DB.FeatureStateGet(ctx, feature, accountID, captchaBreakerKey(accountID, f.dayUTC()))
	return err == nil && ok && v == "1"
}

// tripCaptchaBreaker opens the breaker for today: persists the flag, adds a
// review item and emits EvCaptchaExhausted once.
func (f *Feature) tripCaptchaBreaker(ctx context.Context, accountID, reason string) {
	if err := f.opt.DB.FeatureStateSet(ctx, feature, accountID, captchaBreakerKey(accountID, f.dayUTC()), "1"); err != nil {
		f.log.Error("multiapply: breaker flag write failed", "account", accountID, "err", err)
	}
	f.review(ctx, accountID, "captcha_exhausted", reason, "{}")
	if f.opt.Notifier != nil {
		f.opt.Notifier.Emit(webhook.Event{
			Type:      webhook.EvCaptchaExhausted,
			AccountID: accountID,
			Time:      f.opt.Now(),
			Message:   "Цепочка капчи исчерпана на сегодня: " + reason,
			Payload:   map[string]any{"account_id": accountID, "reason": reason},
		})
	}
	f.log.Warn("multiapply: captcha breaker tripped", "account", accountID, "reason", reason)
}

// captchaChallenge builds the challenge handed to the provider chain; the
// exact hh challenge type is not parsed from the page, so the image type
// with the resume page URL is the conservative default.
func captchaChallenge() captcha.Challenge {
	return captcha.Challenge{Type: captcha.TypeImage, PageURL: "/applicant/resumes"}
}

// webhookEvent builds the apply_sent notification payload.
func webhookEvent(accountID, runID string, v hhapi.Vacancy) webhook.Event {
	return webhook.Event{
		Type:      webhook.EvApplySent,
		AccountID: accountID,
		Time:      time.Now(),
		Message:   fmt.Sprintf("Отклик отправлен: vacancy %d (%s)", v.ID, v.Name),
		Payload: map[string]any{
			"run_id":      runID,
			"vacancy_id":  v.ID,
			"employer_id": v.EmployerID,
		},
	}
}

// resolveResumeHash picks the run resume: the configured hash or the first
// published+searchable snapshot (refreshing the store when empty).
func (f *Feature) resolveResumeHash(ctx context.Context, accountID string, api hhapi.HHAPI, run config.Run) (string, error) {
	if run.ResumeID != "" {
		return run.ResumeID, nil
	}
	pick := func(rows []store.Resume) string {
		for _, r := range rows {
			if resumeLiftable(r) {
				return r.HhResumeID
			}
		}
		return ""
	}
	if err := f.opt.DB.AccountsUpsert(ctx, store.Account{ID: accountID}); err != nil {
		f.log.Warn("multiapply: account ensure failed", "account", accountID, "err", err)
	}
	rows, err := f.opt.DB.ResumesListByAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	if hash := pick(rows); hash != "" {
		return hash, nil
	}
	got, err := api.ListResumes(ctx)
	if err != nil {
		return "", err
	}
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
		if err := f.opt.DB.ResumesUpsert(ctx, &row); err != nil {
			return "", err
		}
	}
	rows, err = f.opt.DB.ResumesListByAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	if hash := pick(rows); hash != "" {
		return hash, nil
	}
	return "", errors.New("no published searchable resume")
}

// resumeLiftable mirrors the autolift published+searchable filter via the
// shared featurekit helper (Wave 3 review fix 6).
func resumeLiftable(r store.Resume) bool {
	return featurekit.ResumeLiftable(r.Status)
}

// loadProfile returns the injected profile or the default file/fact loader.
func (f *Feature) loadProfile(ctx context.Context, accountID string, api hhapi.HHAPI) (profile.Profile, error) {
	if f.opt.ProfileFor != nil {
		return f.opt.ProfileFor(ctx, accountID)
	}
	return profile.LoadUserContext(ctx, f.opt.Config, accountID, f.opt.DB, api)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// prefilter runs the seen write-through and gates F2-F5 (exclude words,
// employer lists, rating, recorded applications). Returns the first skip
// reason plus the whitelisted flag for the F6 threshold bypass.
func (f *Feature) prefilter(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, resumeHash string, v hhapi.Vacancy) (string, bool) {
	// Write-through the seen record (audit trail; not a skip condition —
	// quota-filtered vacancies must stay retryable on later days).
	_, _ = f.opt.DB.VacanciesSeenUpsert(ctx, store.VacancySeen{
		AccountID: accountID, VacancyID: v.ID, Source: "multiapply",
		FirstSeenAt: f.opt.Now(), Meta: runID,
	})

	// F2: exclude words (name always; description when enabled).
	if excluded(v, run.Filters) {
		return "exclude_words", false
	}

	// F3: blacklist / whitelist (by employer id and name).
	flt := run.Filters
	whitelisted := len(flt.CompanyWhitelist) > 0 && matchEmployer(flt.CompanyWhitelist, v.EmployerID, v.EmployerName)
	if len(flt.CompanyWhitelist) > 0 && !whitelisted {
		return "not_whitelisted", false
	}
	if matchEmployer(flt.CompanyBlacklist, v.EmployerID, v.EmployerName) {
		return "blacklisted", false
	}
	bypass := bypassSet(flt.WhitelistBypass)

	// F4: employer rating (skipped for whitelisted employers listing "F4").
	if (!whitelisted || !bypass[stageF4]) && flt.MinEmployerRating > 0 {
		pass, reason := emplrating.New(f.opt.DB, api, f.log).
			Pass(ctx, accountID, v.EmployerID, flt)
		if !pass {
			if reason == "no_reviews_human" || reason == "no_reviews" {
				return reason, whitelisted
			}
			return "rating_low", whitelisted
		}
	}

	// F5: dedup — recorded application (live mode ignores dry_run survey
	// rows: a survey never blocks a real apply; dry-run sees them, so a
	// repeated dry-run filters the already-surveyed vacancies out instead
	// of re-proposing them and tripping the UNIQUE dedup_key — T68), native
	// auto-responder.
	key := featurekit.DedupKey(accountID, resumeHash, v.ID)
	getByDedupKey := f.opt.DB.ApplicationsGetByDedupKeyLive
	if f.opt.Config.General.DryRun {
		getByDedupKey = f.opt.DB.ApplicationsGetByDedupKey
	}
	if app, err := getByDedupKey(ctx, key); err == nil && app != nil {
		return "already_applied", whitelisted
	}
	if f.opt.Config.Features.MultiApply.DedupNativeResp {
		if accepts, found := rawAcceptsAutoResponse(v.Raw); found && !accepts {
			return "native_autoresponse", whitelisted
		}
	}
	return "", whitelisted
}

// aiFilterSkip is gate F6: AI relevance with the whitelisted threshold
// bypass; needs_human and injection verdicts still gate. Every passing
// verdict returns its scored candidate for the T84 queue; gray marks the
// gray-band policy route (T75) so the apply record can mark its origin.
func (f *Feature) aiFilterSkip(ctx context.Context, runID string, run config.Run, accountID string,
	prof profile.Profile, resumeHash string, v hhapi.Vacancy, whitelisted bool, bypass map[string]bool,
) (string, *candidate) {
	// F6: AI relevance (threshold bypassed for whitelisted employers
	// listing "F6_threshold"; needs_human/injection still gate).
	// T71 fail-closed: ai_filter=true with an unavailable runner skips the
	// vacancy with a reason — an automation tool never applies unscored.
	if !f.opt.Config.Features.MultiApply.AIFilter {
		// No scoring configured: the candidate joins the queue with no
		// verdict and sorts to the unscored tail (T84).
		return "", &candidate{v: v}
	}
	if f.opt.Runner == nil {
		return "ai_unavailable", nil
	}
	threshold := run.AIFilterOverrides.MinScore
	if threshold <= 0 {
		threshold = f.opt.Config.Features.MultiApply.MinMatchPercent
	}
	if threshold <= 0 {
		threshold = defaultMinMatch
	}
	if whitelisted && bypass[stageF6Threshold] {
		threshold = 0
	}
	rules := relevance.Rules{
		ExcludeWords:      run.Filters.ExcludeWords,
		TargetRoles:       relevance.RolesFromFacts(prof),
		ExtraInstructions: run.AIFilterOverrides.ExtraInstructions,
	}
	vd, pass, reason := relevance.New(f.opt.Runner, f.opt.DB, accountID, runID, prof, rules, f.log,
		f.opt.Config.Features.MultiApply.GrayBand).
		Pass(ctx, v, resumeHash, threshold)
	// T69: the classification verdict line (score and apply/skip
	// outcome) — off by default, the [console].verdicts toggle turns
	// it on as an alternative to --log-level debug.
	f.console.Verdict(v, vd.Score, threshold, pass, reason)
	if !pass {
		return reason, nil
	}
	return "", &candidate{v: v, verdict: &vd, gray: reason == relevance.GrayBandReason}
}

// quotaCheck is gate F7: run, account and per-employer daily caps. A stop
// means the run is done for today; a skip only affects this vacancy.
func (f *Feature) quotaCheck(ctx context.Context, runID string, run config.Run, accountID string, v hhapi.Vacancy) (string, string) {
	// F7: daily quotas — cheap DB checks run BEFORE the expensive popup
	// request (Wave 3 review fix 1).
	if run.MaxPerDay > 0 && f.countToday(ctx, accountID, runID) >= run.MaxPerDay {
		return "run_quota", ""
	}
	if f.accountCountToday(ctx, accountID) >= f.accountCap(accountID) {
		return "account_cap", ""
	}
	if f.employerToday(ctx, accountID, v.EmployerID) >= perEmployerDailyCap {
		return "", "employer_daily_cap"
	}
	return "", ""
}

// letterIgnoreLanguage reports the T86 [features.multiapply]
// letter_ignore_language toggle (default true): LANGUAGE-type resume
// inconsistencies do not block the letter or the apply.
func (f *Feature) letterIgnoreLanguage() bool {
	v := f.opt.Config.Features.MultiApply.LetterIgnoreLanguage
	return v == nil || *v
}

// popupGate is F6.5: the apply-popup feasibility probe (logged-out stops
// the account; inconsistencies, already-applied, disabled chat skip).
// T80: a vacancy with tests additionally yields its fetched VacancyTest
// (nil when the test cannot be read — the caller then skips).
func (f *Feature) popupGate(ctx context.Context, accountID string, api hhapi.HHAPI,
	v hhapi.Vacancy) (string, string, bool, *hhapi.VacancyTest) {
	// F6.5: feasibility from the apply popup.
	meta, err := api.ApplyPopup(ctx, v.ID)
	if err != nil {
		if errors.Is(err, hhapi.ErrLoggedOut) {
			f.stopAccount(ctx, accountID, "apply_popup: "+err.Error())
			return stopLoggedOut, "", false, nil
		}
		if errors.Is(err, hhapi.ErrRateLimited) {
			return "", "hh_limit", false, nil
		}
		f.log.Warn("multiapply: popup failed", "vacancy", v.ID, "err", err)
		return "", "popup_failed", false, nil
	}
	// T86: a LANGUAGE-only gap never blocks (the owner consciously
	// answers past formal language screening); mixed sets keep the skip.
	langOnly := f.letterIgnoreLanguage() && featurekit.OnlyLanguageInconsistencies(meta.ResumeInconsistencies)
	if len(meta.ResumeInconsistencies) > 0 && !langOnly {
		f.log.Info("multiapply: resume inconsistencies", "vacancy", v.ID,
			"inconsistencies", strings.Join(meta.ResumeInconsistencies, "; "))
		return "", "inconsistencies", false, nil
	}
	if len(meta.ResumeInconsistencies) > 0 {
		f.log.Info("multiapply: LANGUAGE resume inconsistencies ignored",
			"vacancy", v.ID, "inconsistencies", strings.Join(meta.ResumeInconsistencies, "; "))
	}
	if meta.AlreadyApplied {
		// Live popup surfaces the structural alreadyApplied flag upfront
		// (responseStatus.alreadyApplied) — same skip as the dedup hit,
		// one request earlier.
		return "", "already_applied", false, nil
	}
	if meta.HasTests || meta.TestRequired {
		stop, skip, tests := f.fetchVacancyTests(ctx, accountID, api, v)
		if stop != "" || skip != "" {
			return stop, skip, false, nil
		}
		return "", "", meta.LetterRequired, tests
	}
	if meta.ChatWritePossibility == "DISABLED" {
		return "", "chat_disabled", false, nil
	}
	return "", "", meta.LetterRequired, nil
}

// fetchVacancyTests reads the vacancy's test for the solve step. A non-empty
// stop/skip reason means the vacancy is settled (the caller returns it
// verbatim); tests != nil means the solve may proceed. Anything unreadable
// (captcha has no solver — owner decision; contract deviations are
// fail-closed) becomes the T77 test_required skip.
func (f *Feature) fetchVacancyTests(ctx context.Context, accountID string, api hhapi.HHAPI,
	v hhapi.Vacancy) (string, string, *hhapi.VacancyTest) {
	tests, err := api.VacancyTests(ctx, v.ID)
	if err == nil {
		if len(tests.Tasks) == 0 {
			return "", "test_required", nil
		}
		return "", "", tests
	}
	if errors.Is(err, hhapi.ErrLoggedOut) {
		f.stopAccount(ctx, accountID, "vacancy_tests: "+err.Error())
		return stopLoggedOut, "", nil
	}
	if errors.Is(err, hhapi.ErrRateLimited) {
		return "", "hh_limit", nil
	}
	f.log.Warn("multiapply: vacancy test unavailable", "vacancy", v.ID, "err", err)
	return "", "test_required", nil
}

// prepareLetter builds the cover letter when the popup requires one, the
// run configures a template or the [features.multiapply.cover_letter]
// section asks for one; links in the letter are a hard skip. T60: whenever
// a letter is built the FULL vacancy card is fetched — its complete field
// set feeds the AI prompt and the template placeholders alike; a failed
// fetch skips the vacancy fail-closed (a letter without the full context
// contradicts the owner's requirement).
func (f *Feature) prepareLetter(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, v hhapi.Vacancy, _ string, letterRequired bool, stats *passStats,
) (string, string) {
	mode, tmpl := run.LetterMode, run.LetterTemplate
	if mode == "" {
		mode = config.LetterModeAICompany // pre-T60 default
	}
	if cl := f.opt.Config.Features.MultiApply.CoverLetter; cl.Enabled {
		switch cl.Mode {
		case config.CoverLetterModeOff:
			// Letters are never generated; a letter-required vacancy
			// cannot be applied to properly.
			if letterRequired {
				return "", "letter_off"
			}
			return "", ""
		case config.CoverLetterModeTemplate:
			mode = config.LetterModeTemplate
		default: // "" behaves as ai
			mode = config.LetterModeAICompany
		}
		if tmpl == "" {
			tmpl = cl.Template
		}
	}
	if !letterRequired && tmpl == "" {
		return "", ""
	}
	full, err := api.VacancyCard(ctx, v.ID)
	if err != nil {
		f.log.Warn("multiapply: vacancy card failed", "vacancy", v.ID, "err", err)
		return "", "vacancy_card_failed"
	}
	// T79: a still-empty card (no title or no publication date even after
	// the vacancyFull unwrap) cannot feed a letter — skip before spending
	// the AI call, fail-closed like the fetch failure above.
	if full.Name == "" || full.PublishedAt.IsZero() {
		f.log.Warn("multiapply: vacancy card empty", "vacancy", v.ID)
		return "", "vacancy_card_failed"
	}
	details := buildVacancyDetails(*full)
	letter, err := f.buildLetter(ctx, runID, mode, tmpl, run.AIFilterOverrides.ExtraInstructions,
		accountID, prof, full, details, stats)
	if err != nil {
		if letterRequired {
			return "", "letter_unavailable"
		}
		return "", ""
	}
	if letter == "" && letterRequired {
		return "", "letter_unavailable"
	}
	if letter != "" {
		if lerr := featurekit.NoLinksRule(letter); lerr != nil {
			f.review(ctx, accountID, "letter_links", lerr.Error(),
				fmt.Sprintf(`{"vacancy_id":%d,"run":%q}`, v.ID, runID))
			return "", "letter_links"
		}
	}
	return letter, ""
}

// dryRunApply records a dry_run survey application: no pacer, no intent,
// no mutation — the survey row is upgradeable in place later (Wave 3 fix 2).
func (f *Feature) dryRunApply(ctx context.Context, runID string, accountID string, resumeHash string,
	v hhapi.Vacancy, letter string, stats *passStats) bool {
	if !f.opt.Config.General.DryRun {
		return false
	}
	// T69: the console surface renders the dry-run line («Откликнулся бы»);
	// the structured record stays visible at debug level (one banner per
	// event — T52).
	stats.applied++
	f.console.Applied(true, accountID, resumeHash, v, grayNote(stats))
	f.log.Debug("multiapply: dry-run apply", "account", accountID, "vacancy", v.ID, "run", runID)
	app := store.Application{
		AccountID: accountID, RunID: runID, HhResumeID: resumeHash,
		VacancyID: v.ID, EmployerID: v.EmployerID,
		DedupKey: featurekit.DedupKey(accountID, resumeHash, v.ID),
		Status:   "dry_run",
		Letter:   strPtr(letter),
		// The feature clock stamps the survey time: the dry-run quota view
		// counts today's survey rows by created_at (T68).
		CreatedAt: f.opt.Now(),
	}
	if stats.gray != nil {
		// T75: the survey row carries the gray-band origin flag like a
		// live apply.
		app.FailReason = strPtr(grayBandFlag)
	}
	if err := f.opt.DB.ApplicationsCreate(ctx, &app); err != nil {
		f.log.Error("multiapply: dry-run application record failed", "err", err)
	}
	f.incr(ctx, "apply.dry_run")
	return true
}

// grayBandFlag is the applications.fail_reason marker (T75): the apply came
// from a gray-band verdict the gray-band policy routed to apply.
const grayBandFlag = "gray_band"

// grayNote renders the applied-line gray-band marker: "gray 50".
func grayNote(stats *passStats) string {
	if stats.gray == nil {
		return ""
	}
	return fmt.Sprintf("%s %d", stats.gray.Band, stats.gray.Score)
}

// sendWithCaptcha sends the apply once and, on a captcha challenge, honors
// the circuit breaker or solves and retries exactly once. skip is set when
// the vacancy is skipped without an error (captcha breaker open).
func (f *Feature) sendWithCaptcha(ctx context.Context, accountID string, api hhapi.HHAPI, v hhapi.Vacancy,
	resumeHash, letter string, applyTest *hhapi.ApplyTest, claim featurekit.IntentClaim) (string, error) {
	_, err := f.sendApply(ctx, api, v, resumeHash, letter, applyTest)
	if errors.Is(err, hhapi.ErrCaptcha) {
		if f.captchaExhausted(ctx, accountID) {
			return f.captchaBreakerSkip(ctx, claim), nil
		}
		if f.solveCaptchaOrBreak(ctx, accountID) {
			_, err = f.sendApply(ctx, api, v, resumeHash, letter, applyTest)
		}
	}
	return "", err
}

// captchaBreakerSkip abandons the apply intent (a later pass re-attacks the
// vacancy once the chain is healthy again) and reports the day-scoped skip.
func (f *Feature) captchaBreakerSkip(ctx context.Context, claim featurekit.IntentClaim) string {
	// Circuit breaker open: skip without another Solve call; the day-scoped
	// key resets tomorrow. Abandon so a later pass can re-attack the vacancy
	// once the chain is healthy again.
	if aerr := featurekit.AbandonIntent(ctx, f.opt.DB, claim); aerr != nil {
		f.log.Error("multiapply: intent abandon failed", "err", aerr)
	}
	return "captcha_exhausted"
}

// solveCaptchaOrBreak solves one captcha challenge via the account chain;
// a missing chain or failed solve trips the breaker and reports false.
func (f *Feature) solveCaptchaOrBreak(ctx context.Context, accountID string) bool {
	var chain captcha.Chain
	var ok bool
	if f.opt.ChainFor != nil {
		chain, ok = f.opt.ChainFor(accountID)
	}
	if !ok || chain == nil {
		f.tripCaptchaBreaker(ctx, accountID, "captcha challenge with no provider chain configured")
		return false
	}
	if _, serr := chain.Solve(ctx, captchaChallenge()); serr != nil {
		f.tripCaptchaBreaker(ctx, accountID, "captcha solve failed: "+serr.Error())
		return false
	}
	f.incr(ctx, "apply.captcha_retry")
	return true
}

// resolveAmbiguous settles an ambiguous apply by reading the negotiations:
// found → applied; not found → the single sanctioned retry; unreadable →
// park for a human. handled=true means applyOne returns cleanly.
func (f *Feature) resolveAmbiguous(ctx context.Context, runID string, accountID string, api hhapi.HHAPI,
	resumeHash string, v hhapi.Vacancy, letter string, applyTest *hhapi.ApplyTest, claim featurekit.IntentClaim, _ error, rc *cloneRunState,
	stats *passStats,
) (bool, error) {
	found, rerr := f.reconcileNegotiations(ctx, api, v.ID)
	switch {
	case rerr == nil && found:
		f.recordApplied(ctx, runID, accountID, resumeHash, v, claim, letter, "apply.reconciled", stats)
		return true, nil
	case rerr == nil && !found:
		// Server has no trace: abandon the intent and retry once —
		// this is the only sanctioned repeat.
		if aerr := featurekit.AbandonIntent(ctx, f.opt.DB, claim); aerr != nil {
			f.log.Error("multiapply: intent abandon failed", "err", aerr)
		}
		claim2, retryErr := f.retryApplyOnce(ctx, runID, accountID, api, v, resumeHash, letter, applyTest, rc)
		if retryErr == nil {
			metric := "apply.ok"
			if applyTest != nil {
				metric = "apply.test_solved"
			}
			f.recordApplied(ctx, runID, accountID, resumeHash, v, claim2, letter, metric, stats)
			return true, nil
		}
		// A second ambiguity inside the sanctioned retry is handled
		// symmetrically with the first: resolve by reading, never a
		// silent "failed" (Wave 3 review fix 7).
		var amb2 *hhapi.AmbiguousError
		if errors.As(retryErr, &amb2) {
			found2, rerr2 := f.reconcileNegotiations(ctx, api, v.ID)
			if rerr2 == nil && found2 {
				f.recordApplied(ctx, runID, accountID, resumeHash, v, claim2, letter, "apply.reconciled", stats)
				return true, nil
			}
			f.review(ctx, accountID, "apply_ambiguous",
				"second ambiguous outcome; negotiations empty or unreadable",
				fmt.Sprintf(`{"vacancy_id":%d,"run":%q}`, v.ID, runID))
			f.incr(ctx, "apply.ambiguous_review")
			return true, nil // intent stays pending: parked for a human
		}
		_ = featurekit.FinishIntent(ctx, f.opt.DB, claim2, retryErr)
		f.createApplication(ctx, runID, accountID, resumeHash, v, "failed", letter, retryErr.Error(), stats)
		f.incr(ctx, "apply.error")
		return true, nil
	default:
		// Undeterminable: park for a human; the intent stays pending so
		// the vacancy is not re-attacked automatically.
		f.review(ctx, accountID, "apply_ambiguous",
			"apply outcome unknown and negotiations unavailable: "+rerr.Error(),
			fmt.Sprintf(`{"vacancy_id":%d,"run":%q}`, v.ID, runID))
		f.incr(ctx, "apply.ambiguous_review")
		return true, nil
	}

}
