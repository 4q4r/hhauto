package multiapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// processVacancy runs the F2..F8 cascade for one vacancy.
// stop is a non-empty stop reason (profile ends; rescheduled unless it is
// stopLoggedOut); skip is a non-empty per-vacancy skip reason.
func (f *Feature) processVacancy(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, v hhapi.Vacancy, batch map[int64]bool,
) (stop, skip string) {
	// In-batch dedup: the same vacancy from two sources is processed once.
	if batch[v.ID] {
		return "", "dup_batch"
	}
	batch[v.ID] = true

	// F2-F5: static filters, rating and dedup (the seen row is written
	// through as audit trail, never as a skip condition).
	bypass := bypassSet(run.Filters.WhitelistBypass)
	skip, whitelisted := f.prefilter(ctx, runID, run, accountID, api, resumeHash, v)
	if skip != "" {
		return "", skip
	}

	// F6: AI relevance (threshold bypassed for whitelisted employers
	// listing "F6_threshold"; needs_human/injection still gate).
	if skip := f.aiFilterSkip(ctx, runID, run, accountID, prof, resumeHash, v, whitelisted, bypass); skip != "" {
		return "", skip
	}

	// F7: daily quotas — cheap DB checks run BEFORE the expensive popup
	// request (Wave 3 review fix 1).
	if stop, skip := f.quotaCheck(ctx, runID, run, accountID, v); stop != "" || skip != "" {
		return stop, skip
	}

	// F6.5: feasibility from the apply popup.
	stop, skip, letterRequired := f.popupGate(ctx, accountID, api, v)
	if stop != "" || skip != "" {
		return stop, skip
	}

	// F8: apply.
	return f.applyOne(ctx, runID, run, accountID, api, prof, resumeHash, v, letterRequired)
}

