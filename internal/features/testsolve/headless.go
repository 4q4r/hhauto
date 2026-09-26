// headless.go implements T17 — the headless (chromedp) executor for external
// test forms. The filler owns one isolated Chrome profile per run and never
// shares state with the hh session:
//
//	CONTEXT ISOLATION INVARIANT: the hh.ru session lives in its own cookie
//	store (internal/cookies) and is only touched by hhapi clients. The
//	browser started here gets a THROWAWAY user-data-dir under the OS temp
//	directory, receives no cookies, no stored passwords and no extensions,
//	and the directory is deleted when the run ends. An external form site
//	can therefore never observe (or poison) the hh identity, and the hh
//	cookies can never leak into a third-party form. Testsolve is the ONLY
//	package allowed to open a browser for form filling.
//
// Flow per Fill call: discover Chrome → resolve the account proxy (no-auth
// proxies only for v1) → isolated profile → navigate within
// forms.page_timeout_sec → reject off-allow-list redirects and password
// fields (anti-phishing, same semantics as the draft path) → extract fields
// with one in-page JS snippet → answer via AI Runner (TaskTestAnswer, batches
// ≤5) → fill by field type → serialize the DOM → submit only under
// submit_policy="auto", guarded by the "extsubmit:<account>:<testID>"
// idempotency intent, clicked exactly once (a lost click answer burns the
// intent: a replay could double-submit an external form).
//
// Screenshots are forbidden by policy: the artifacts of a run are the
// serialized DOM and the answers JSON, stored on the assessment row.

package testsolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/chromedp/chromedp"

	"hhauto/internal/ai"
	"hhauto/internal/browser"
	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/kit11"
	"hhauto/internal/store"
)

// Sentinel errors surfaced to the queue/reporting layer.
var (
	// ErrChromeMissing reports that no Chrome/Chromium executable was found
	// (HHAUTO_CHROME_PATH unset or pointing nowhere). Doctor shows the fix.
	ErrChromeMissing = errors.New("testsolve: Chrome не найден (укажите HHAUTO_CHROME_PATH или установите chromium)")
	// ErrAuthProxyUnsupported reports an authenticated proxy ref: chromedp's
	// --proxy-server flag carries no credentials, so v1 refuses the run.
	ErrAuthProxyUnsupported = errors.New("testsolve: auth-прокси для headless не поддержан — используйте прокси без логина/пароля или draft_only")
	// ErrPasswordDetected is the anti-phishing abort (password field on a
	// form page) — same semantics as the draft path.
	ErrPasswordDetected = errors.New("testsolve: password field on the form page — suspected phishing")
	// ErrHostDisallowed reports a navigation that ended outside the
	// forms.allow_domains list (e.g. an off-list redirect).
	ErrHostDisallowed = errors.New("testsolve: page landed on a non-allowed host — refused")
	// ErrNoFields reports a page without answerable fields.
	ErrNoFields = errors.New("testsolve: no answerable fields on the form page")
	// ErrNoSubmitButton is internal: policy=auto but the DOM has no submit
	// control. No side effect could happen, so the intent is retriable.
	ErrNoSubmitButton = errors.New("testsolve: submit button not found")
)

// Filler executes one external-form run in a real browser without hh
// cookies. HeadlessFiller is the chromedp implementation; tests inject fakes.
type Filler interface {
	Fill(ctx context.Context, req FillRequest) (FillResult, error)
}

// FillRequest is one solve-the-form request.
type FillRequest struct {
	AccountID    string
	TestID       string // one-shot identity ("ext:…"), scopes the submit intent
	URL          string
	SubmitPolicy string // "auto" submits; anything else (incl. "") = human, never submits
	ProfileText  string // trusted candidate context for the AI answers
}

// SubmitState reports what the submit step did.
type SubmitState int

const (
	// SubmitNotRequested means the policy is human (or the run became
	// unsure) — the form was filled but nothing was clicked.
	SubmitNotRequested SubmitState = iota
	// SubmitClicked means policy auto and the submit control was clicked once.
	SubmitClicked
	// SubmitAlreadyDone means the extsubmit intent was already settled — a
	// previous run consumed it. NEVER clicked again.
	SubmitAlreadyDone
	// SubmitAlreadyPending means another run holds a live extsubmit intent.
	SubmitAlreadyPending
	// SubmitConsumed means the click answer was lost after dispatch; the
	// intent was settled done so no replay can double-submit.
	SubmitConsumed
)

