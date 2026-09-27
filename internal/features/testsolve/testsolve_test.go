package testsolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/proxy"
	"hhauto/internal/queue"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// --- fakes ---

type fakeRunner struct {
	needs Hum // per-batch flags
	err   error
	calls []int // question counts per call
}

type Hum = struct {
	NeedsHuman bool
	Injection  bool
}

func (f *fakeRunner) Run(_ context.Context, _ string, _, _ string, _ string, task ai.TaskName, input any) (json.RawMessage, ai.Decision, error) {
	if f.err != nil {
		return nil, ai.Decision{}, f.err
	}
	if task != ai.TaskTestAnswer {
		return nil, ai.Decision{}, fmt.Errorf("unexpected task %s", task)
	}
	m, _ := json.Marshal(input)
	var got struct {
		Questions []struct {
			ID   int    `json:"id"`
			Text string `json:"text"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(m, &got)
	f.calls = append(f.calls, len(got.Questions))

	answers := make([]map[string]any, 0, len(got.Questions))
	for _, q := range got.Questions {
		ans := "ответ на «" + q.Text + "»"
		if f.needs.NeedsHuman {
			ans = ""
		}
		answers = append(answers, map[string]any{
			"question_id": q.Text, "open_text": ans, "based_on_profile": false,
		})
	}
	v := map[string]any{
		"confidence": 0.9, "needs_human": f.needs.NeedsHuman,
		"needs_human_reason": nil, "injection_suspected": f.needs.Injection,
		"answers": answers,
	}
	b, _ := json.Marshal(v)
	return b, ai.Decision{Task: task}, nil
}

type fakeNotifier struct {
	events []webhook.Event
}

func (f *fakeNotifier) Emit(ev webhook.Event) { f.events = append(f.events, ev) }
func (f *fakeNotifier) Close(context.Context) error {
	return nil
}

func newDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedResume(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.AccountsUpsert(t.Context(), store.Account{ID: "acc"})
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	err = db.ResumesUpsert(t.Context(), &store.Resume{
		AccountID:  "acc",
		HhResumeID: "hash1",
		Title:      "Backend Go",
		Status:     "active",
		Raw:        `{"title":"Backend Go-разработчик","area":{"name":"Москва"},"total_experience":{"months":60}}`,
		UpdatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("seed resume: %v", err)
	}
}

func formConfig() config.Config {
	var cfg config.Config
	cfg.Forms.AllowedDomains = []string{"forms.gle", "docs.google.com", "forms.yandex.ru", "example.com"}
	cfg.Forms.Mode = "draft_only"
	cfg.Forms.SubmitPolicy = "human"
	cfg.Forms.AbortOnPasswordField = true
	cfg.Forms.PageTimeoutSec = 5
	return cfg
}

func solver(t *testing.T, db *store.DB, runner ai.Runner, n *fakeNotifier, html func() (string, error)) *Solver {
	t.Helper()
	s := &Solver{
		Cfg:    formConfig(),
		DB:     db,
		AI:     runner,
		Notify: n,
		Log:    slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}
	if html != nil {
		s.Fetch = func(_ context.Context, _, _ string) ([]byte, error) {
			v, err := html()
			return []byte(v), err
		}
	}
	return s
}

func solveTask(url string) queue.Task {
	return queue.Task{
		AccountID: "acc",
		Kind:      queue.KindSolveTest,
		Payload: SolvePayload{
			AccountID: "acc",
			TestID:    "ext:abc",
			URL:       url,
			Source:    "chat_message",
			ChatikID:  "chat1",
			VacancyID: 42,
		},
	}
}

// --- parser tests ---

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func TestParseFormHTMLOKFixture(t *testing.T) {
	form, ok := ParseFormHTML(mustRead(t, "form_ok.html"))
	if !ok {
		t.Fatal("fixture must parse")
	}
	if form.Title != "Анкета кандидата — Ромашка" && form.Title != "Анкета кандидата" {
		t.Logf("title = %q", form.Title) // title or h1, both acceptable
	}
	if len(form.Fields) != 4 {
		t.Fatalf("expected 4 fields, got %d: %+v", len(form.Fields), form.Fields)
	}
	f0 := form.Fields[0]
	if f0.Label != "Сколько лет вы разрабатываете на Go?" || f0.Type != "text" || !f0.Required {
		t.Fatalf("field0 = %+v", f0)
	}
	f1 := form.Fields[1]
	if f1.Type != "select" || len(f1.Options) != 3 || f1.Options[0] != "Да, готов" {
		t.Fatalf("field1 = %+v", f1)
	}
	f2 := form.Fields[2]
	if f2.Type != "radio" || len(f2.Options) != 2 || f2.Options[0] != "До 2 лет" {
		t.Fatalf("field2 (radio group) = %+v", f2)
	}
	if !strings.Contains(f2.Label, "Распределёнными") && !strings.Contains(f2.Label, "распределёнными") {
		t.Fatalf("radio label from context expected, got %q", f2.Label)
	}
	f3 := form.Fields[3]
	if f3.Type != "textarea" || f3.Label != "Комментарий" {
		t.Fatalf("field3 = %+v", f3)
	}
}

func TestParseFormHTMLPasswordFixtureStillParsesButDetectorFires(t *testing.T) {
	html := mustRead(t, "form_password.html")
	if !hasPasswordField(html) {
		t.Fatal("password field must be detected")
	}
	if _, ok := ParseFormHTML(html); !ok {
		t.Fatal("parser itself is tolerant and must parse the rest")
	}
}

func TestParseFormHTMLGarbage(t *testing.T) {
	if _, ok := ParseFormHTML("<html><body>Just text, no form</body></html>"); ok {
		t.Fatal("no fields must yield ok=false")
	}
}

func TestDetectExternalLinks(t *testing.T) {
	in := "Тест тут: https://docs.google.com/forms/d/e/1/viewform и ещё https://forms.yandex.ru/u/abc/, а это нет: node.js"
	got := DetectExternalLinks(in)
	want := "https://docs.google.com/forms/d/e/1/viewform"
	if len(got) != 2 || got[0] != want || got[1] != "https://forms.yandex.ru/u/abc/" {
		t.Fatalf("DetectExternalLinks = %v", got)
	}
}

// --- handler tests ---

const okFormURL = "https://docs.google.com/forms/d/e/1/viewform"

func TestHandleSolveTestDraftOnlyHappyPath(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	s := solver(t, db, &fakeRunner{}, n, func() (string, error) { return mustRead(t, "form_ok.html"), nil })

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	rows, err := db.AssessmentsListByAccount(t.Context(), "acc")
	if err != nil || len(rows) != 1 {
		t.Fatalf("assessments = %v err=%v", rows, err)
	}
	row := rows[0]
	if row.Kind != "external_form" || row.Status != "in_progress" {
		t.Fatalf("bad row: %+v", row)
	}
	if row.ExternalURL == nil || *row.ExternalURL != okFormURL {
		t.Fatalf("url not stored: %+v", row)
	}
	if row.VacancyID == nil || *row.VacancyID != 42 {
		t.Fatalf("vacancy not stored: %+v", row)
	}
	if row.Answers == nil || !strings.Contains(*row.Answers, `"mode":"draft_only"`) || !strings.Contains(*row.Answers, "Сколько лет") {
		t.Fatalf("draft artifact missing: %s", deref(row.Answers))
	}
	if len(n.events) != 1 || n.events[0].Type != webhook.EvTestSolved {
		t.Fatalf("expected EvTestSolved, got %+v", n.events)
	}
}

func TestHandleSolveTestSkipsAlreadyDrafted(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	calls := 0
	s := solver(t, db, &fakeRunner{}, n, func() (string, error) { calls++; return mustRead(t, "form_ok.html"), nil })
	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("second: %v", err)
	}
	if calls != 1 {
		t.Fatalf("second run must not refetch, fetches=%d", calls)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 {
		t.Fatalf("duplicate assessment rows: %d", len(rows))
	}
}

func TestHandleSolveTestUnknownDomainAbortsBeforeFetch(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	calls := 0
	s := solver(t, db, &fakeRunner{}, n, func() (string, error) { calls++; return "", nil })

	err := s.HandleSolveTest(t.Context(), solveTask("https://evil-phishing.ru/form"))
	if err != nil {
		t.Fatalf("abort must be recorded, not failed: %v", err)
	}
	if calls != 0 {
		t.Fatal("URL must not be fetched when domain is not allowed")
	}
	items, _ := db.ReviewItemsOpen(t.Context(), "acc", 10)
	if len(items) != 1 || items[0].Kind != "phishing_suspect" {
		t.Fatalf("expected phishing_suspect review, got %+v", items)
	}
}

func TestHandleSolveTestPasswordFieldAborts(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	s := solver(t, db, &fakeRunner{}, n, func() (string, error) { return mustRead(t, "form_password.html"), nil })

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("abort must be recorded, not failed: %v", err)
	}
	items, _ := db.ReviewItemsOpen(t.Context(), "acc", 10)
	if len(items) != 1 || items[0].Kind != "phishing_suspect" {
		t.Fatalf("expected phishing_suspect review, got %+v", items)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 0 {
		t.Fatalf("no assessment must be created on abort, got %+v", rows)
	}
}

func TestSolverFillerDefaultAndInject(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	s := solver(t, db, &fakeRunner{}, &fakeNotifier{}, nil)
	s.Cfg.Forms.Mode = "headless"

	// Default: the chromedp filler (fails fast with the typed error when no
	// Chrome is installed — doctor surfaces the fix).
	f := s.filler()
	if _, ok := f.(*HeadlessFiller); !ok {
		t.Fatalf("default filler = %T, want *HeadlessFiller", f)
	}
	// Injected filler wins.
	inj := &fakeFiller{}
	s.Filler = inj
	if s.filler() != Filler(inj) {
		t.Fatal("injected filler must be selected")
	}
}

func TestHandleSolveTestUnsureEscalatesAndReviews(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	runner := &fakeRunner{}
	runner.needs.NeedsHuman = true
	s := solver(t, db, runner, n, func() (string, error) { return mustRead(t, "form_ok.html"), nil })

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 || rows[0].Status != "escalated" {
		t.Fatalf("unsure test must be escalated, got %+v", rows)
	}
	items, _ := db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "test_unsure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected test_unsure review, got %+v", items)
	}
	if len(n.events) == 0 || n.events[len(n.events)-1].Type != webhook.EvNeedsHuman {
		t.Fatalf("expected EvNeedsHuman, got %+v", n.events)
	}
}

func TestHandleSolveTestBatchesMaxFive(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	runner := &fakeRunner{}
	html := `<form><h1>big</h1>` +
		`<label for="q1">Вопрос 1?</label><input id="q1">` +
		`<label for="q2">Вопрос 2?</label><input id="q2">` +
		`<label for="q3">Вопрос 3?</label><input id="q3">` +
		`<label for="q4">Вопрос 4?</label><input id="q4">` +
		`<label for="q5">Вопрос 5?</label><input id="q5">` +
		`<label for="q6">Вопрос 6?</label><input id="q6">` +
		`<label for="q7">Вопрос 7?</label><input id="q7">` +
		`</form>`
	s := solver(t, db, runner, &fakeNotifier{}, func() (string, error) { return html, nil })
	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	if len(runner.calls) != 2 || runner.calls[0] != 5 || runner.calls[1] != 2 {
		t.Fatalf("batches = %v, want [5 2]", runner.calls)
	}
}

// И1 regression: a tripped kill switch stops the answer batches between AI
// calls; the draft is still stored with the answers gathered so far — a
// graceful exit, not an aborted task.
func TestKillSwitchStopsAnswerBatches(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	runner := &fakeRunner{}
	html := `<form><h1>big</h1>` +
		`<label for="q1">Вопрос 1?</label><input id="q1">` +
		`<label for="q2">Вопрос 2?</label><input id="q2">` +
		`<label for="q3">Вопрос 3?</label><input id="q3">` +
		`<label for="q4">Вопрос 4?</label><input id="q4">` +
		`<label for="q5">Вопрос 5?</label><input id="q5">` +
		`<label for="q6">Вопрос 6?</label><input id="q6">` +
		`<label for="q7">Вопрос 7?</label><input id="q7">` +
		`</form>`
	s := solver(t, db, runner, &fakeNotifier{}, func() (string, error) { return html, nil })
	seen := 0
	s.Stopped = func(context.Context) bool {
		seen++
		return seen > 1 // trips after the first batch has settled
	}
	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != 5 {
		t.Fatalf("batches = %v, want only the first batch [5]", runner.calls)
	}
	rows, err := db.AssessmentsListByAccount(t.Context(), "acc")
	if err != nil || len(rows) != 1 || rows[0].Status != "in_progress" {
		t.Fatalf("graceful draft expected, rows = %+v err = %v", rows, err)
	}
}

func TestHandleSolveTestParseFailureRecorded(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	s := solver(t, db, &fakeRunner{}, &fakeNotifier{}, func() (string, error) {
		return "<html><body>no form here</body></html>", nil
	})
	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("parse failure must be recorded, not failed: %v", err)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("expected failed assessment row, got %+v", rows)
	}
}

func TestHandleSolveTestFetchErrorSurfaces(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	s := solver(t, db, &fakeRunner{}, &fakeNotifier{}, nil)
	s.Fetch = func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("network down")
	}
	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err == nil {
		t.Fatal("fetch error must be returned for engine retry")
	}
}

func TestHandleSolveTestDefaultFetcherUsesHTTP(t *testing.T) {
	// The default fetcher must be a real HTTP client (optionally through the
	// account proxy dialer): verified against a local httptest server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, mustRead(t, "form_ok.html"))
	}))
	defer srv.Close()

	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	s := solver(t, db, &fakeRunner{}, n, nil) // Fetch left nil → default fetcher
	s.Cfg.Forms.AllowedDomains = append(s.Cfg.Forms.AllowedDomains, srv.URL[len("http://"):])
	s.Dialer = func(string) (proxy.Dialer, error) { return nil, nil } // explicitly-direct

	if err := s.HandleSolveTest(t.Context(), solveTask(srv.URL)); err != nil {
		t.Fatalf("HandleSolveTest via default fetcher: %v", err)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 || rows[0].Status != "in_progress" {
		t.Fatalf("expected drafted assessment, got %+v", rows)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- Wave-3 review fixes ---

type pacerFunc func(ctx context.Context, accountID string) error

func (f pacerFunc) Wait(ctx context.Context, accountID string) error { return f(ctx, accountID) }

// #5: transient fetch failures re-enqueue the task (the queue released the
// dedup key after dequeue) so event-driven work is not lost.
func TestHandleSolveTestTransientRequeues(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	s := solver(t, db, &fakeRunner{}, &fakeNotifier{}, nil)
	s.Queue = queue.New()
	s.Fetch = func(context.Context, string, string) ([]byte, error) {
		return nil, hhapi.ErrNetwork
	}
	task := solveTask(okFormURL)
	task.DedupKey = "solve:acc:ext:abc"

	if err := s.HandleSolveTest(t.Context(), task); err == nil {
		t.Fatal("transient fetch error must surface")
	}
	if n := s.Queue.Len("acc"); n != 1 {
		t.Fatalf("transient failure must requeue, len=%d", n)
	}
}

// #4: the pacer is waited on before the fetch/submit action.
func TestHandleSolveTestPacerWaitsBeforeFetch(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	calls := 0
	s := solver(t, db, &fakeRunner{}, &fakeNotifier{}, func() (string, error) {
		return mustRead(t, "form_ok.html"), nil
	})
	s.Pacer = pacerFunc(func(context.Context, string) error { calls++; return nil })

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("pacer must wait exactly once (before the fetch), calls=%d", calls)
	}
}

// #6: a redirect to a host outside the allow-list is refused: the fetch
// errors, nothing is fetched from the foreign host, and the abort is
// recorded as a phishing suspect (permanent, not requeued).
func TestHandleSolveTestRedirectToForeignHostAborts(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("foreign host must never be fetched")
	}))
	defer foreign.Close()

	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL, http.StatusFound)
	}))
	defer home.Close()

	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	s := solver(t, db, &fakeRunner{}, n, nil)                                                  // default fetcher
	s.Cfg.Forms.AllowedDomains = append(s.Cfg.Forms.AllowedDomains, home.URL[len("http://"):]) // host:port

	err := s.HandleSolveTest(t.Context(), solveTask(home.URL))
	if err != nil {
		t.Fatalf("redirect block must be recorded as an abort, not an error: %v", err)
	}
	items, _ := db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "phishing_suspect" && strings.Contains(it.Reason, "redirect") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected phishing_suspect review for the redirect, got %+v", items)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 0 {
		t.Fatalf("no assessment on redirect abort, got %+v", rows)
	}
}

// #6: oversized pages are refused.
func TestHandleSolveTestPageSizeLimit(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(make([]byte, 3<<20))
	}))
	defer big.Close()

	db := newDB(t)
	seedResume(t, db)
	s := solver(t, db, &fakeRunner{}, &fakeNotifier{}, nil)
	s.Cfg.Forms.AllowedDomains = append(s.Cfg.Forms.AllowedDomains, big.URL[len("http://"):])

	if err := s.HandleSolveTest(t.Context(), solveTask(big.URL)); err == nil {
		t.Fatal("oversized page must be refused")
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 0 {
		t.Fatalf("no assessment for oversized page, got %+v", rows)
	}
}
