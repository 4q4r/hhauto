// editor_runner.go implements T30 — the browser (chromedp) apply of the
// T29 improve plan. The hh.ru /profile/** POST family is edge-gated for
// off-browser TLS clients (T29 live probes, 2026-09-25), so the planned
// values are applied through the REAL editor pages: browser TLS passes the
// edge and the page itself issues the correct XHRs.
//
//	CONTEXT ISOLATION INVARIANT: the account session rides the regular
//	cookie store (internal/cookies) and is injected into a THROWAWAY
//	user-data-dir browser profile immediately before navigation, scoped to
//	the hh.ru domain (config cookie_domain) and nothing else. The profile
//	is deleted when the run ends; no third party ever sees the session.
//	No proxy is used: the editor walks from the machine's own IP — the
//	challenge was already passed by the bootstrap session.
//
// Flow per ApplyFieldPlan: resolve the clone's account through the local
// lineage (hh clients are per-account; the plan is addressed by clone
// hash) → isolated profile + session cookies → per editor page (in
// first-seen plan order): open the page URL (default
// {base}/resume/edit/{resume}/{segment} — live-verified routes:
// position, about, experience/{item}; config-overridable per screen) →
// fill the planned controls in-page (native value setter +
// input/change/focusout events for React state; data-qa selector chains
// with a label-text fallback, testsolve-style) → click the save control
// and give the wizard a bounded window to leave the page
// (nextIncompleteScreenId shift) → the authoritative gate is the
// get_by_hash read-back through the regular hhapi client (double control:
// the browser's word is never trusted alone).
//
// Per-change failures (selector not found, verify mismatch) degrade to
// Failed entries; only invariant/read failures (not a tracked clone,
// cookies unavailable, Chrome missing) error the whole call — the caller
// parks the plan for manual apply in that case.

package autoclone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"hhauto/internal/browser"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// ErrEditorUnavailable reports a degradation-class browser failure (Chrome
// missing, cookies unavailable, browser start failure): the plan did not
// even reach the page, so the caller parks it for manual apply instead of
// reporting per-field errors.
var ErrEditorUnavailable = errors.New("autoclone: browser editor unavailable")

// FieldChange is one planned field mutation for the browser editor runner
// (the EditorRunner projection of the improve pipeline's pending target).
type FieldChange struct {
	// Field is the field spec id ("title", "key_skills", ...).
	Field string
	// Locator is the unique payload path of the target
	// ("resume.title", "profile.experience.0.description").
	Locator string
	// Screen is the wizard screen that persists the field.
	Screen string
	// Kind is the value shape (drives the DOM actions).
	Kind FieldKind
	// Label is the human field name (RU) backing the selector
	// label-heuristic fallback.
	Label string
	// Item is the experience item index (-1 for non-item fields).
	Item int
	// Value is the natural DOM value: JSON string | []string | number |
	// {"startDate","endDate"} | "YYYY-MM-DD" (birthday).
	Value json.RawMessage
}

// FieldApplyError is one change that did not land (Field carries the
// locator, so per-item targets stay distinguishable).
type FieldApplyError struct {
	Field string
	Err   string
}

// ApplyResult reports which changes landed (locators, in plan order) and
// which failed with a reason.
type ApplyResult struct {
	Applied []string
	Failed  []FieldApplyError
}

// EditorRunner applies a whole field plan to one clone through the real
// editor pages. The chromedp implementation is ChromeEditorRunner; tests
// and the pipeline inject fakes.
type EditorRunner interface {
	ApplyFieldPlan(ctx context.Context, cloneHash string, plan []FieldChange) (ApplyResult, error)
}

// EditorCookieSource supplies the account's hh.ru session cookies for the
// editor browser. The CLI wiring implements it over cookies.Store — the
// decrypted session never leaves the process except into the browser jar.
type EditorCookieSource interface {
	EditorCookies(ctx context.Context, accountID string) ([]*http.Cookie, error)
}

// ContentReader is the read-back surface the runner verifies through
// (the regular get_by_hash shard via hhapi).
type ContentReader interface {
	ResumeContent(ctx context.Context, hash string) (*hhapi.ResumeContent, error)
}

// BrowserFactory starts an authenticated browser session for one plan run.
// The returned cleanup stops the browser and removes the profile dir.
// nil in EditorOptions means the real chromedp factory.
type BrowserFactory func(ctx context.Context, cookies []*http.Cookie) (browserRunner, func(), error)

// browserRunner is the per-screen browser seam: open url, apply the fill
// ops, save the screen. Test doubles replace the chromedp implementation.
type browserRunner interface {
	runScreen(ctx context.Context, url string, ops []editorFillOp) ([]editorOpOutcome, error)
}