// applyOne performs the mutation with intent, letter, captcha retry and
// ambiguous reconcile.
func (f *Feature) applyOne(ctx context.Context, runID string, run config.Run, accountID string,
	api hhapi.HHAPI, prof profile.Profile, resumeHash string, v hhapi.Vacancy, letterRequired bool,
) (stop, skip string) {
	// Cover letter: required by the popup or configured by the run.
	letter, skip := f.prepareLetter(ctx, runID, run, accountID, prof, v, resumeHash, letterRequired)
	if skip != "" {
		return "", skip
	}

	if f.dryRunApply(ctx, runID, accountID, resumeHash, v, letter) {
		return "", ""
	}

	if f.opt.Pacer != nil {
		if err := f.opt.Pacer.Wait(ctx, accountID); err != nil {
			return "", "pacer_cancelled"
		}
	}

	claim, err := featurekit.BeginIntent(ctx, f.opt.DB,
		"apply:"+accountID+":"+strconv.FormatInt(v.ID, 10),
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

	skip, err = f.sendWithCaptcha(ctx, accountID, api, v, resumeHash, letter, claim)
	if skip != "" {
		return "", skip
	}
	if err == nil {
		f.recordApplied(ctx, runID, accountID, resumeHash, v, claim, letter, "apply.ok")
		return "", ""
	}

	// Already responded: hh's validation carries the marker — mark applied
	// silently (no webhook, no apply.ok metric).
	var se *hhapi.StatusError
	if errors.As(err, &se) && errors.Is(err, hhapi.ErrValidation) && alreadyMarker(err) {
		_ = featurekit.FinishIntent(ctx, f.opt.DB, claim, nil)
		f.createApplication(ctx, runID, accountID, resumeHash, v, "applied", letter, "")
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
		handled, _ := f.resolveAmbiguous(ctx, runID, accountID, api, resumeHash, v, letter, claim, err)
		if handled {
			return "", ""
		}
	}

	// Plain failure.
	_ = featurekit.FinishIntent(ctx, f.opt.DB, claim, err)
	f.createApplication(ctx, runID, accountID, resumeHash, v, "failed", letter, err.Error())
	f.incr(ctx, "apply.error")
	f.log.Warn("multiapply: apply failed", "vacancy", v.ID, "err", err)
	return "", ""
}

// sendApply performs one Apply round trip.
func (f *Feature) sendApply(ctx context.Context, api hhapi.HHAPI, v hhapi.Vacancy,
	resumeHash, letter string,
) (hhapi.NegotiationRef, error) {
	return api.Apply(ctx, hhapi.ApplyRequest{VacancyID: v.ID, ResumeHash: resumeHash, Letter: letter})
}

// retryApplyOnce re-arms the key and repeats the apply a single time.
func (f *Feature) retryApplyOnce(ctx context.Context, runID, accountID string, api hhapi.HHAPI,
	v hhapi.Vacancy, resumeHash, letter string,
) (featurekit.IntentClaim, error) {
	claim, err := featurekit.BeginIntent(ctx, f.opt.DB,
		"apply:"+accountID+":"+strconv.FormatInt(v.ID, 10),
		featurekit.IntentPayload(RunPayload{RunID: runID}))
	if err != nil {
		return claim, fmt.Errorf("multiapply: re-arm intent: %w", err)
	}
	if claim.Step != featurekit.IntentProceed {
		return claim, fmt.Errorf("multiapply: re-arm intent blocked (step %d)", claim.Step)
	}
	ref, err := f.sendApply(ctx, api, v, resumeHash, letter)
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
	v hhapi.Vacancy, claim featurekit.IntentClaim, letter, metric string,
) {
	if err := featurekit.FinishIntent(ctx, f.opt.DB, claim, nil); err != nil {
		f.log.Error("multiapply: intent mark failed", "err", err)
	}
	f.createApplication(ctx, runID, accountID, resumeHash, v, "applied", letter, "")
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
	v hhapi.Vacancy, status, letter, failReason string,
) {
	appKey := featurekit.DedupKey(accountID, resumeHash, v.ID)
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

// buildLetter produces the outgoing cover letter through the AI runner
// (TaskCoverLetter), falling back to the run's static template; the result
// still passes through NoLinksRule at the call site.
func (f *Feature) buildLetter(ctx context.Context, runID string, run config.Run, accountID string,
	prof profile.Profile, v hhapi.Vacancy, _ string,
) (string, error) {
	if f.opt.Runner != nil {
		letter, aerr := f.aiLetter(ctx, runID, run, accountID, prof, v)
		if aerr == nil {
			return letter, nil
		}
		f.log.Warn("multiapply: cover letter task failed", "vacancy", v.ID, "err", aerr)
	}
	if run.LetterTemplate != "" {
		return run.LetterTemplate, nil
	}
	return "", errors.New("multiapply: no cover letter available")
}

// aiLetter asks the runner for the cover-letter verdict; needs_human,
// injection-flagged or empty letters are refused (errLetterNeedsHuman).
func (f *Feature) aiLetter(ctx context.Context, runID string, run config.Run, accountID string,
	prof profile.Profile, v hhapi.Vacancy) (string, error) {
	raw, _, err := f.opt.Runner.Run(ctx, accountID, "vacancy",
		strconv.FormatInt(v.ID, 10), runID, ai.TaskCoverLetter, letterInput{
			Vacancy:           v,
			ResumeDigest:      prof.ResumeDigest,
			ProfileText:       prof.ProfileText,
			Facts:             prof.Facts,
			Template:          run.LetterTemplate,
			ExtraInstructions: run.AIFilterOverrides.ExtraInstructions,
		})
	if err != nil {
		return "", err
	}
	var lv struct {
		Letter     string `json:"letter"`
		NeedsHuman bool   `json:"needs_human"`
		Injection  bool   `json:"injection_suspected"`
	}
	if jerr := json.Unmarshal(raw, &lv); jerr != nil {
		return "", jerr
	}
	if lv.NeedsHuman || lv.Injection || strings.TrimSpace(lv.Letter) == "" {
		f.review(ctx, accountID, "letter_needs_human",
			"cover letter verdict needs human or is empty",
			fmt.Sprintf(`{"vacancy_id":%d,"run":%q}`, v.ID, runID))
		return "", errLetterNeedsHuman
	}
	return strings.TrimSpace(lv.Letter), nil
}

type letterInput struct {
	Vacancy           hhapi.Vacancy     `json:"vacancy"`
	ResumeDigest      string            `json:"resume_digest"`
	ProfileText       string            `json:"profile_text"`
	Facts             map[string]string `json:"facts"`
	Template          string            `json:"template,omitempty"`
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

// countToday counts today's applied applications for account+run.
func (f *Feature) countToday(ctx context.Context, accountID, runID string) int {
	n, err := f.opt.DB.ApplicationsCountToday(ctx, accountID, runID, f.dayUTC())
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

	// F5: dedup — recorded application (dry_run survey rows never block a
	// live apply), native auto-responder.
	key := featurekit.DedupKey(accountID, resumeHash, v.ID)
	if app, err := f.opt.DB.ApplicationsGetByDedupKeyLive(ctx, key); err == nil && app != nil {
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
// bypass; needs_human and injection verdicts still gate.
func (f *Feature) aiFilterSkip(ctx context.Context, runID string, run config.Run, accountID string,
	prof profile.Profile, resumeHash string, v hhapi.Vacancy, whitelisted bool, bypass map[string]bool) string {
	// F6: AI relevance (threshold bypassed for whitelisted employers
	// listing "F6_threshold"; needs_human/injection still gate).
	if f.opt.Config.Features.MultiApply.AIFilter && f.opt.Runner != nil {
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
		_, pass, reason := relevance.New(f.opt.Runner, f.opt.DB, accountID, runID, prof, rules, f.log).
			Pass(ctx, v, resumeHash, threshold)
		if !pass {
			return reason
		}
	}
	return ""
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

// popupGate is F6.5: the apply-popup feasibility probe (logged-out stops
// the account; inconsistencies, already-applied, disabled chat skip).
func (f *Feature) popupGate(ctx context.Context, accountID string, api hhapi.HHAPI,
	v hhapi.Vacancy) (string, string, bool) {
	// F6.5: feasibility from the apply popup.
	meta, err := api.ApplyPopup(ctx, v.ID)
	if err != nil {
		if errors.Is(err, hhapi.ErrLoggedOut) {
			f.stopAccount(ctx, accountID, "apply_popup: "+err.Error())
			return stopLoggedOut, "", false
		}
		if errors.Is(err, hhapi.ErrRateLimited) {
			return "", "hh_limit", false
		}
		f.log.Warn("multiapply: popup failed", "vacancy", v.ID, "err", err)
		return "", "popup_failed", false
	}
	if len(meta.ResumeInconsistencies) > 0 {
		f.log.Info("multiapply: resume inconsistencies", "vacancy", v.ID,
			"inconsistencies", strings.Join(meta.ResumeInconsistencies, "; "))
		return "", "inconsistencies", false
	}
	if meta.AlreadyApplied {
		// Live popup surfaces the structural alreadyApplied flag upfront
		// (responseStatus.alreadyApplied) — same skip as the dedup hit,
		// one request earlier.
		return "", "already_applied", false
	}
	if meta.ChatWritePossibility == "DISABLED" {
		return "", "chat_disabled", false
	}
	return "", "", meta.LetterRequired
}

// prepareLetter builds the cover letter when the popup requires one or the
// run configures a template; links in the letter are a hard skip.
func (f *Feature) prepareLetter(ctx context.Context, runID string, run config.Run, accountID string,
	prof profile.Profile, v hhapi.Vacancy, resumeHash string, letterRequired bool) (string, string) {
	if !letterRequired && run.LetterTemplate == "" {
		return "", ""
	}
	letter, err := f.buildLetter(ctx, runID, run, accountID, prof, v, resumeHash)
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
	v hhapi.Vacancy, letter string) bool {
	if !f.opt.Config.General.DryRun {
		return false
	}
	f.log.Info("multiapply: dry-run apply", "account", accountID, "vacancy", v.ID, "run", runID)
	if err := f.opt.DB.ApplicationsCreate(ctx, &store.Application{
		AccountID: accountID, RunID: runID, HhResumeID: resumeHash,
		VacancyID: v.ID, EmployerID: v.EmployerID,
		DedupKey: featurekit.DedupKey(accountID, resumeHash, v.ID),
		Status:   "dry_run",
		Letter:   strPtr(letter),
	}); err != nil {
		f.log.Error("multiapply: dry-run application record failed", "err", err)
	}
	f.incr(ctx, "apply.dry_run")
	return true
}

// sendWithCaptcha sends the apply once and, on a captcha challenge, honors
// the circuit breaker or solves and retries exactly once. skip is set when
// the vacancy is skipped without an error (captcha breaker open).
func (f *Feature) sendWithCaptcha(ctx context.Context, accountID string, api hhapi.HHAPI, v hhapi.Vacancy,
	resumeHash, letter string, claim featurekit.IntentClaim) (string, error) {
	_, err := f.sendApply(ctx, api, v, resumeHash, letter)
	if errors.Is(err, hhapi.ErrCaptcha) {
		if f.captchaExhausted(ctx, accountID) {
			return f.captchaBreakerSkip(ctx, claim), nil
		}
		if f.solveCaptchaOrBreak(ctx, accountID) {
			_, err = f.sendApply(ctx, api, v, resumeHash, letter)
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
	resumeHash string, v hhapi.Vacancy, letter string, claim featurekit.IntentClaim, _ error) (bool, error) {
	found, rerr := f.reconcileNegotiations(ctx, api, v.ID)
	switch {
	case rerr == nil && found:
		f.recordApplied(ctx, runID, accountID, resumeHash, v, claim, letter, "apply.reconciled")
		return true, nil
	case rerr == nil && !found:
		// Server has no trace: abandon the intent and retry once —
		// this is the only sanctioned repeat.
		if aerr := featurekit.AbandonIntent(ctx, f.opt.DB, claim); aerr != nil {
			f.log.Error("multiapply: intent abandon failed", "err", aerr)
		}
		claim2, retryErr := f.retryApplyOnce(ctx, runID, accountID, api, v, resumeHash, letter)
		if retryErr == nil {
			f.recordApplied(ctx, runID, accountID, resumeHash, v, claim2, letter, "apply.ok")
			return true, nil
		}
		// A second ambiguity inside the sanctioned retry is handled
		// symmetrically with the first: resolve by reading, never a
		// silent "failed" (Wave 3 review fix 7).
		var amb2 *hhapi.AmbiguousError
		if errors.As(retryErr, &amb2) {
			found2, rerr2 := f.reconcileNegotiations(ctx, api, v.ID)
			if rerr2 == nil && found2 {
				f.recordApplied(ctx, runID, accountID, resumeHash, v, claim2, letter, "apply.reconciled")
				return true, nil
			}
			f.review(ctx, accountID, "apply_ambiguous",
				"second ambiguous outcome; negotiations empty or unreadable",
				fmt.Sprintf(`{"vacancy_id":%d,"run":%q}`, v.ID, runID))
			f.incr(ctx, "apply.ambiguous_review")
			return true, nil // intent stays pending: parked for a human
		}
		_ = featurekit.FinishIntent(ctx, f.opt.DB, claim2, retryErr)
		f.createApplication(ctx, runID, accountID, resumeHash, v, "failed", letter, retryErr.Error())
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
