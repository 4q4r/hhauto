// Package testsolve implements WF5 — external test forms (draft_only).
//
// Flow: a detected form URL is first checked against the allow-list (never
// fetched otherwise), then fetched through the account proxy dialer with the
// configured page timeout, scanned for password fields (anti-phishing abort),
// parsed with a tolerant HTML form parser, and answered by the AI in batches
// of at most 5 questions. The result is a DRAFT stored on the assessment row
// plus a webhook — nothing is ever submitted from this package.
//
// headless mode (chromedp, isolated throwaway profile without hh cookies)
// is implemented by HeadlessFiller (headless.go): see Fill for the per-run
// contract. draft_only remains the default mode.
package testsolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/browser"
	"hhauto/internal/config"
	"hhauto/internal/features"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/kit11"
	"hhauto/internal/proxy"
	"hhauto/internal/queue"
	"hhauto/internal/review"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// maxBatch is the per-AI-call question limit (WF4/WF5: answers in batches ≤5).
const maxBatch = 5

// defaultPageTimeout is used when forms.page_timeout_sec is unset.
const defaultPageTimeout = 20 * time.Second

// maxPageSize caps a fetched form page (wave-3 review #6).
const maxPageSize = 2 << 20 // 2 MiB

// maxRedirects bounds the redirect chain of one fetch.
const maxRedirects = 4

// Fetch-level sentinels (wave-3 review #6).
var (
	errRedirectBlocked = errors.New("testsolve: redirect to a non-allowed host blocked")
	errPageTooLarge    = errors.New("testsolve: page exceeds the size limit")
)

// SolvePayload is the queue payload for queue.KindSolveTest.
type SolvePayload struct {
	AccountID  string
	TestID     string // one-shot identity, e.g. "ext:" + url hash
	URL        string
	Source     string // chat_message | vacancy_description
	ChatikID   string // platform chat id when detected in a chat
	VacancyID  int64  // 0 when unknown
	ResumeHash string // optional, unused by the draft flow itself
}

// TestIDForURL derives the stable external-test identity from its URL.
func TestIDForURL(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "ext:" + hex.EncodeToString(sum[:6])
}

// Solver answers external forms in draft_only mode.
type Solver struct {
	Cfg    config.Config
	DB     *store.DB
	AI     ai.Runner
	Notify webhook.Notifier
	// Dialer resolves the per-account proxy dialer; returning (nil, nil)
	// means explicitly-direct. Used by the default Fetch.
	Dialer func(accountID string) (proxy.Dialer, error)
	// Pacer is waited on before the page fetch (the testsolve "submit"
	// action). nil = no-op (wave-3 review #4).
	Pacer featurekit.Pacer
	Log   *slog.Logger
	Now   func() time.Time

	// Review is the review-queue fan-out (dedup + needs_human webhook).
	// nil = storage-only adds (unwired tests).
	Review review.Reviewer
	// Stopped reports the kill switch. nil = never stopped (unwired tests).
	// Checked between AI answer batches inside one form.
	Stopped func(ctx context.Context) bool

	// Fetch overrides page retrieval (tests). Signature: (ctx, accountID, url).
	Fetch func(ctx context.Context, accountID, url string) ([]byte, error)

	// Queue is retained by RegisterTasks (solve_test tasks are enqueued by
	// chatops; nothing is seeded on a schedule).
	Queue queue.Queue

	// Filler executes headless runs when Forms.Mode=headless. nil selects
	// the chromedp HeadlessFiller; tests inject a fake (no browser).
	Filler Filler

	// ChromeProvider resolves the browser executable through the shared
	// browser subsystem (T32: HHAUTO_CHROME_PATH → PATH → cloakbrowser
	// cache → auto-download). nil = the legacy FindChrome chain (прежнее
	// поведение).
	ChromeProvider browser.Provider
}

// Compile-time conformance with the feature-module contract.
var _ features.Feature = (*Solver)(nil)