// EditorOptions wires the ChromeEditorRunner (fail-fast validation).
type EditorOptions struct {
	Cfg     config.AutoCloneImproveEditor
	DB      *store.DB // lineage: clone hash → account
	Cookies EditorCookieSource
	Verify  ContentReader
	// AccountID pins the owning account (T35): when set, ApplyFieldPlan
	// skips the lineage resolution entirely — the resume-improve command
	// drives BASE resumes through the editor, which have no lineage row.
	// Empty keeps the strict clone invariant.
	AccountID string
	// Browser overrides the chromedp session factory (tests). nil = real.
	Browser BrowserFactory
	// ChromeProvider resolves the executable through the shared browser
	// subsystem (T32: HHAUTO_CHROME_PATH → PATH → cloakbrowser cache →
	// auto-download). nil = the legacy findEditorChrome chain (прежнее
	// поведение). Used by the real factory only.
	ChromeProvider browser.Provider
	// BrowserCfg carries the [browser] section: the careful-preset stealth
	// launch flags and the humanize pacing/typing cadence.
	BrowserCfg config.Browser
	// SpecUserAgent pins the lane's identity to the captured spec UA
	// (T82: the spec is the single source of truth). Empty = the
	// browser's own identity stands.
	SpecUserAgent string
	Log           *slog.Logger
}

// ChromeEditorRunner is the chromedp-backed EditorRunner.
type ChromeEditorRunner struct {
	opt     EditorOptions
	browser BrowserFactory
	log     *slog.Logger
}

var _ EditorRunner = (*ChromeEditorRunner)(nil)

// NewChromeEditorRunner validates the wiring (fail fast: a broken editor
// section must stop the process, not degrade per clone).
func NewChromeEditorRunner(o EditorOptions) (*ChromeEditorRunner, error) {
	if o.DB == nil {
		return nil, errors.New("autoclone: editor runner requires the store (lineage invariant)")
	}
	if o.Cookies == nil {
		return nil, errors.New("autoclone: editor runner requires the cookie source")
	}
	if o.Verify == nil {
		return nil, errors.New("autoclone: editor runner requires the content verifier")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	runner := &ChromeEditorRunner{opt: o, log: o.Log}
	if o.Browser != nil {
		runner.browser = o.Browser
	} else {
		runner.browser = runner.startChrome
	}
	return runner, nil
}

// ApplyFieldPlan implements EditorRunner. See the file head for the flow.
func (r *ChromeEditorRunner) ApplyFieldPlan(ctx context.Context, cloneHash string, plan []FieldChange) (ApplyResult, error) {
	if len(plan) == 0 {
		return ApplyResult{}, nil
	}
	var (
		accountID string
		err       error
	)
	if r.opt.AccountID != "" {
		accountID = r.opt.AccountID // T35 pinned: base resumes have no lineage
	} else {
		accountID, err = r.accountForClone(ctx, cloneHash)
		if err != nil {
			return ApplyResult{}, err
		}
	}
	ck, err := r.opt.Cookies.EditorCookies(ctx, accountID)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: session cookies: %w", ErrEditorUnavailable, err)
	}
	if len(ck) == 0 {
		return ApplyResult{}, fmt.Errorf("%w: no session cookies for account %q", ErrEditorUnavailable, accountID)
	}

	buildable, failed := splitBuildable(plan)
	failedByLocator := map[string]string{}
	for _, e := range failed {
		failedByLocator[e.Field] = e.Err
	}

	if len(buildable) > 0 {
		browser, cleanup, err := r.browser(ctx, ck)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("%w: %w", ErrEditorUnavailable, err)
		}
		defer cleanup()
		r.applyScreens(ctx, browser, cloneHash, buildable, failedByLocator)
	}
	// Double control: the get_by_hash read-back decides what actually
	// landed, whatever the browser reported.
	live, err := r.opt.Verify.ResumeContent(ctx, cloneHash)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("editor verify read: %w", err)
	}
	res := ApplyResult{}
	for _, fc := range plan {
		if reason, bad := failedByLocator[fc.Locator]; bad {
			res.Failed = append(res.Failed, FieldApplyError{Field: fc.Locator, Err: reason})
			continue
		}
		if editorChangeVerified(live, fc) {
			res.Applied = append(res.Applied, fc.Locator)
			continue
		}
		res.Failed = append(res.Failed, FieldApplyError{Field: fc.Locator,
			Err: "verify mismatch: the value did not land on the resume"})
	}
	return res, nil
}

// accountForClone resolves the clone's owning account through the local
// lineage — the same invariant the improver enforces before planning.
func (r *ChromeEditorRunner) accountForClone(ctx context.Context, cloneHash string) (string, error) {
	row, err := r.opt.DB.ResumeClonesGet(ctx, cloneHash)
	if err != nil {
		return "", fmt.Errorf("editor runner: lineage lookup: %w", err)
	}
	if row == nil || row.AccountID == "" {
		return "", fmt.Errorf("%w: hash %q", ErrNotClone, cloneHash)
	}
	return row.AccountID, nil
}

