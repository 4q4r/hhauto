// Package testsolve implements WF5 — external test forms (draft_only).
//
// Flow: a detected form URL is first checked against the allow-list (never
// fetched otherwise), then fetched through the account proxy dialer with the
// configured page timeout, scanned for password fields (anti-phishing abort),
// parsed with a tolerant HTML form parser, and answered by the AI in batches
// of at most 5 questions. The result is a DRAFT stored on the assessment row
// plus a webhook — nothing is ever submitted from this package.
//
// headless mode (chromedp, isolated context without hh cookies) is a separate
// wiring step (T13): this package returns an explicit stub error for it.
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

	"golang.org/x/net/html/charset"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features"
	"hhauto/internal/features/kit11"
	"hhauto/internal/proxy"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// maxBatch is the per-AI-call question limit (WF4/WF5: answers in batches ≤5).
const maxBatch = 5

// defaultPageTimeout is used when forms.page_timeout_sec is unset.
const defaultPageTimeout = 20 * time.Second

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
	Log    *slog.Logger
	Now    func() time.Time

	// Fetch overrides page retrieval (tests). Signature: (ctx, accountID, url).
	Fetch func(ctx context.Context, accountID, url string) ([]byte, error)

	// Queue is retained by RegisterTasks (solve_test tasks are enqueued by
	// chatops; nothing is seeded on a schedule).
	Queue queue.Queue
}

// Compile-time conformance with the feature-module contract.
var _ features.Feature = (*Solver)(nil)

// Name implements features.Feature.
func (s *Solver) Name() string { return "testsolve" }

// Enabled implements features.Feature.
func (s *Solver) Enabled() bool { return s.Cfg.Features.TestSolve.Enabled }

// RegisterTasks implements features.Feature.
func (s *Solver) RegisterTasks(ctx context.Context, q queue.Queue, _ *scheduler.Registry) error {
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

	// 1. Anti-phishing gate: only allow-listed hosts are ever fetched.
	host := hostOf(p.URL)
	if !kit11.HostAllowed(host, s.Cfg.Forms.AllowedDomains) {
		return s.review(ctx, p.AccountID, "phishing_suspect", "testsolve: domain not allowed: "+host, p.URL)
	}

	// 2. headless is not implemented here by design (T13 wires chromedp).
	if s.Cfg.Forms.Mode == "headless" {
		return errors.New("headless mode: T13 wiring")
	}

	// 3. Dedup: a test that already has a live draft/verdict is not re-solved.
	done, err := s.alreadyDrafted(ctx, p.AccountID, p.URL)
	if err != nil {
		return err
	}
	if done {
		log.Info("testsolve: draft already exists, skipping", "url", p.URL, "account", p.AccountID)
		return nil
	}

	// 4. Fetch the page.
	body, err := s.fetch(ctx, p.AccountID, p.URL)
	if err != nil {
		return fmt.Errorf("testsolve: fetch %s: %w", p.URL, err)
	}
	htmlText := string(body)

	// 5. Anti-phishing: password field anywhere on the page aborts the flow.
	if hasPasswordField(htmlText) {
		return s.review(ctx, p.AccountID, "phishing_suspect",
			"testsolve: password field on the form page — suspected phishing", p.URL)
	}

	form, okForm := ParseFormHTML(htmlText)
	if !okForm || len(form.Fields) == 0 {
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

	// 6. Candidate context (trusted block) for the AI.
	profileText, perr := kit11.RenderProfile(ctx, s.Cfg, p.AccountID, s.DB, nil)
	if perr != nil {
		return fmt.Errorf("testsolve: profile context: %w", perr)
	}

	// 7. Answers in batches of ≤5.
	var answers []testAnswerEntry
	unsure := false
	for start := 0; start < len(form.Fields); start += maxBatch {
		end := min(start+maxBatch, len(form.Fields))
		batchAnswers, batchUnsure, err := s.answerBatch(ctx, p.AccountID, form.Fields[start:end], profileText)
		if err != nil {
			return err
		}
		answers = append(answers, batchAnswers...)
		unsure = unsure || batchUnsure
	}

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
			Payload:   map[string]any{"url": p.URL, "draft": true, "answers": len(answers)},
		})
	}
	log.Info("testsolve: draft saved", "url", p.URL, "account", p.AccountID, "answers", len(answers))
	return nil
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
	if _, err := s.DB.ReviewItemsAdd(ctx, accountID, kind, reason,
		mustJSON(map[string]any{"url": url})); err != nil {
		return fmt.Errorf("testsolve: review add: %w", err)
	}
	s.log().Info("testsolve: review recorded", "kind", kind, "url", url, "account", accountID)
	return nil
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
	client := &http.Client{Transport: tr, Timeout: timeout}

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
	return readBody(resp)
}

// setBrowserUA sets a generic browser User-Agent; external form sites
// commonly reject empty or bot UAs. The value is NOT the hh session UA.
func setBrowserUA(req *http.Request) {
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
}

// readBody reads the response body, decoding to UTF-8 when a non-UTF-8
// charset is declared (legacy form pages sometimes serve windows-1251).
func readBody(resp *http.Response) ([]byte, error) {
	r, err := charset.NewReader(resp.Body, resp.Header.Get("Content-Type"))
	if err != nil {
		return io.ReadAll(resp.Body)
	}
	return io.ReadAll(r)
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

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
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