// String implements fmt.Stringer for logs/webhook payloads.
func (s SubmitState) String() string {
	switch s {
	case SubmitClicked:
		return "clicked"
	case SubmitAlreadyDone:
		return "already_done"
	case SubmitAlreadyPending:
		return "already_pending"
	case SubmitConsumed:
		return "consumed"
	default:
		return "not_requested"
	}
}

// AnswerEntry is one filled question (artifact + FillResult shape).
type AnswerEntry struct {
	Question       string `json:"question"`
	Answer         string `json:"answer"`
	BasedOnProfile bool   `json:"based_on_profile"`
}

// FillResult carries the run artifacts: the DOM serialized after filling and
// the answers. Screenshots are forbidden by policy.
type FillResult struct {
	URL      string // final URL after redirects
	DOM      string // serialized document after filling (before any submit)
	Answers  []AnswerEntry
	Unsure   bool
	Unfilled int // answerable fields the filler could not set
	Submit   SubmitState
}

// HeadlessFiller is the chromedp-backed Filler.
type HeadlessFiller struct {
	Cfg config.Config
	DB  *store.DB // extsubmit intents (submit-once)
	AI  ai.Runner
	Log *slog.Logger
	Now func() time.Time
	// Chrome overrides executable discovery (tests). nil = FindChrome.
	// Ignored when Browser is set.
	Chrome func() (string, error)
	// Browser resolves the executable through the shared browser subsystem
	// (T32: HHAUTO_CHROME_PATH → PATH → cloakbrowser cache → auto-download).
	// nil = the legacy FindChrome chain without cache/download (прежнее
	// поведение).
	Browser browser.Provider
	// clickSubmit overrides the in-page submit click (tests). nil = the
	// chromedp Evaluate. Returns ErrNoSubmitButton when the DOM has no
	// clickable submit control (no side effect could happen).
	clickSubmit func(ctx context.Context) error
}

// Compile-time conformance.
var _ Filler = (*HeadlessFiller)(nil)

// FindChrome resolves the browser executable: HHAUTO_CHROME_PATH wins, then
// the usual chromium-family names on PATH. A set-but-broken override is an
// error — the explicit setting is the single source of truth.
func FindChrome() (string, error) {
	if p := strings.TrimSpace(os.Getenv("HHAUTO_CHROME_PATH")); p != "" {
		if path, err := exec.LookPath(p); err == nil {
			return path, nil
		}
		return "", fmt.Errorf("%w: HHAUTO_CHROME_PATH=%q не существует или не исполняем", ErrChromeMissing, p)
	}
	for _, name := range []string{"chrome", "chromium", "google-chrome", "google-chrome-stable", "chromium-browser"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", ErrChromeMissing
}

// proxyServerFor builds the --proxy-server flag value for the account's
// proxy: "<type>://<host>:<port>". Empty string means direct. Authenticated
// proxies are refused: the chromedp flag carries no credentials (v1 keeps
// pure-Go flags; an extension-based resolver is future work).
func proxyServerFor(cfg config.Config, accountID string) (string, error) {
	acc, ok := cfg.Accounts[accountID]
	if !ok || acc.ProxyRef == "" {
		return "", nil // direct (explicitly allowed, same as the draft fetcher)
	}
	p, ok := cfg.Proxy[acc.ProxyRef]
	if !ok {
		return "", fmt.Errorf("testsolve: proxy ref %q: unknown proxy (fail-closed)", acc.ProxyRef)
	}
	if p.User != "" || p.PassEnv != "" || p.Pass != "" {
		return "", fmt.Errorf("%w (proxy ref %q)", ErrAuthProxyUnsupported, acc.ProxyRef)
	}
	scheme := p.Type
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, p.Host, p.Port), nil
}

// extractedPage is the JSON payload of the in-page extraction snippet.
type extractedPage struct {
	PasswordDetected bool             `json:"password"`
	Title            string           `json:"title"`
	URL              string           `json:"url"`
	Fields           []extractedField `json:"fields"`
}