// applyScreens walks the plan's editor pages through the browser: one
// visit per resolved URL (first-seen order), per-page failures recorded
// by locator.
func (r *ChromeEditorRunner) applyScreens(ctx context.Context, browser browserRunner, cloneHash string, buildable []FieldChange, failedByLocator map[string]string) {
	groups, gerrs := groupPlanByURL(r.opt.Cfg.BaseURL, r.opt.Cfg.ScreenURLs, cloneHash, buildable)
	for _, e := range gerrs {
		failedByLocator[e.Field] = e.Err
	}
	for _, g := range groups {
		ops, oerrs := buildScreenOps(g.Changes)
		for _, e := range oerrs {
			failedByLocator[e.Field] = e.Err
		}
		if len(ops) == 0 {
			continue
		}
		outcomes, err := browser.runScreen(ctx, g.URL, ops)
		if err != nil {
			r.log.Warn("autoclone: editor page failed", "url", g.URL, "err", err)
			for _, fc := range g.Changes {
				failedByLocator[fc.Locator] = err.Error()
			}
			continue
		}
		mergeOpOutcomes(outcomes, failedByLocator)
	}
}

func mergeOpOutcomes(outcomes []editorOpOutcome, failedByLocator map[string]string) {
	for _, o := range outcomes {
		if o.OK || o.Field == "" {
			continue
		}
		reason := o.Err
		if reason == "" {
			reason = "the control was not found on the page"
		}
		failedByLocator[o.Field] = reason
	}
}

// splitBuildable filters the changes the runner cannot even attempt
// (dictionary-backed structured fields).
func splitBuildable(plan []FieldChange) (buildable []FieldChange, failed []FieldApplyError) {
	for _, fc := range plan {
		if fc.Kind == KindStructured {
			failed = append(failed, FieldApplyError{Field: fc.Locator,
				Err: "structured fields are not browser-fillable (dictionary widgets stay out of the runner contract)"})
			continue
		}
		buildable = append(buildable, fc)
	}
	return buildable, failed
}

// --- plan → screens → DOM ops (pure, unit-tested) ---------------------------

// urlGroup is the per-page batch: one visit fills every change of the
// group and saves once.
type urlGroup struct {
	URL     string
	Changes []FieldChange
}

// groupPlanByURL clusters the plan by its resolved editor page URL,
// preserving the first-seen order (the wizard walk follows the plan).
// A change whose URL cannot be built (broken override template) comes
// back as a failed entry instead of failing the whole run.
func groupPlanByURL(base string, overrides map[string]string, resumeHash string, plan []FieldChange) (groups []urlGroup, errs []FieldApplyError) {
	index := map[string]int{}
	for _, fc := range plan {
		url, err := editorScreenURL(base, overrides, resumeHash, fc)
		if err != nil {
			errs = append(errs, FieldApplyError{Field: fc.Locator, Err: err.Error()})
			continue
		}
		i, ok := index[url]
		if !ok {
			i = len(groups)
			index[url] = i
			groups = append(groups, urlGroup{URL: url})
		}
		groups[i].Changes = append(groups[i].Changes, fc)
	}
	return groups, errs
}

// editorScreenURL builds the wizard page URL for one change. The default
// (live-verified 2026-09-26, chromium probe) pattern is
// {base}/resume/edit/{resume}/{segment} where the segment is the real
// editor route of the field (title/salary → position, about → about,
// experience components → experience/{item}); fields without a verified
// segment fall back to the model screen name. A per-screen override in
// cfg.screen_urls replaces the whole template: it must carry {resume},
// and {item} when the screen hosts per-item fields.
func editorScreenURL(base string, overrides map[string]string, resumeHash string, fc FieldChange) (string, error) {
	if t, ok := overrides[fc.Screen]; ok && strings.TrimSpace(t) != "" {
		tmpl := t
		if fc.Item >= 0 && !strings.Contains(tmpl, "{item}") && itemScreens()[fc.Screen] {
			return "", fmt.Errorf("editor url for screen %q: this screen hosts per-item fields, the override template must carry the {item} placeholder", fc.Screen)
		}
		if !strings.Contains(tmpl, "{resume}") {
			return "", fmt.Errorf("editor url for screen %q: template must carry the {resume} placeholder", fc.Screen)
		}
		if strings.Contains(tmpl, "{item}") && fc.Item < 0 {
			return "", fmt.Errorf("editor url for screen %q: template requires {item} but the change carries no item", fc.Screen)
		}
		return strings.NewReplacer(
			"{resume}", resumeHash,
			"{screen}", fc.Screen,
			"{item}", strconv.Itoa(fc.Item),
		).Replace(tmpl), nil
	}
	return strings.TrimSuffix(base, "/") + "/resume/edit/" + resumeHash + "/" + editorSegment(fc), nil
}

// editorSegment maps a field change onto the live editor's route segment.
// The segments were verified against the deployed editor (position/about
// render the form, experience/{item} addresses one item); unknown screens
// ride the model screen name — the owner can pin them via screen_urls.
func editorSegment(fc FieldChange) string {
	switch fc.Field {
	case "title", "salary":
		return "position"
	case "about":
		return "about"
	case "experience_company", "experience_position", "experience_description", "experience_dates":
		if fc.Item >= 0 {
			return "experience/" + strconv.Itoa(fc.Item)
		}
		return "experience"
	default:
		return strings.ToLower(fc.Screen)
	}
}

// itemScreens derives the wizard screens hosting per-item fields from the
// field model (the experience components). The config layer keeps a
// hardcoded copy of the same names: config must stay free of feature
// imports — keep the two lists in sync.
func itemScreens() map[string]bool {
	m := map[string]bool{}
	for _, s := range fieldSpecs {
		if s.ExpPart == "" {
			continue
		}
		for _, sc := range s.Screens {
			m[sc] = true
		}
	}
	return m
}