// Name implements features.Feature.
func (s *Solver) Name() string { return "testsolve" }

// Enabled implements features.Feature.
func (s *Solver) Enabled() bool { return s.Cfg.Features.TestSolve.Enabled }

// RegisterTasks implements features.Feature.
func (s *Solver) RegisterTasks(_ context.Context, q queue.Queue, _ *scheduler.Registry) error {
	s.Queue = q
	return nil
}

// HandleSolveTest processes one queue.KindSolveTest task. Transient fetch/AI
// errors are returned so the engine can retry; anti-phishing aborts, allow-list
// rejections and parse failures are recorded in the review queue and reported
// as handled (returning them as errors would only produce a poison loop).
func (s *Solver) HandleSolveTest(ctx context.Context, task queue.Task) error {
	p, ok := task.Payload.(SolvePayload)
	if !ok {
		return fmt.Errorf("testsolve: bad payload type %T", task.Payload)
	}
	log := s.log()

	if stop, err := s.gateSolve(ctx, p); stop || err != nil {
		return err
	}

	// 2. headless delegates to the chromedp filler; draft_only is default.
	if s.Cfg.Forms.Mode == "headless" {
		return s.handleHeadless(ctx, task, p)
	}

	htmlText, stop, err := s.fetchFormHTML(ctx, task, p)
	if err != nil || stop {
		return err
	}

	form, okForm := ParseFormHTML(htmlText)

	if !okForm || len(form.Fields) == 0 {
		return s.parseFailed(ctx, p)
	}

	// 6. Candidate context (trusted block) for the AI.
	profileText, perr := kit11.RenderProfile(ctx, s.Cfg, p.AccountID, s.DB, nil)
	if perr != nil {
		return fmt.Errorf("testsolve: profile context: %w", perr)
	}

	answers, unsure, err := s.answerFields(ctx, task, p, form, profileText, log)
	if err != nil {
		return err
	}

	if err := s.saveDraft(ctx, p, answers, unsure); err != nil {
		return err
	}

	return s.finishDraft(ctx, p, unsure, len(answers), log)
}

// handleHeadless runs the chromedp flow (Mode=headless). It shares the
// draft path's gates (allow-list was checked by the caller, dedup, pacer,
// trusted profile context) and its terminal semantics: password fields and
// off-list hosts are phishing reviews, parse failures land as failed
// assessment rows, transient browser/AI errors requeue the task.
func (s *Solver) handleHeadless(ctx context.Context, task queue.Task, p SolvePayload) error {
	profileText, done, err := s.prepareFill(ctx, p)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	res, err := s.filler().Fill(ctx, FillRequest{
		AccountID:    p.AccountID,
		TestID:       p.TestID,
		URL:          p.URL,
		SubmitPolicy: s.Cfg.Forms.SubmitPolicy,
		ProfileText:  profileText,
	})
	if err != nil {
		return s.fillErrOut(ctx, task, p, err)
	}

	if err := s.storeHeadlessAssessment(ctx, p, res); err != nil {
		return err
	}

	handled, err := s.headlessOutcome(ctx, p, res)
	if err != nil || handled {
		return err
	}
	return s.headlessFinish(ctx, p, res)
}

// filler selects the injected Filler or constructs the chromedp one.
func (s *Solver) filler() Filler {
	if s.Filler != nil {
		return s.Filler
	}
	return &HeadlessFiller{
		Cfg: s.Cfg, DB: s.DB, AI: s.AI, Log: s.log(), Now: s.Now,
		Browser: s.ChromeProvider,
	}
}

// testAnswerEntry mirrors one item of the test_answer verdict.
type testAnswerEntry struct {
	Question       string `json:"question"`
	Answer         string `json:"answer"`
	BasedOnProfile bool   `json:"based_on_profile"`
}