// extractedField is one fillable control group in DOM coordinates. Ctrls
// index into the page's flat input/textarea/select list — the fill snippet
// clicks/sets by these indices, never by fragile CSS selectors.
type extractedField struct {
	Kind     string   `json:"kind"` // text | textarea | select | radio | check
	Ctrls    []int    `json:"ctrls"`
	Label    string   `json:"label"`
	Options  []string `json:"options"`
	Required bool     `json:"required"`
}

// decodeExtracted parses the extraction snippet payload.
func decodeExtracted(raw string) (extractedPage, error) {
	var page extractedPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		return extractedPage{}, fmt.Errorf("testsolve: extraction decode: %w", err)
	}
	return page, nil
}

// form projects the extracted fields onto the draft-path question shape.
func (p extractedPage) form() Form {
	f := Form{Title: p.Title}
	for _, ef := range p.Fields {
		f.Fields = append(f.Fields, FormField{
			Label:    ef.Label,
			Type:     ef.Kind,
			Options:  ef.Options,
			Required: ef.Required,
		})
	}
	return f
}

// normalizeAnswer folds case, punctuation and whitespace for option matching.
func normalizeAnswer(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// matchOption reports whether a select/radio option matches the AI answer:
// exact fold first, then containment (minimum 3 letters to avoid noise).
func matchOption(opt, answer string) bool {
	o, a := normalizeAnswer(opt), normalizeAnswer(answer)
	if o == "" || a == "" {
		return false
	}
	if o == a {
		return true
	}
	if len([]rune(o)) >= 3 && strings.Contains(a, o) {
		return true
	}
	return len([]rune(a)) >= 3 && strings.Contains(o, a)
}

// positiveAnswer parses yes-style checkbox answers (checked state).
func positiveAnswer(answer string) bool {
	switch normalizeAnswer(answer) {
	case "да", "yes", "true", "1", "ок", "ok", "ага", "согласен", "принято", "подтверждаю":
		return true
	}
	return false
}

// fillOp is one planned DOM mutation for the fill snippet. Index is the
// option index within Ctrls (select/radio); Checked drives checkboxes.
type fillOp struct {
	Kind    string `json:"kind"` // set | select | radio | check
	Ctrls   []int  `json:"ctrls"`
	Value   string `json:"value,omitempty"`
	Index   int    `json:"index,omitempty"`
	Checked bool   `json:"checked,omitempty"`
}

// planFillOps maps AI answers onto DOM operations. An answer that matches no
// option produces NO operation (never guess) and counts toward unfilled —
// the caller escalates such runs instead of submitting half-filled forms.
func planFillOps(fields []extractedField, answers []AnswerEntry) (ops []fillOp, unfilled int) {
	for i, ef := range fields {
		if i >= len(answers) {
			unfilled += len(fields) - i
			break
		}
		ans := answers[i].Answer
		switch ef.Kind {
		case "text", "textarea":
			if strings.TrimSpace(ans) != "" {
				ops = append(ops, fillOp{Kind: "set", Ctrls: ef.Ctrls, Value: ans})
			} else {
				unfilled++
			}
		case "select":
			idx := -1
			for oi, opt := range ef.Options {
				if matchOption(opt, ans) {
					idx = oi
					break
				}
			}
			if idx < 0 {
				unfilled++
				continue
			}
			ops = append(ops, fillOp{Kind: "select", Ctrls: ef.Ctrls, Index: idx})
		case "radio":
			idx := -1
			for oi, opt := range ef.Options {
				if matchOption(opt, ans) {
					idx = oi
					break
				}
			}
			if idx < 0 || idx >= len(ef.Ctrls) {
				unfilled++
				continue
			}
			// Click by option index within the radio group's control list.
			ops = append(ops, fillOp{Kind: "radio", Ctrls: []int{ef.Ctrls[idx]}})
		case "check":
			ops = append(ops, fillOp{Kind: "check", Ctrls: ef.Ctrls, Checked: positiveAnswer(ans)})
		default:
			unfilled++
		}
	}
	return ops, unfilled
}

// Fill implements Filler. See the file head comment for the full contract.
func (h *HeadlessFiller) Fill(ctx context.Context, req FillRequest) (FillResult, error) {
	runCtx, cancel, err := h.newBrowser(ctx, req)
	if err != nil {
		return FillResult{}, err
	}
	defer cancel()

	finalURL, err := h.navigate(runCtx, req.URL)
	if err != nil {
		return FillResult{}, err
	}
	page, err := h.extractPage(runCtx)
	if err != nil {
		return FillResult{}, err
	}

	answers, unsure, err := h.answerAll(runCtx, req, page.form())
	if err != nil {
		return FillResult{}, err // transient: AI outage → engine requeue
	}
	unfilled, err := h.fillFields(runCtx, page.Fields, answers)
	if err != nil {
		return FillResult{}, err
	}
	unsure = unsure || unfilled > 0
	dom, err := h.domSnapshot(runCtx)
	if err != nil {
		return FillResult{}, err
	}

	res := FillResult{
		URL:      finalURL,
		DOM:      dom,
		Answers:  answers,
		Unsure:   unsure,
		Unfilled: unfilled,
	}
	if err := h.maybeSubmit(runCtx, req, &res); err != nil {
		return res, err
	}
	return res, nil
}

// newBrowser prepares a throwaway-profile chromedp run context: resolves
// Chrome (provider chain or legacy discovery), applies the preset stealth
// flags, wires the optional no-auth proxy, arms the page timeout. The
// returned cancel stops the browser and removes the profile dir (the run
// context's own cancel must be called first by the caller).
func (h *HeadlessFiller) newBrowser(ctx context.Context, req FillRequest) (context.Context, func(), error) {
	// 1. Chrome discovery fails fast, before any directory or process work.
	var chromePath string
	cleanupChrome := func() {}
	if h.Browser != nil {
		var err error
		chromePath, cleanupChrome, err = h.Browser.Chrome(ctx)
		if err != nil {
			return nil, nil, err
		}
	} else {
		lookup := h.Chrome
		if lookup == nil {
			lookup = FindChrome
		}
		var err error
		chromePath, err = lookup()
		if err != nil {
			return nil, nil, err
		}
	}

	// 2. Proxy: no-auth flag only (typed refusal otherwise).
	server, err := proxyServerFor(h.Cfg, req.AccountID)
	if err != nil {
		return nil, nil, err
	}

	// 3. Throwaway profile under the OS temp dir; removed after the browser
	// is gone (defer LIFO below: cancelBrowser → cancelAlloc → RemoveAll).
	// NEVER seeded with hh cookies — see the file-head invariant.
	dir, err := os.MkdirTemp("", "hhauto-forms-")
	if err != nil {
		return nil, nil, fmt.Errorf("testsolve: temp profile: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(dir),
	)
	for _, f := range h.stealthFlags() {
		opts = append(opts, chromedp.Flag(f.Name, f.Value))
	}
	if server != "" {
		opts = append(opts, chromedp.ProxyServer(server))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)

	timeout := time.Duration(h.Cfg.Forms.PageTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultPageTimeout
	}
	runCtx, cancelRun := context.WithTimeout(browserCtx, timeout)

	cancel := func() {
		cancelRun()
		cancelBrowser()
		cancelAlloc()
		cleanupChrome()
		_ = os.RemoveAll(dir)
	}
	return runCtx, cancel, nil
}

// stealthFlags maps the [browser] preset onto the launch flag set
// (careful = the conservative stealth pair; zero value = none).
func (h *HeadlessFiller) stealthFlags() []browser.StealthFlag {
	flags, err := browser.StealthFlags(h.Cfg.Browser.Preset)
	if err != nil {
		h.log().Warn("testsolve: браузерный preset проигнорирован", "err", err)
		return nil
	}
	return flags
}

// humanize reports the [browser].humanize toggle. A zero-value config
// keeps the legacy single-pass fill — the toggle is activated by the
// config defaults (nil→true in applyDefaults), never implicitly.
func (h *HeadlessFiller) humanize() bool {
	return h.Cfg.Browser.Humanize != nil && *h.Cfg.Browser.Humanize
}

// navigate opens the form URL and enforces the allow-list on the final
// URL after redirects (same refusal as the draft fetcher's CheckRedirect).
func (h *HeadlessFiller) navigate(ctx context.Context, reqURL string) (string, error) {
	// Allocator starts on the first Run.
	var finalURL string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(reqURL),
		chromedp.Location(&finalURL),
	); err != nil {
		return "", fmt.Errorf("testsolve: navigate %s: %w", reqURL, err)
	}
	// Redirects must stay on the allow-list (same refusal as the draft
	// fetcher's CheckRedirect).
	if !kit11.HostAllowed(hostPortOf(finalURL), h.Cfg.Forms.AllowedDomains) {
		return "", fmt.Errorf("%w: %s", ErrHostDisallowed, finalURL)
	}
	return finalURL, nil
}

// extractPage runs the field-extraction JS, decodes it and applies the
// raw-DOM gates: password fields, unanswerable pages, off-list hosts.
func (h *HeadlessFiller) extractPage(ctx context.Context) (extractedPage, error) {
	// 5. Extract fields (anti-phishing password gate first, raw-DOM
	// semantics preserved from the draft path).
	var extracted string
	if err := chromedp.Run(ctx, chromedp.Evaluate(extractFieldsJS, &extracted)); err != nil {
		return extractedPage{}, fmt.Errorf("testsolve: extract: %w", err)
	}
	page, err := decodeExtracted(extracted)
	if err != nil {
		return extractedPage{}, err
	}
	if page.PasswordDetected {
		return extractedPage{}, ErrPasswordDetected
	}
	if len(page.Fields) == 0 {
		return extractedPage{}, ErrNoFields
	}
	if !kit11.HostAllowed(hostPortOf(page.URL), h.Cfg.Forms.AllowedDomains) {
		return extractedPage{}, fmt.Errorf("%w: %s", ErrHostDisallowed, page.URL)
	}
	return page, nil
}

// fillFields applies the fill operations; with humanize enabled the ops
// run one by one with the human cadence (chunked typing, micro-delays,
// smooth focus), otherwise in one legacy JS pass; the count of unanswered
// fields is reported either way (never guessed).
func (h *HeadlessFiller) fillFields(ctx context.Context, fields []extractedField, answers []AnswerEntry) (int, error) {
	ops, unfilled := planFillOps(fields, answers)
	if len(ops) == 0 {
		return unfilled, nil
	}
	if h.humanize() {
		return h.fillFieldsHumanized(ctx, ops, unfilled)
	}
	var fillRes string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fillJS(ops), &fillRes)); err != nil {
		return unfilled, fmt.Errorf("testsolve: fill: %w", err)
	}
	return unfilled, nil
}