// editorFillOp is one DOM mutation: set the first resolvable control of
// the selector chain to Value (React-safe) and fire the state events.
type editorFillOp struct {
	Field     string   `json:"field"`           // locator (outcome reporting)
	Part      string   `json:"part,omitempty"`  // "" | start | end (date pair)
	Label     string   `json:"label,omitempty"` // label-text fallback anchor
	Selectors []string `json:"selectors"`       // data-qa / name chain
	Value     string   `json:"value,omitempty"` // the text to set
}

// editorOpOutcome reports one op's resolution for the result mapping.
type editorOpOutcome struct {
	Field    string `json:"field"`
	OK       bool   `json:"ok"`
	Selector string `json:"selector,omitempty"`
	Err      string `json:"err,omitempty"`
}

// buildScreenOps maps a screen's changes onto fill ops. Structured
// (dictionary-backed) fields are rejected with an explicit reason: the
// runner never guesses dictionary widgets.
func buildScreenOps(changes []FieldChange) ([]editorFillOp, []FieldApplyError) {
	var ops []editorFillOp
	var errs []FieldApplyError
	for _, fc := range changes {
		switch fc.Kind {
		case KindText:
			ops = append(ops, editorFillOp{Field: fc.Locator, Label: fc.Label,
				Selectors: editorSelectors(fc, ""), Value: editorStringValue(fc.Value)})
		case KindTextList:
			ops = append(ops, editorFillOp{Field: fc.Locator, Label: fc.Label,
				Selectors: editorSelectors(fc, ""), Value: editorListValue(fc.Value)})
		case KindSalary:
			ops = append(ops, editorFillOp{Field: fc.Locator, Label: fc.Label,
				Selectors: editorSelectors(fc, ""), Value: editorAmountValue(fc.Value)})
		case KindDate:
			if fc.Item >= 0 {
				start, end := editorDatePair(fc.Value)
				ops = append(ops,
					editorFillOp{Field: fc.Locator, Part: "start", Label: fc.Label,
						Selectors: editorSelectors(fc, "start"), Value: start},
					editorFillOp{Field: fc.Locator, Part: "end", Label: fc.Label,
						Selectors: editorSelectors(fc, "end"), Value: end})
				continue
			}
			ops = append(ops, editorFillOp{Field: fc.Locator, Label: fc.Label,
				Selectors: editorSelectors(fc, ""), Value: editorBirthdayValue(fc.Value)})
		default:
			errs = append(errs, FieldApplyError{Field: fc.Locator,
				Err: "structured fields are not browser-fillable (dictionary widgets stay out of the runner contract)"})
		}
	}
	return ops, errs
}

// editorSelectors builds the fallback chain for one control. The chain
// leads with the live editor's data-qa anchors (verified 2026-09-26:
// resume-edit-title-suggest, resume-salary-amount, resume-editor-about,
// resume-editor-experience-*-input), then falls back to generic
// field-id data-qa and name attributes; the in-page label heuristic is
// the last resort.
func editorSelectors(fc FieldChange, part string) []string {
	var primary []string
	switch fc.Field {
	case "title":
		primary = []string{`[data-qa="resume-edit-title-suggest"]`}
	case "about":
		primary = []string{`[data-qa="resume-editor-about"]`}
	case "salary":
		primary = []string{`[data-qa="resume-salary-amount"]`}
	case "experience_company":
		primary = []string{`[data-qa="resume-editor-experience-company-input"]`}
	case "experience_position":
		primary = []string{`[data-qa="resume-editor-experience-position-input"]`}
	case "experience_description":
		primary = []string{`[data-qa="resume-editor-experience-description-input"]`}
	case "experience_dates":
		switch part {
		case "start":
			primary = []string{`[data-qa="resume-editor-experience-start-year-input"]`}
		case "end":
			primary = []string{`[data-qa="resume-editor-experience-end-year-input"]`}
		}
	}
	switch {
	case part != "":
		return append(primary,
			fmt.Sprintf(`[data-qa="%s-%d-%s"]`, fc.Field, fc.Item, part),
			fmt.Sprintf(`[data-qa="%s-%s"]`, fc.Field, part),
			fmt.Sprintf(`input[name="%s-%s"]`, fc.Field, part))
	case fc.Item >= 0:
		return append(primary,
			fmt.Sprintf(`[data-qa="%s-%d"]`, fc.Field, fc.Item),
			fmt.Sprintf(`[data-qa="%s"]`, fc.Field))
	default:
		return append(primary,
			fmt.Sprintf(`[data-qa="%s"]`, fc.Field),
			fmt.Sprintf(`input[name="%s"]`, fc.Field),
			fmt.Sprintf(`textarea[name="%s"]`, fc.Field))
	}
}

func editorStringValue(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func editorListValue(raw json.RawMessage) string {
	var items []string
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	return strings.Join(items, ", ")
}

func editorAmountValue(raw json.RawMessage) string {
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return ""
	}
	return n.String()
}