// answerBatch runs one TaskTestAnswer call for up to maxBatch fields.
func (s *Solver) answerBatch(ctx context.Context, accountID string, fields []FormField, profileText string) ([]testAnswerEntry, bool, error) {
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
	raw, _, err := s.AI.Run(ctx, accountID, "test", questions[0].Text[:min(40, len(questions[0].Text))], "", ai.TaskTestAnswer, input)
	if err != nil {
		return nil, false, fmt.Errorf("testsolve: ai test_answer: %w", err)
	}
	var verdict struct {
		ai.Common
		Answers []testAnswerEntry `json:"answers"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return nil, false, fmt.Errorf("testsolve: verdict decode: %w", err)
	}
	unsure := verdict.NeedsHuman || verdict.InjectionSuspected
	// A question that the model refused (empty answer) also means "unsure".
	for _, a := range verdict.Answers {
		if strings.TrimSpace(a.Answer) == "" {
			unsure = true
		}
	}
	return verdict.Answers, unsure, nil
}

// alreadyDrafted reports whether an assessment for this URL already carries a
// live draft or a verdict (in_progress / solved / escalated).
func (s *Solver) alreadyDrafted(ctx context.Context, accountID, rawURL string) (bool, error) {
	rows, err := s.DB.AssessmentsListByAccount(ctx, accountID)
	if err != nil {
		return false, fmt.Errorf("testsolve: list assessments: %w", err)
	}
	for _, r := range rows {
		if r.ExternalURL == nil || *r.ExternalURL != rawURL {
			continue
		}
		switch r.Status {
		case "in_progress", "solved", "escalated":
			return true, nil
		}
	}
	return false, nil
}

// review records an abort/flag in the review queue and returns nil: these are
// terminal decisions, not retryable failures.
func (s *Solver) review(ctx context.Context, accountID, kind, reason, url string) error {
	payload := mustJSON(map[string]any{"url": url})
	if s.Review != nil {
		// Wired reviewer: dedup of open items + needs_human webhook fan-out.
		if _, err := s.Review.Add(ctx, accountID, kind, reason, payload); err != nil {
			return fmt.Errorf("testsolve: review add: %w", err)
		}
		s.log().Info("testsolve: review recorded", "kind", kind, "url", url, "account", accountID)
		return nil
	}
	if _, err := s.DB.ReviewItemsAdd(ctx, accountID, kind, reason, payload); err != nil {
		return fmt.Errorf("testsolve: review add: %w", err)
	}
	s.log().Info("testsolve: review recorded", "kind", kind, "url", url, "account", accountID)
	return nil
}

// requeue puts the task back with its original dedup key (no-op when the
// queue handle is not wired).
func (s *Solver) requeue(ctx context.Context, task queue.Task) {
	if s.Queue == nil {
		return
	}
	if err := s.Queue.Enqueue(ctx, task); err != nil {
		s.log().Warn("testsolve: requeue failed", "task", task.ID, "err", err)
	}
}

// fetch retrieves the form page through the default HTTP fetcher unless an
// injected Fetch is set (tests).
func (s *Solver) fetch(ctx context.Context, accountID, rawURL string) ([]byte, error) {
	if s.Fetch != nil {
		return s.Fetch(ctx, accountID, rawURL)
	}
	timeout := time.Duration(s.Cfg.Forms.PageTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultPageTimeout
	}
	var dialer proxy.Dialer
	if s.Dialer != nil {
		d, err := s.Dialer(accountID)
		if err != nil {
			return nil, fmt.Errorf("testsolve: proxy dialer: %w", err)
		}
		dialer = d // may be nil: explicitly-direct
	}
	tr := &http.Transport{Proxy: nil}
	if dialer != nil {
		tr.DialContext = dialer.DialContext
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// Wave-3 review #6: every redirect target must stay on the
		// allow-list — a bounce to a foreign host is refused. The check
		// uses URL.Host (host[:port]) so different ports are different
		// hosts; default-port URLs carry the bare domain and match the
		// domain allow-list as usual.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("testsolve: stopped after %d redirects", maxRedirects)
			}
			if !kit11.HostAllowed(strings.ToLower(req.URL.Host), s.Cfg.Forms.AllowedDomains) {
				return errRedirectBlocked
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	setBrowserUA(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPageSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPageSize {
		return nil, errPageTooLarge
	}
	return data, nil
}

// setBrowserUA sets a generic browser User-Agent; external form sites
// commonly reject empty or bot UAs. The value is NOT the hh session UA.
func setBrowserUA(req *http.Request) {
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
}

func (s *Solver) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Solver) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// hostPortOf is hostOf but keeps an explicit port ("example.com:8443").
func hostPortOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func intPtr(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// gateSolve runs the shared pre-flight gates: the anti-phishing host
// allow-list and the draft dedup (a test with a live draft is not re-solved).
// stop=true means the task is fully handled.
func (s *Solver) gateSolve(ctx context.Context, p SolvePayload) (bool, error) {
	// 1. Anti-phishing gate: only allow-listed hosts are ever fetched.
	// URL.Host (host[:port]) is the key — consistent with the redirect
	// policy below; default-port URLs carry the bare domain.
	host := hostPortOf(p.URL)
	if !kit11.HostAllowed(host, s.Cfg.Forms.AllowedDomains) {
		return true, s.review(ctx, p.AccountID, "phishing_suspect", "testsolve: domain not allowed: "+host, p.URL)
	}

	// 3. Dedup: a test that already has a live draft/verdict is not re-solved.
	done, err := s.alreadyDrafted(ctx, p.AccountID, p.URL)
	if err != nil {
		return false, err
	}
	if done {
		s.log().Info("testsolve: draft already exists, skipping", "url", p.URL, "account", p.AccountID)
		return true, nil
	}
	return false, nil
}

// fetchFormHTML waits the pacer, fetches the form page and applies the
// password-field anti-phishing gate. Off-list redirects and password fields
// are terminal reviews; transient fetch failures requeue the task.
func (s *Solver) fetchFormHTML(ctx context.Context, task queue.Task, p SolvePayload) (string, bool, error) {
	// 4. Fetch the page (pacer waited before the action — wave-3 review #4).
	if s.Pacer != nil {
		if err := s.Pacer.Wait(ctx, p.AccountID); err != nil {
			return "", false, fmt.Errorf("testsolve: pacer: %w", err)
		}
	}
	body, err := s.fetch(ctx, p.AccountID, p.URL)
	if err != nil {
		if errors.Is(err, errRedirectBlocked) {
			// A redirect outside the allow-list is a security refusal:
			// permanent, recorded, never requeued.
			return "", true, s.review(ctx, p.AccountID, "phishing_suspect",
				"testsolve: redirect to a non-allowed host — refused", p.URL)
		}
		// Transient fetch failure: re-enqueue the same dedup key — the queue
		// released it after dequeue (wave-3 review #5).
		s.requeue(ctx, task)
		return "", false, fmt.Errorf("testsolve: fetch %s: %w", p.URL, err)
	}
	htmlText := string(body)

	// 5. Anti-phishing: password field anywhere on the page aborts the flow.
	if hasPasswordField(htmlText) {
		// Anti-phishing abort: the review is the task outcome, nothing further.
		return "", true, s.review(ctx, p.AccountID, "phishing_suspect",
			"testsolve: password field on the form page — suspected phishing", p.URL)
	}
	return htmlText, false, nil
}

// parseFailed records a failed assessment row and the review item for a
// form that yielded no answerable fields.
func (s *Solver) parseFailed(ctx context.Context, p SolvePayload) error {
	{
		aerr := s.DB.AssessmentsAdd(ctx, &store.Assessment{
			AccountID:   p.AccountID,
			VacancyID:   intPtr(p.VacancyID),
			Kind:        "external_form",
			Provider:    strPtr(formKindForURL(p.URL)),
			ExternalURL: &p.URL,
			Status:      "failed",
			Result:      strPtr("parse_failed: no answerable fields found"),
		})
		if aerr != nil {
			return fmt.Errorf("testsolve: store failed assessment: %w", aerr)
		}
		return s.review(ctx, p.AccountID, "test_parse_failed",
			"testsolve: form could not be parsed", p.URL)
	}
}

// answerFields answers the form in batches of <=5 with the kill-switch
// check between batches; the draft keeps whatever was answered so far.
func (s *Solver) answerFields(ctx context.Context, task queue.Task, p SolvePayload, form Form, profileText string, log *slog.Logger) ([]testAnswerEntry, bool, error) {
	// 7. Answers in batches of ≤5.
	var answers []testAnswerEntry
	unsure := false
	for start := 0; start < len(form.Fields); start += maxBatch {
		if s.Stopped != nil && s.Stopped(ctx) {
			// Kill switch: no further AI answer batches; the draft is
			// stored with the answers gathered so far (graceful exit).
			log.Info("testsolve: kill switch tripped — stopping the answer batches", "url", p.URL)
			break
		}
		end := min(start+maxBatch, len(form.Fields))
		batchAnswers, batchUnsure, err := s.answerBatch(ctx, p.AccountID, form.Fields[start:end], profileText)
		if err != nil {
			s.requeue(ctx, task) // AI outage must not lose the task (#5)
			return nil, false, err
		}
		answers = append(answers, batchAnswers...)
		unsure = unsure || batchUnsure
	}
	return answers, unsure, nil
}

// saveDraft persists the draft artifact on a new assessment row; an unsure
// verdict escalates the row status.
func (s *Solver) saveDraft(ctx context.Context, p SolvePayload, answers []testAnswerEntry, unsure bool) error {
	// 8. Draft artifact on the assessment row. Nothing is submitted.
	artifact, err := json.Marshal(map[string]any{
		"url":        p.URL,
		"mode":       "draft_only",
		"provider":   formKindForURL(p.URL),
		"fetched_at": s.now().UTC().Format(time.RFC3339),
		"source":     p.Source,
		"chat":       p.ChatikID,
		"unsure":     unsure,
		"answers":    answers,
	})
	if err != nil {
		return fmt.Errorf("testsolve: artifact marshal: %w", err)
	}
	status := "in_progress"
	if unsure {
		status = "escalated"
	}
	aerr := s.DB.AssessmentsAdd(ctx, &store.Assessment{
		AccountID:   p.AccountID,
		VacancyID:   intPtr(p.VacancyID),
		Kind:        "external_form",
		Provider:    strPtr(formKindForURL(p.URL)),
		ExternalURL: &p.URL,
		Status:      status,
		Answers:     strPtr(string(artifact)),
	})
	if aerr != nil {
		return fmt.Errorf("testsolve: store assessment: %w", aerr)
	}
	return nil
}

// finishDraft emits the unsure review/notification or the draft-ready
// metrics, notification and log line.
func (s *Solver) finishDraft(ctx context.Context, p SolvePayload, unsure bool, answers int, log *slog.Logger) error {
	if unsure {
		if err := s.review(ctx, p.AccountID, "test_unsure",
			"testsolve: AI unsure about form answers — draft saved, human decision needed", p.URL); err != nil {
			return err
		}
		if s.Notify != nil {
			s.Notify.Emit(webhook.Event{
				Type:      webhook.EvNeedsHuman,
				AccountID: p.AccountID,
				Time:      s.now(),
				Message:   "Внешний тест: черновик готов, требуется проверка",
				Payload:   map[string]any{"url": p.URL, "draft": true},
			})
		}
		return nil
	}

	_ = s.DB.MetricsIncr(ctx, "testsolve.draft", kit11.Day(s.now()))
	if s.Notify != nil {
		s.Notify.Emit(webhook.Event{
			Type:      webhook.EvTestSolved,
			AccountID: p.AccountID,
			Time:      s.now(),
			Message:   "Внешний тест: черновик ответов готов (draft_only, отправка человеком)",
			Payload:   map[string]any{"url": p.URL, "draft": true, "answers": answers},
		})
	}
	log.Info("testsolve: draft saved", "url", p.URL, "account", p.AccountID, "answers", answers)
	return nil
}

// prepareFill runs the shared headless pre-flight: draft dedup, pacer and
// the trusted profile context for the AI.
func (s *Solver) prepareFill(ctx context.Context, p SolvePayload) (string, bool, error) {
	// Dedup: a test that already has a live draft/verdict is not re-solved.
	done, err := s.alreadyDrafted(ctx, p.AccountID, p.URL)
	if err != nil {
		return "", false, err
	}
	if done {
		s.log().Info("testsolve: draft already exists, skipping", "url", p.URL, "account", p.AccountID)
		return "", true, nil
	}

	// Pacer before the browser action.
	if s.Pacer != nil {
		if err := s.Pacer.Wait(ctx, p.AccountID); err != nil {
			return "", false, fmt.Errorf("testsolve: pacer: %w", err)
		}
	}

	// Trusted candidate context for the AI.
	profileText, perr := kit11.RenderProfile(ctx, s.Cfg, p.AccountID, s.DB, nil)
	if perr != nil {
		return "", false, fmt.Errorf("testsolve: profile context: %w", perr)
	}
	return profileText, false, nil
}

// fillErrOut maps a failed chromedp fill to its terminal semantics:
// password/off-list hosts are phishing reviews, missing fields become failed
// assessments, transient browser/AI errors requeue the task.
func (s *Solver) fillErrOut(ctx context.Context, task queue.Task, p SolvePayload, err error) error {
	switch {
	case errors.Is(err, ErrPasswordDetected):
		// Anti-phishing abort — terminal, same as the draft path.
		return s.review(ctx, p.AccountID, "phishing_suspect",
			"testsolve: password field on the form page — suspected phishing", p.URL)
	case errors.Is(err, ErrHostDisallowed):
		// A redirect/navigation outside the allow-list is a security
		// refusal — terminal, never requeued.
		return s.review(ctx, p.AccountID, "phishing_suspect",
			"testsolve: page landed on a non-allowed host — refused", p.URL)
	case errors.Is(err, ErrNoFields):
		aerr := s.DB.AssessmentsAdd(ctx, &store.Assessment{
			AccountID:   p.AccountID,
			VacancyID:   intPtr(p.VacancyID),
			Kind:        "external_form",
			Provider:    strPtr(formKindForURL(p.URL)),
			ExternalURL: &p.URL,
			Status:      "failed",
			Result:      strPtr("parse_failed: no answerable fields found"),
		})
		if aerr != nil {
			return fmt.Errorf("testsolve: store failed assessment: %w", aerr)
		}
		return s.review(ctx, p.AccountID, "test_parse_failed",
			"testsolve: form could not be parsed", p.URL)
	}
	// Transient browser/AI error: re-enqueue the same dedup key.
	s.requeue(ctx, task)
	return fmt.Errorf("testsolve: headless fill %s: %w", p.URL, err)
}

// storeHeadlessAssessment builds the artifact and stores the assessment row
// with the submit-state derived status.
func (s *Solver) storeHeadlessAssessment(ctx context.Context, p SolvePayload, res FillResult) error {
	// Artifacts on the assessment row: answers JSON + serialized DOM.
	artifact := headlessArtifact(p.URL, p.Source, p.ChatikID, res, s.now())
	status := "in_progress"
	switch {
	case res.Submit == SubmitClicked:
		status = "solved"
	case res.Submit == SubmitAlreadyDone || res.Submit == SubmitAlreadyPending || res.Unsure:
		status = "escalated"
	}
	if aerr := s.DB.AssessmentsAdd(ctx, &store.Assessment{
		AccountID:   p.AccountID,
		VacancyID:   intPtr(p.VacancyID),
		Kind:        "external_form",
		Provider:    strPtr(formKindForURL(p.URL)),
		ExternalURL: &p.URL,
		Status:      status,
		Answers:     strPtr(artifact),
		Result:      strPtr(res.DOM),
	}); aerr != nil {
		return fmt.Errorf("testsolve: store assessment: %w", aerr)
	}
	return nil
}

// headlessOutcome handles the already-consumed-intent and unsure outcomes:
// both are terminal hand-offs to a human (never a second click).
func (s *Solver) headlessOutcome(ctx context.Context, p SolvePayload, res FillResult) (bool, error) {
	switch {
	case res.Submit == SubmitAlreadyDone || res.Submit == SubmitAlreadyPending:
		// The extsubmit intent was consumed by a previous/parallel run:
		// never click again, hand the uncertainty to a human.
		if rerr := s.review(ctx, p.AccountID, "test_submit_uncertain",
			"testsolve: submit intent already consumed — replay forbidden, verify the form outcome manually", p.URL); rerr != nil {
			return false, rerr
		}
		if s.Notify != nil {
			s.Notify.Emit(webhook.Event{
				Type:      webhook.EvNeedsHuman,
				AccountID: p.AccountID,
				Time:      s.now(),
				Message:   "Внешний тест: отправка не подтверждена (повтор запрещён), нужна проверка",
				Payload:   map[string]any{"url": p.URL, "submit_state": res.Submit.String()},
			})
		}
		return true, nil
	case res.Unsure:
		if rerr := s.review(ctx, p.AccountID, "test_unsure",
			"testsolve: AI unsure or fields unfilled — draft saved, human decision needed", p.URL); rerr != nil {
			return false, rerr
		}
		if s.Notify != nil {
			s.Notify.Emit(webhook.Event{
				Type:      webhook.EvNeedsHuman,
				AccountID: p.AccountID,
				Time:      s.now(),
				Message:   "Внешний тест: черновик готов, требуется проверка",
				Payload:   map[string]any{"url": p.URL, "draft": true},
			})
		}
		return true, nil
	}
	return false, nil
}

// headlessFinish reports the terminal headless state: submitted or draft.
func (s *Solver) headlessFinish(ctx context.Context, p SolvePayload, res FillResult) error {
	if res.Submit == SubmitClicked {
		_ = s.DB.MetricsIncr(ctx, "testsolve.submit", kit11.Day(s.now()))
		if s.Notify != nil {
			s.Notify.Emit(webhook.Event{
				Type:      webhook.EvTestSolved,
				AccountID: p.AccountID,
				Time:      s.now(),
				Message:   "Внешний тест: заполнен и отправлен (headless, submit_policy=auto)",
				Payload:   map[string]any{"url": p.URL, "submitted": true, "answers": len(res.Answers)},
			})
		}
		s.log().Info("testsolve: submitted", "url", p.URL, "account", p.AccountID)
		return nil
	}

	_ = s.DB.MetricsIncr(ctx, "testsolve.draft", kit11.Day(s.now()))
	if s.Notify != nil {
		s.Notify.Emit(webhook.Event{
			Type:      webhook.EvTestSolved,
			AccountID: p.AccountID,
			Time:      s.now(),
			Message:   "Внешний тест: черновик готов (headless, отправка человеком)",
			Payload:   map[string]any{"url": p.URL, "draft": true, "answers": len(res.Answers)},
		})
	}
	s.log().Info("testsolve: headless draft saved", "url", p.URL, "account", p.AccountID, "answers", len(res.Answers))
	return nil
}