// fillFieldsHumanized applies the ops with the humanize cadence: smooth
// focus + chunked typing for text values, paced single steps for
// select/radio/check. The unfilled semantics are identical to the batched
// path, and pacing never extends the page timeout (Wait honors ctx).
func (h *HeadlessFiller) fillFieldsHumanized(ctx context.Context, ops []fillOp, unfilled int) (int, error) {
	pacer, err := browser.NewPacer(h.Cfg.Browser.Preset, time.Now().UnixNano())
	if err != nil {
		return unfilled, err
	}
	for _, step := range typePlanSteps(ops, pacer) {
		if err := pacer.Wait(ctx, browser.PaceAction); err != nil {
			return unfilled, err
		}
		var res string
		if err := chromedp.Run(ctx, chromedp.Evaluate(humanFillJS(step), &res)); err != nil {
			return unfilled, fmt.Errorf("testsolve: fill: %w", err)
		}
	}
	return unfilled, nil
}

// humanStep is one humanize fill step: a typed chunk of a text value
// (typed=true, first/last mark the focus/change edges) or a whole
// non-text op.
type humanStep struct {
	op    fillOp
	chunk browser.TypeChunk
	typed bool
	first bool
	last  bool
}

// typePlanSteps projects the fill ops onto cadence steps: text values
// split through browser.TypePlan (chunk/cap from the preset), everything
// else stays one step.
func typePlanSteps(ops []fillOp, pacer *browser.Pacer) []humanStep {
	var steps []humanStep
	for _, op := range ops {
		if op.Kind == "set" {
			plan := browser.TypePlan(op.Value, pacer.Chunk(), pacer.MaxTyped())
			for i, ch := range plan {
				steps = append(steps, humanStep{
					op: op, chunk: ch, typed: true,
					first: i == 0, last: i == len(plan)-1,
				})
			}
			continue
		}
		steps = append(steps, humanStep{op: op})
	}
	return steps
}