func editorDatePair(raw json.RawMessage) (start, end string) {
	var pair struct {
		StartDate string  `json:"startDate"`
		EndDate   *string `json:"endDate"`
	}
	if json.Unmarshal(raw, &pair) != nil {
		return "", ""
	}
	if pair.EndDate != nil {
		end = *pair.EndDate
	}
	return pair.StartDate, end
}

func editorBirthdayValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return editorStringValue(raw)
}

// --- verify (double control through get_by_hash) ----------------------------

// editorChangeVerified compares one planned change against the live
// content: the locator must carry exactly the planned natural value.
func editorChangeVerified(live *hhapi.ResumeContent, fc FieldChange) bool {
	spec, ok := Spec(fc.Field)
	if !ok {
		return false
	}
	if spec.Kind == KindDate && spec.Key == "experience" {
		return experienceDatesVerified(live, fc)
	}
	for _, tg := range expandTargets(spec, live) {
		if tg.locator != fc.Locator {
			continue
		}
		got := unwrapValue(spec, tg.raw)
		switch spec.Kind {
		case KindSalary:
			got = salaryAmountJSON(got)
		case KindDate: // birthday stored as [{"date": s}]
			got = birthdayDateJSON(tg.raw)
		}
		return rawEqualT(got, fc.Value)
	}
	return false
}

// experienceDatesVerified compares a planned {startDate, endDate} range
// against the live experience item (null and "" both mean "open").
func experienceDatesVerified(live *hhapi.ResumeContent, fc FieldChange) bool {
	raw, ok := live.Profile["experience"]
	if !ok {
		return false
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil || fc.Item < 0 || fc.Item >= len(items) {
		return false
	}
	var want struct {
		Start string  `json:"startDate"`
		End   *string `json:"endDate"`
	}
	if json.Unmarshal(fc.Value, &want) != nil {
		return false
	}
	var got struct {
		Start string  `json:"startDate"`
		End   *string `json:"endDate"`
	}
	if json.Unmarshal(items[fc.Item]["startDate"], &got.Start) != nil {
		return false
	}
	if endRaw, ok := items[fc.Item]["endDate"]; ok && len(endRaw) > 0 && string(endRaw) != "null" {
		_ = json.Unmarshal(endRaw, &got.End)
	}
	if got.Start != want.Start {
		return false
	}
	wantEnd := want.End != nil && *want.End != ""
	gotEnd := got.End != nil && *got.End != ""
	if wantEnd != gotEnd {
		return false
	}
	return !wantEnd || *want.End == *got.End
}

func salaryAmountJSON(raw json.RawMessage) json.RawMessage {
	var box struct {
		Amount *float64 `json:"amount"`
	}
	if json.Unmarshal(raw, &box) != nil || box.Amount == nil {
		return nil
	}
	b, _ := json.Marshal(int64(*box.Amount))
	return b
}

func birthdayDateJSON(raw json.RawMessage) json.RawMessage {
	var parts []map[string]string
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return raw
	}
	b, _ := json.Marshal(parts[0]["date"])
	return b
}

// editorScreensFor resolves the wizard screen a field's PAGE lives on:
// the spec's candidates intersected with the resume's live screens; no
// intersection falls back to the FIRST spec candidate — unlike the API
// path (screensFor), the browser cannot save from an arbitrary screen,
// it must open the field's own page.
func editorScreensFor(id string, content *hhapi.ResumeContent) string {
	spec, ok := specIndex[id]
	if !ok {
		return ""
	}
	live := map[string]bool{}
	for _, s := range content.Screens {
		live[s] = true
	}
	for _, s := range spec.Screens {
		if live[s] {
			return s
		}
	}
	if len(spec.Screens) > 0 {
		return spec.Screens[0]
	}
	if len(content.Screens) > 0 {
		return content.Screens[0]
	}
	return ""
}

// editorNaturalValue projects a validated payload value into the natural
// DOM shape the FieldChange carries (the pipeline hands pending values in
// condition format).
func editorNaturalValue(spec FieldSpec, raw json.RawMessage) json.RawMessage {
	if spec.Kind == KindDate && spec.Key == "birthday" {
		return birthdayDateJSON(raw)
	}
	if spec.Kind == KindSalary {
		// unwrapValue returns {amount, currency}; the DOM carries the
		// amount alone (the currency is a fact the runner never touches).
		if amount := salaryAmountJSON(unwrapValue(spec, raw)); amount != nil {
			return amount
		}
		return raw
	}
	return unwrapValue(spec, raw)
}

// --- chromedp implementation -------------------------------------------------