// humanFillJS applies one cadence step. Text steps focus the control on
// the first chunk (the real focus event), set the value through the
// native prototype setter up to the chunk boundary and fire input per
// chunk; the last chunk adds change. Non-text steps reproduce the fillJS
// behavior for a single op. The control-index resolution mirrors fillJS.
func humanFillJS(s humanStep) string {
	opJSON, _ := json.Marshal(s.op)
	return `(() => {
  const op = ` + string(opJSON) + `;
  const typed = ` + jsBool(s.typed) + `, first = ` + jsBool(s.first) + `, last = ` + jsBool(s.last) + `;
  const to = ` + itoa(s.chunk.To) + `;
  const controls = [...document.querySelectorAll('input,textarea,select')];
  const fire = (el, ts) => { for (const t of ts) el.dispatchEvent(new Event(t, {bubbles: true})); };
  const els = (op.ctrls || []).map(i => controls[i]).filter(Boolean);
  if (!els.length) return JSON.stringify({filled: 0});
  const el = els[0];
  if (el.disabled || el.readOnly) return JSON.stringify({filled: 0});
  if (typed) {
    if (first) el.focus();
    const proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, op.value.slice(0, to));
    if (last) { fire(el, ['input', 'change']); } else { fire(el, ['input']); }
  } else if (op.kind === 'select') {
    if (el.tagName !== 'SELECT' || op.index >= el.options.length) return JSON.stringify({filled: 0});
    el.selectedIndex = op.index; fire(el, ['input', 'change']);
  } else if (op.kind === 'radio') {
    el.click();
  } else if (op.kind === 'check') {
    if (el.checked !== op.checked) el.click();
  }
  return JSON.stringify({filled: 1});
})()`
}