// findEditorChrome resolves the browser executable: HHAUTO_CHROME_PATH
// wins, then the usual chromium-family names on PATH (same discovery as
// the testsolve filler).
func findEditorChrome() (string, error) {
	if p := strings.TrimSpace(os.Getenv("HHAUTO_CHROME_PATH")); p != "" {
		if path, err := exec.LookPath(p); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("%w: HHAUTO_CHROME_PATH=%q не существует или не исполняем", ErrChromeMissingEditor, p)
	}
	for _, name := range []string{"chrome", "chromium", "google-chrome", "google-chrome-stable", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", ErrChromeMissingEditor
}

// ErrChromeMissingEditor reports that no Chrome executable was found —
// the caller degrades to the manual-apply path.
var ErrChromeMissingEditor = errors.New("autoclone: Chrome не найден (укажите HHAUTO_CHROME_PATH или установите chromium)")

// startChrome is the real BrowserFactory: shared-subsystem Chrome
// resolution, preset stealth flags, throwaway profile, session cookies
// injected before the first navigation, page timeout armed.
func (r *ChromeEditorRunner) startChrome(ctx context.Context, ck []*http.Cookie) (browserRunner, func(), error) {
	var chromePath string
	cleanupChrome := func() {}
	if r.opt.ChromeProvider != nil {
		var err error
		chromePath, cleanupChrome, err = r.opt.ChromeProvider.Chrome(ctx)
		if err != nil {
			cleanupChrome()
			// Both sentinels stay discoverable: the editor degrades on
			// ErrChromeMissingEditor, the provider's own error travels too.
			return nil, nil, fmt.Errorf("%w: %v", ErrChromeMissingEditor, err)
		}
	} else {
		var err error
		chromePath, err = findEditorChrome()
		if err != nil {
			return nil, nil, err
		}
	}
	dir, err := os.MkdirTemp("", "hhauto-editor-")
	if err != nil {
		return nil, nil, fmt.Errorf("editor temp profile: %w", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(dir),
	)
	for _, f := range r.launchFlags() {
		opts = append(opts, chromedp.Flag(f.Name, f.Value))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	runCtx, cancelRun := context.WithTimeout(browserCtx, r.pageTimeout())

	cleanup := func() {
		cancelRun()
		cancelBrowser()
		cancelAlloc()
		cleanupChrome()
		_ = os.RemoveAll(dir)
	}

	secure := strings.HasPrefix(r.opt.Cfg.BaseURL, "https://")
	if err := chromedp.Run(runCtx, injectCookiesAction(ck, r.opt.Cfg.CookieDomain, secure)); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("inject session cookies: %w", err)
	}
	return &chromeBrowser{ctx: runCtx, cfg: r.opt.Cfg, pacer: editorPacer(r.opt.BrowserCfg)}, cleanup, nil
}

// stealthFlags maps the [browser] preset onto the launch flag set
// (careful = the conservative stealth pair; zero value = none).
func (r *ChromeEditorRunner) stealthFlags() []browser.StealthFlag {
	flags, err := browser.StealthFlags(r.opt.BrowserCfg.Preset)
	if err != nil {
		r.log.Warn("autoclone: браузерный preset проигнорирован", "err", err)
		return nil
	}
	return flags
}

// launchFlags assembles the lane's full launch identity (T32 stealth
// pair + T82 spec-UA pin): the session presents exactly the captured
// spec's UA, whatever the raw browser would report.
func (r *ChromeEditorRunner) launchFlags() []browser.StealthFlag {
	return append(r.stealthFlags(), browser.SpecUserAgentFlags(r.opt.SpecUserAgent)...)
}

// editorPacer builds the humanize pacer for the [browser] section; nil
// when humanize is off — a zero-value config keeps the legacy pacing.
func editorPacer(cfg config.Browser) *browser.Pacer {
	if cfg.Humanize == nil || !*cfg.Humanize {
		return nil
	}
	p, err := browser.NewPacer(cfg.Preset, time.Now().UnixNano())
	if err != nil {
		return nil
	}
	return p
}

// injectCookiesAction writes the account session into the browser jar,
// scoped to the configured hh domain. Cookies outside that domain are
// skipped — the isolation invariant (only hh.ru ever receives them).
func injectCookiesAction(ck []*http.Cookie, domain string, secure bool) chromedp.Action {
	want := strings.TrimPrefix(domain, ".")
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for _, c := range ck {
			dom := c.Domain
			if dom == "" {
				dom = domain
			}
			host := strings.TrimPrefix(dom, ".")
			if host != want && !strings.HasSuffix(host, "."+want) {
				continue
			}
			path := c.Path
			if path == "" {
				path = "/"
			}
			p := network.SetCookie(c.Name, c.Value).
				WithDomain(dom).
				WithPath(path).
				WithSecure(c.Secure || secure)
			if c.HttpOnly {
				p = p.WithHTTPOnly(true)
			}
			if err := p.Do(ctx); err != nil {
				return fmt.Errorf("set cookie %s: %w", c.Name, err)
			}
		}
		return nil
	})
}

// chromeBrowser runs the per-screen work on an authenticated session.
type chromeBrowser struct {
	ctx   context.Context
	cfg   config.AutoCloneImproveEditor
	pacer *browser.Pacer // nil = legacy pacing (humanize off)
}

func (b *chromeBrowser) saveTimeout() time.Duration {
	d := time.Duration(b.cfg.SaveTimeoutSec) * time.Second
	if d <= 0 {
		d = 20 * time.Second
	}
	return d
}

func (r *ChromeEditorRunner) pageTimeout() time.Duration {
	d := time.Duration(r.opt.Cfg.PageTimeoutSec) * time.Second
	if d <= 0 {
		d = 120 * time.Second
	}
	return d
}

// runScreen opens the wizard page, fills the ops, saves and waits a
// bounded window for the wizard to leave the screen. The save-wait is
// best effort: the get_by_hash read-back is the authoritative gate.
//
// The work runs on the browser's own authenticated context (b.ctx, a
// descendant of the caller's ctx — cancellation still propagates); the
// caller's raw context carries no chromedp session.
func (b *chromeBrowser) runScreen(_ context.Context, url string, ops []editorFillOp) ([]editorOpOutcome, error) {
	ctx := b.ctx
	// Humanize pacing between screens (a no-op when humanize is off).
	if b.pacer != nil {
		if err := b.pacer.Wait(ctx, browser.PaceAction); err != nil {
			return nil, err
		}
	}
	var finalURL string
	if err := chromedp.Run(ctx, chromedp.Navigate(url), chromedp.Location(&finalURL)); err != nil {
		return nil, fmt.Errorf("navigate %s: %w", url, err)
	}
	// The SPA must have rendered the form before filling.
	ready := editorReadyJS(ops)
	if err := chromedp.Run(ctx, chromedp.Poll(ready, nil,
		chromedp.WithPollingTimeout(b.saveTimeout()))); err != nil {
		return nil, fmt.Errorf("editor form did not render on %s: %w", url, err)
	}

	var outcomes []editorOpOutcome
	outcomes, err := b.fillOps(ctx, ops)
	if err != nil {
		return nil, err
	}

	if b.pacer != nil {
		if err := b.pacer.Wait(ctx, browser.PaceAction); err != nil {
			return nil, err
		}
	}
	var clicked string
	if err := chromedp.Run(ctx, chromedp.Evaluate(clickEditorSaveJS, &clicked)); err != nil {
		return nil, fmt.Errorf("editor save click: %w", err)
	}
	if clicked == "none" {
		return nil, errors.New("editor save control not found on the page")
	}
	// Bounded nextIncompleteScreenId-shift wait; a timeout is not fatal
	// (hh may keep the wizard on an incomplete screen — the read-back
	// verifies the values either way).
	_ = chromedp.Run(ctx, chromedp.Poll(
		"location.href !== "+strconv.Quote(finalURL), nil,
		chromedp.WithPollingTimeout(b.saveTimeout())))
	return outcomes, nil
}

// fillOps applies the screen's fill ops: with the humanize pacer armed,
// text ops go through chunked typing and the rest through the batched
// fill; otherwise everything runs in the legacy single pass.
func (b *chromeBrowser) fillOps(ctx context.Context, ops []editorFillOp) ([]editorOpOutcome, error) {
	if b.pacer == nil {
		return b.fillBatch(ctx, ops)
	}
	typed, rest := splitTypedOps(ops)
	out := make([]editorOpOutcome, 0, len(ops))
	manual, err := b.typeValues(ctx, typed)
	if err != nil {
		return nil, err
	}
	out = append(out, manual...)
	if len(rest) == 0 {
		return out, nil
	}
	batched, err := b.fillBatch(ctx, rest)
	if err != nil {
		return nil, err
	}
	return append(out, batched...), nil
}

// splitTypedOps separates text-value ops (humanized typing) from the rest,
// preserving order on both sides.
func splitTypedOps(ops []editorFillOp) (typed, rest []editorFillOp) {
	for _, op := range ops {
		if op.Value != "" {
			typed = append(typed, op)
		} else {
			rest = append(rest, op)
		}
	}
	return typed, rest
}

// fillBatch applies ops through the legacy single-pass fill JS.
func (b *chromeBrowser) fillBatch(ctx context.Context, ops []editorFillOp) ([]editorOpOutcome, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fillEditorJS(ops), &raw)); err != nil {
		return nil, fmt.Errorf("editor fill: %w", err)
	}
	return decodeOpOutcomes(raw)
}

// typeValues applies text ops with the humanize cadence: smooth focus on
// the first chunk, chunked native-set slices with per-chunk input events,
// the full event set on the last chunk. A control that never resolves
// yields one not-found outcome for its op (the batched semantics).
func (b *chromeBrowser) typeValues(ctx context.Context, ops []editorFillOp) ([]editorOpOutcome, error) {
	out := make([]editorOpOutcome, 0, len(ops))
	for _, op := range ops {
		if err := b.pacer.Wait(ctx, browser.PaceFocus); err != nil {
			return out, err
		}
		chunk, maxTyped := b.pacer.Chunk(), b.pacer.MaxTyped()
		plan := browser.TypePlan(op.Value, chunk, maxTyped)
		for i, ch := range plan {
			first, last := i == 0, i == len(plan)-1
			if !first {
				if err := b.pacer.Wait(ctx, browser.PaceType); err != nil {
					return out, err
				}
			}
			var raw string
			if err := chromedp.Run(ctx, chromedp.Evaluate(humanEditorSetJS(op, ch.To, first, last), &raw)); err != nil {
				return out, fmt.Errorf("editor type fill: %w", err)
			}
			var oc editorOpOutcome
			if err := json.Unmarshal([]byte(raw), &oc); err != nil {
				return out, fmt.Errorf("decode typed outcome: %w", err)
			}
			if !oc.OK {
				out = append(out, oc) // control never resolved — stop typing it
				break
			}
			if last {
				out = append(out, oc)
			}
		}
	}
	return out, nil
}