// jsBool renders a Go bool as a JS literal.
func jsBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// itoa is a tiny strconv.Itoa alias for the JS builders.
func itoa(n int) string { return strconv.Itoa(n) }

// domSnapshot serializes the filled DOM (the artifact; screenshots forbidden).
func (h *HeadlessFiller) domSnapshot(ctx context.Context) (string, error) {
	var dom string
	if err := chromedp.Run(ctx, chromedp.Evaluate(domJS, &dom)); err != nil {
		return "", fmt.Errorf("testsolve: serialize: %w", err)
	}
	return dom, nil
}

// maybeSubmit clicks submit exactly once when the policy allows and the
// verdict is certain; a missing button escalates to unsure instead.
func (h *HeadlessFiller) maybeSubmit(ctx context.Context, req FillRequest, res *FillResult) error {
	// 9. Submit exactly once, only when policy allows and nothing is unsure.
	if req.SubmitPolicy != "auto" || res.Unsure {
		return nil
	}
	submitFn := h.clickSubmit
	if submitFn == nil {
		submitFn = func(c context.Context) error { return clickSubmit(c) }
	}
	st, serr := h.submitOnce(ctx, req.AccountID, req.TestID, submitFn)
	if errors.Is(serr, ErrNoSubmitButton) {
		// Deterministic no-side-effect: the intent stays retriable; the run
		// escalates to a human instead of looping the queue.
		res.Unsure = true
		return nil
	}
	if serr != nil {
		// Lost click answer: intent burned (never repeat), task requeues and
		// the next run hits SubmitAlreadyDone → escalated.
		res.Submit = st
		return fmt.Errorf("testsolve: submit: %w", serr)
	}
	res.Submit = st
	return nil
}

// submitOnce guards the single click with the extsubmit intent. Any error
// after the intent was claimed settles the intent DONE: an external submit
// must never be replayed on an unknown outcome.
func (h *HeadlessFiller) submitOnce(ctx context.Context, accountID, testID string, click func(context.Context) error) (SubmitState, error) {
	key := fmt.Sprintf("extsubmit:%s:%s", accountID, testID)
	claim, err := featurekit.BeginIntent(ctx, h.DB, key,
		featurekit.IntentPayload(map[string]string{"account": accountID, "test": testID, "url_at_claim": ""}))
	if err != nil {
		return SubmitNotRequested, fmt.Errorf("testsolve: intent: %w", err)
	}
	switch claim.Step {
	case featurekit.IntentReconcileFirst:
		return SubmitAlreadyDone, nil
	case featurekit.IntentSkipPending:
		return SubmitAlreadyPending, nil
	}

	clickErr := click(ctx)
	switch {
	case clickErr == nil:
		if derr := h.DB.IntentsMarkDone(ctx, claim.ID); derr != nil {
			h.log().Warn("testsolve: intent done mark failed", "key", key, "err", derr)
		}
		return SubmitClicked, nil
	case errors.Is(clickErr, ErrNoSubmitButton):
		// Nothing was clicked: the intent is failed (re-armed later is safe).
		if ferr := h.DB.IntentsMarkFailed(ctx, claim.ID); ferr != nil {
			h.log().Warn("testsolve: intent failed mark failed", "key", key, "err", ferr)
		}
		return SubmitNotRequested, ErrNoSubmitButton
	default:
		// Unknown outcome after dispatch → burn the intent forever.
		if derr := h.DB.IntentsMarkDone(ctx, claim.ID); derr != nil {
			h.log().Warn("testsolve: intent done mark failed", "key", key, "err", derr)
		}
		return SubmitConsumed, clickErr
	}
}