func decodeOpOutcomes(raw string) ([]editorOpOutcome, error) {
	var out []editorOpOutcome
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode fill outcomes: %w", err)
	}
	return out, nil
}

// --- in-page JavaScript (thin, testsolve-style) ------------------------------

// editorReadyJS polls until the first op's primary control (or any
// form control) exists in the DOM.
func editorReadyJS(ops []editorFillOp) string {
	sel := ""
	if len(ops) > 0 && len(ops[0].Selectors) > 0 {
		sel = ops[0].Selectors[0]
	}
	return `(() => {
  const q = s => { try { return document.querySelector(s); } catch (e) { return null; } };
  return !!(q(` + strconv.Quote(sel) + `) || q('input,textarea'));
})()`
}

// editorResolveHelpersJS is the shared in-page helper block for the fill
// builders: label normalization, visibility and the two control
// resolution strategies (selector chain first, label-text fallback).
const editorResolveHelpersJS = `
  const norm = s => (s || '').replace(/\s+/g, ' ').trim().toLowerCase();
  const visible = el => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const bySelectors = sels => {
    for (const s of sels) {
      let el = null;
      try { el = document.querySelector(s); } catch (e) {}
      if (el && !el.disabled && !el.readOnly && visible(el)) return {el, by: s};
    }
    return null;
  };
  const byLabel = label => {
    if (!label) return null;
    for (const el of document.querySelectorAll('input,textarea')) {
      if (el.disabled || el.readOnly) continue;
      const texts = [];
      if (el.id) {
        let l = null;
        try { l = document.querySelector('label[for="' + CSS.escape(el.id) + '"]'); } catch (e) {}
        if (l) texts.push(l.textContent);
      }
      const wrap = el.closest('label');
      if (wrap) texts.push(wrap.textContent);
      texts.push(el.getAttribute('aria-label') || '', el.getAttribute('placeholder') || '');
      if (texts.some(t => norm(t).includes(norm(label)))) return {el, by: 'label:' + label};
    }
    return null;
  };`

// fillEditorJS resolves each op by its selector chain (label-text
// fallback), sets the value through the native prototype setter and
// fires the React state events.
func fillEditorJS(ops []editorFillOp) string {
	b, _ := json.Marshal(ops)
	return `(() => {
  const ops = ` + string(b) + `;` + editorResolveHelpersJS + `
  const fire = el => {
    for (const t of ['input', 'change', 'blur', 'focusout']) {
      el.dispatchEvent(new Event(t, {bubbles: ['blur', 'focusout'].includes(t) ? false : true}));
    }
  };
  const out = [];
  for (const op of ops) {
    const hit = bySelectors(op.selectors) || byLabel(op.label);
    if (!hit) { out.push({field: op.field, ok: false, err: 'not_found'}); continue; }
    const proto = hit.el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(hit.el, op.value);
    fire(hit.el);
    out.push({field: op.field, ok: true, selector: hit.by});
  }
  return JSON.stringify(out);
})()`
}

// humanEditorSetJS applies one humanize typing step to a single editor op:
// the value prefix through `to` is set via the native prototype setter
// (same resolution as the batched fill), the real focus event fires on
// the first chunk, per-chunk input events in between and the full event
// set on the last chunk — identical end state, human cadence.
func humanEditorSetJS(op editorFillOp, to int, first, last bool) string {
	opJSON, _ := json.Marshal(op)
	return `(() => {
  const op = ` + string(opJSON) + `;
  const to = ` + strconv.Itoa(to) + `, first = ` + jsBool(first) + `, last = ` + jsBool(last) + `;` + editorResolveHelpersJS + `
  const hit = bySelectors(op.selectors) || byLabel(op.label);
  if (!hit) return JSON.stringify({field: op.field, ok: false, err: 'not_found'});
  if (first) hit.el.focus();
  const proto = hit.el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
  Object.getOwnPropertyDescriptor(proto, 'value').set.call(hit.el, op.value.slice(0, to));
  if (last) {
    for (const t of ['input', 'change', 'blur', 'focusout']) {
      hit.el.dispatchEvent(new Event(t, {bubbles: ['blur', 'focusout'].includes(t) ? false : true}));
    }
  } else {
    hit.el.dispatchEvent(new Event('input', {bubbles: true}));
  }
  return JSON.stringify({field: op.field, ok: true, selector: hit.by});
})()`
}

// jsBool renders a Go bool as a JS literal.
func jsBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// clickEditorSaveJS clicks the first visible save-like control and
// reports what it clicked ("none" when nothing qualifies).
const clickEditorSaveJS = `(() => {
  const visible = el => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const cand = [...document.querySelectorAll('[data-qa*="save"], button[type=submit], input[type=submit], button:not([type])')]
    .find(b => !b.disabled && visible(b));
  if (!cand) return 'none';
  cand.click();
  return 'clicked';
})()`