// answerAll answers every field via Runner.TaskTestAnswer in batches of ≤5.
// The verdict contract (answers in question order, unsure flags) matches the
// draft path's answerBatch.
func (h *HeadlessFiller) answerAll(ctx context.Context, req FillRequest, form Form) ([]AnswerEntry, bool, error) {
	unsure := false
	var out []AnswerEntry
	for start := 0; start < len(form.Fields); start += maxBatch {
		end := min(start+maxBatch, len(form.Fields))
		batch, batchUnsure, err := h.answerBatch(ctx, req.AccountID, form.Fields[start:end], req.ProfileText)
		if err != nil {
			return nil, false, err
		}
		out = append(out, batch...)
		unsure = unsure || batchUnsure
	}
	return out, unsure, nil
}

// answerBatch is the headless twin of Solver.answerBatch (same prompt input,
// same verdict decoding, same unsure semantics).
func (h *HeadlessFiller) answerBatch(ctx context.Context, accountID string, fields []FormField, profileText string) ([]AnswerEntry, bool, error) {
	type q struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
		Type string `json:"type,omitempty"`
	}
	questions := make([]q, 0, len(fields))
	for i, f := range fields {
		text := f.Label
		if text == "" {
			text = fmt.Sprintf("(вопрос %d без текста)", i+1)
		}
		if len(f.Options) > 0 {
			text += " Варианты: " + strings.Join(f.Options, " / ")
		}
		questions = append(questions, q{ID: i + 1, Text: text, Type: f.Type})
	}
	input := map[string]any{
		"questions": questions,
		"profile":   profileText,
	}
	raw, _, err := h.AI.Run(ctx, accountID, "test", questions[0].Text[:min(40, len(questions[0].Text))], "", ai.TaskTestAnswer, input)
	if err != nil {
		return nil, false, fmt.Errorf("testsolve: ai test_answer: %w", err)
	}
	var verdict struct {
		ai.Common
		Answers []AnswerEntry `json:"answers"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return nil, false, fmt.Errorf("testsolve: verdict decode: %w", err)
	}
	unsure := verdict.NeedsHuman || verdict.InjectionSuspected
	for _, a := range verdict.Answers {
		if strings.TrimSpace(a.Answer) == "" {
			unsure = true
		}
	}
	return verdict.Answers, unsure, nil
}

// headlessArtifact renders the answers JSON stored on the assessment row.
func headlessArtifact(url, source, chat string, res FillResult, now time.Time) string {
	b, err := json.Marshal(map[string]any{
		"url":        url,
		"mode":       "headless",
		"fetched_at": now.UTC().Format(time.RFC3339),
		"source":     source,
		"chat":       chat,
		"unsure":     res.Unsure,
		"unfilled":   res.Unfilled,
		"answers":    res.Answers,
		"submitted":  res.Submit == SubmitClicked,
	})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (h *HeadlessFiller) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// --- in-page JavaScript (thin by design: extract, fill, serialize, submit) ---

// extractFieldsJS walks the flat input/textarea/select list in document
// order, groups radios by name, resolves labels (label[for] → wrapping
// label → aria-label → placeholder → preceding text) and returns a JSON
// payload with control indices the fill snippet later uses.
const extractFieldsJS = `(() => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
  const controls = [...document.querySelectorAll('input,textarea,select')];
  const idx = new Map(controls.map((c, i) => [c, i]));
  const ctxText = el => {
    let n = el;
    for (let i = 0; i < 4 && n && n.previousSibling; i++) {
      n = n.previousSibling;
      const t = norm(n.textContent);
      if (t) return t.slice(0, 200);
    }
    return '';
  };
  const labelFor = el => {
    if (el.id) {
      let l = null;
      try { l = document.querySelector('label[for="' + CSS.escape(el.id) + '"]'); } catch (e) {}
      if (l && norm(l.textContent)) return norm(l.textContent);
    }
    const wrap = el.closest('label');
    if (wrap && norm(wrap.textContent)) return norm(wrap.textContent);
    return norm(el.getAttribute('aria-label')) || norm(el.getAttribute('placeholder')) || ctxText(el);
  };
  const res = {password: false, title: norm(document.title), url: location.href, fields: []};
  const seen = new Set();
  for (const c of controls) {
    const t = c.tagName === 'INPUT' ? (c.type || 'text').toLowerCase() : (c.tagName === 'SELECT' ? 'select' : 'textarea');
    if (t === 'password') { res.password = true; continue; }
    if (['hidden','submit','button','image','reset','file','date','time'].includes(t)) continue;
    if (t === 'radio') {
      const key = 'r:' + c.name;
      if (seen.has(key)) continue;
      seen.add(key);
      const group = controls.filter(x => x.tagName === 'INPUT' && (x.type || '').toLowerCase() === 'radio' && x.name === c.name);
      res.fields.push({
        kind: 'radio', ctrls: group.map(g => idx.get(g)),
        label: labelFor(c),
        options: group.map(g => norm((g.closest('label') && g.closest('label').textContent) || g.value || '')),
        required: group.some(g => g.required)
      });
      continue;
    }
    if (t === 'checkbox') {
      res.fields.push({kind: 'check', ctrls: [idx.get(c)], label: labelFor(c), options: [], required: c.required});
      continue;
    }
    if (t === 'select') {
      res.fields.push({
        kind: 'select', ctrls: [idx.get(c)], label: labelFor(c),
        options: [...c.options].map(o => norm(o.textContent) || norm(o.value)),
        required: c.required
      });
      continue;
    }
    res.fields.push({
      kind: c.tagName === 'TEXTAREA' ? 'textarea' : 'text',
      ctrls: [idx.get(c)], label: labelFor(c), options: [],
      required: c.required, placeholder: norm(c.getAttribute('placeholder'))
    });
  }
  return JSON.stringify(res);
})()`

// fillJS applies the planned operations by control index, firing input and
// change events (React-controlled forms need the native value setter).
func fillJS(ops []fillOp) string {
	b, _ := json.Marshal(ops)
	return `(() => {
  const ops = ` + string(b) + `;
  const controls = [...document.querySelectorAll('input,textarea,select')];
  const fire = el => { for (const t of ['input', 'change']) el.dispatchEvent(new Event(t, {bubbles: true})); };
  let filled = 0;
  for (const op of ops) {
    const els = (op.ctrls || []).map(i => controls[i]).filter(Boolean);
    if (!els.length) continue;
    const el = els[0];
    if (el.disabled || el.readOnly) continue;
    if (op.kind === 'set') {
      const proto = el.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
      Object.getOwnPropertyDescriptor(proto, 'value').set.call(el, op.value);
      fire(el); filled++;
    } else if (op.kind === 'select') {
      if (el.tagName !== 'SELECT' || op.index >= el.options.length) continue;
      el.selectedIndex = op.index; fire(el); filled++;
    } else if (op.kind === 'radio') {
      el.click(); filled++;
    } else if (op.kind === 'check') {
      if (el.checked !== op.checked) el.click();
      filled++;
    }
  }
  return JSON.stringify({filled});
})()`
}

// domJS serializes the current document (the artifact; screenshots are
// forbidden by policy).
const domJS = `document.documentElement.outerHTML`

// clickSubmit clicks the first enabled submit-like control exactly once.
// ErrNoSubmitButton means no control exists — no side effect could happen.
func clickSubmit(ctx context.Context) error {
	var res string
	js := `(() => {
  const norm = s => (s || '').replace(/\s+/g, ' ').trim();
  const visible = el => {
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const cand = [...document.querySelectorAll('input[type=submit],button[type=submit],button:not([type])')]
    .find(b => !b.disabled && visible(b));
  if (!cand) return 'none';
  cand.click();
  return 'clicked:' + (norm(cand.textContent) || norm(cand.value) || 'submit');
})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &res)); err != nil {
		return err
	}
	if res == "none" {
		return ErrNoSubmitButton
	}
	return nil
}
