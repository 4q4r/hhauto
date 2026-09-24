package emaildup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// --- fakes ---

type fakeSender struct {
	calls []Message
	err   func(call int) error
}

func (f *fakeSender) Send(m Message) error {
	f.calls = append(f.calls, m)
	if f.err != nil {
		if err := f.err(len(f.calls)); err != nil {
			return err
		}
	}
	return nil
}

type permanentErr struct{}

func (permanentErr) Error() string { return "550 5.1.1 recipient rejected" }

type transientErr struct{}

func (transientErr) Error() string { return "421 try again later" }

type fakeNotifier struct{ events []webhook.Event }

func (f *fakeNotifier) Emit(ev webhook.Event) { f.events = append(f.events, ev) }
func (f *fakeNotifier) Close(context.Context) error {
	return nil
}

// --- harness ---

func newDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type harness struct {
	db     *store.DB
	sender *fakeSender
	wh     *fakeNotifier
	feat   *Feature
	now    time.Time
	pdfOK  bool
	pdfErr error
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	db := newDB(t)
	if err := db.AccountsUpsert(t.Context(), store.Account{ID: "acc"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Features.EmailDupEnabled = true
	cfg.Email.Retries = 1
	if mutate != nil {
		mutate(&cfg)
	}
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	h := &harness{
		db:     db,
		sender: &fakeSender{},
		wh:     &fakeNotifier{},
		now:    now,
		pdfOK:  true,
	}
	h.feat = &Feature{
		Cfg:    cfg,
		DB:     db,
		Notify: h.wh,
		Sender: h.sender,
		Queue:  queue.New(),
		API:    func(string) (hhapi.HHAPI, error) { return nil, errors.New("no api in this test") },
		PDF: func(context.Context, string, string) ([]byte, error) {
			if h.pdfErr != nil {
				return nil, h.pdfErr
			}
			if h.pdfOK {
				return []byte("%PDF-1.7 fake"), nil
			}
			return nil, errors.New("no pdf")
		},
		Log:   slog.New(slog.DiscardHandler),
		Sleep: func(time.Duration) {},
	}
	h.feat.Now = func() time.Time { return h.now }
	return h
}

func sendTask(email string, employer, vacancy int64) queue.Task {
	return queue.Task{
		AccountID: "acc",
		Kind:      queue.KindSendEmail,
		DedupKey:  fmt.Sprintf("email:acc:%d:%d", employer, vacancy),
		Payload: SendPayload{
			AccountID:    "acc",
			EmployerID:   employer,
			VacancyID:    vacancy,
			Email:        email,
			VacancyTitle: "Go-разработчик",
			ResumeHash:   "res123",
		},
	}
}

func seedContact(t *testing.T, h *harness) {
	t.Helper()
	err := h.db.ContactsUpsert(t.Context(), &store.Contact{
		AccountID:  "acc",
		EmployerID: 777,
		Source:     "card",
		Emails:     `["hr@romashka.ru"]`,
		Raw:        `{"source":"card"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- tests ---

func TestHandleSendEmailHappyPath(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		c.Email.SubjectTemplate = "Кандидат по вакансии {vacancy_title}"
		c.Email.BodyTemplate = "Здравствуйте! Откликнулся на {vacancy_title} (id {vacancy_id}). Резюме: {resume_url}"
	})
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("HR@Romashka.ru", 777, 42)); err != nil {
		t.Fatalf("HandleSendEmail: %v", err)
	}
	if len(h.sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(h.sender.calls))
	}
	m := h.sender.calls[0]
	if m.To != "hr@romashka.ru" {
		t.Fatalf("recipient not normalized: %q", m.To)
	}
	if m.From != "me@example.com" {
		t.Fatalf("from = %q", m.From)
	}
	if !strings.Contains(m.Subject, "Go-разработчик") {
		t.Fatalf("subject placeholder not rendered: %q", m.Subject)
	}
	if !strings.Contains(m.BodyText, "Go-разработчик") || !strings.Contains(m.BodyText, "42") || !strings.Contains(m.BodyText, "res123") {
		t.Fatalf("body placeholders not rendered: %q", m.BodyText)
	}
	if len(m.Attachments) != 1 || !strings.HasPrefix(string(m.Attachments[0].Content), "%PDF") {
		t.Fatalf("PDF attachment missing: %+v", m.Attachments)
	}
	// metrics: hour + day buckets
	snap, err := h.db.MetricsSnapshot(t.Context(), h.now.UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	if snap["emaildup.sent"] != 1 || snap["emaildup.sent.h15"] != 1 {
		t.Fatalf("metrics not recorded: %+v", snap)
	}
	// employer once-marker set
	if _, ok, _ := h.db.FeatureStateGet(t.Context(), FeatureName, "acc", "sent:777"); !ok {
		t.Fatal("per-employer marker not set")
	}
	// intent done
	if _, err := h.db.IntentsBegin(t.Context(), "email:acc:777:42", "pending", "x"); !errors.Is(err, store.ErrIntentDone) {
		t.Fatalf("intent must be done, got %v", err)
	}
}

func TestPerEmployerOnceGate(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 43)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 1 {
		t.Fatalf("per_employer_once must block the second employer email, sends=%d", len(h.sender.calls))
	}

	// explicit opt-out allows a second vacancy
	h2 := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		tr := false
		c.Email.PerEmployerOnce = &tr
	})
	seedContact(t, h2)
	if err := h2.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if err := h2.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 43)); err != nil {
		t.Fatal(err)
	}
	if len(h2.sender.calls) != 2 {
		t.Fatalf("opt-out must allow the second vacancy, sends=%d", len(h2.sender.calls))
	}
}

func TestRateLimits(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		c.Email.PerHourLimit = 1
		c.Email.PerDayLimit = 1
	})
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	// second employer, different email — still blocked by the limit
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("other@x.ru", 778, 43)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 1 {
		t.Fatalf("rate limit must block the second send, sends=%d", len(h.sender.calls))
	}
	items, _ := h.db.ReviewItemsOpen(t.Context(), "acc", 10)
	_ = items // limit skips are logged+counted, not reviewed
}

func TestRequireAttachAbort(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		c.Email.RequireAttach = true
	})
	h.pdfErr = errors.New("resume pdf unavailable")
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 0 {
		t.Fatal("require_attachment must abort without sending")
	}
	items, _ := h.db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "email_attachment_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected email_attachment_missing review, got %+v", items)
	}
}

func TestNoAttachFallbackWhenNotRequired(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	h.pdfErr = errors.New("resume pdf unavailable")
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 1 || len(h.sender.calls[0].Attachments) != 0 {
		t.Fatalf("send without attachment expected, got %+v", h.sender.calls)
	}
}

func TestAttachResumeFalseSkipsFetch(t *testing.T) {
	called := false
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	tf := false
	h.feat.Cfg.Email.AttachResume = &tf
	h.feat.PDF = func(context.Context, string, string) ([]byte, error) { called = true; return nil, nil }
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("attach_resume=false must not fetch the PDF")
	}
	if len(h.sender.calls) != 1 || len(h.sender.calls[0].Attachments) != 0 {
		t.Fatalf("plain send expected, got %+v", h.sender.calls)
	}
}

func TestInvalidEmailReviewed(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("не-почта", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 0 {
		t.Fatal("invalid email must not be sent")
	}
	items, _ := h.db.ReviewItemsOpen(t.Context(), "acc", 10)
	if len(items) == 0 {
		t.Fatal("invalid email must produce a review item")
	}
}

func TestPermanentBounceMarksContactInvalid(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	h.sender.err = func(int) error { return permanentErr{} }
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 1 {
		t.Fatalf("permanent error must not be retried, calls=%d", len(h.sender.calls))
	}
	contacts, _ := h.db.ContactsByEmployer(t.Context(), "acc", 777)
	marked := false
	for _, c := range contacts {
		if strings.Contains(c.Raw, "email_invalid") && strings.Contains(c.Raw, "hr@romashka.ru") {
			marked = true
		}
	}
	if !marked {
		t.Fatalf("contact raw not marked invalid: %+v", contacts)
	}
}

func TestTransientRetriesThenReview(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		c.Email.Retries = 2
	})
	h.sender.err = func(int) error { return transientErr{} }
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 3 { // 1 + 2 retries
		t.Fatalf("expected 3 attempts, got %d", len(h.sender.calls))
	}
	items, _ := h.db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "email_send_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected email_send_failed review, got %+v", items)
	}
	// intent failed → a later manual re-trigger is possible
	if _, err := h.db.IntentsBegin(t.Context(), "email:acc:777:42", "pending", "x"); err != nil {
		t.Fatalf("failed intent must allow re-begin, got %v", err)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		c.General.DryRun = true
	})
	seedContact(t, h)
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 0 {
		t.Fatal("dry-run must not send")
	}
	if _, err := h.db.IntentsBegin(t.Context(), "email:acc:777:42", "pending", "x"); err != nil {
		t.Fatalf("dry-run must leave the intent re-armable (§6: done = really mailed), got %v", err)
	}
}

func TestDisabledFeatureNoop(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Features.EmailDupEnabled = false })
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 0 {
		t.Fatal("disabled emaildup must not send")
	}
}

// Б2 regression: a dry-run pass must not consume the email intent — after
// the switch to a real run the letter is actually delivered.
func TestDryRunThenRealRunSendsEmail(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		c.General.DryRun = true
	})
	seedContact(t, h)
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 0 {
		t.Fatalf("dry-run must not send: %+v", h.sender.calls)
	}

	h.feat.Cfg.General.DryRun = false
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h.sender.calls) != 1 {
		t.Fatalf("real run after dry-run must send the email, calls = %d", len(h.sender.calls))
	}
}

// --- default PDF fetch + URL building (httptest, no external network) ---

func TestResumePDFURL(t *testing.T) {
	var cfg config.Config
	cfg.API.Endpoints.Site = "https://hh.ru"
	if got := ResumePDFURL(cfg, "abc123"); got != "https://hh.ru/resume/abc123?pdf=1" {
		t.Fatalf("default pdf url = %q", got)
	}
	cfg.API.Endpoints.Override = map[string]string{"resume_pdf": "https://custom.example/r/{resume_id}/print"}
	if got := ResumePDFURL(cfg, "abc123"); got != "https://custom.example/r/abc123/print" {
		t.Fatalf("override pdf url = %q", got)
	}
}

func TestDefaultPDFFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/resume/res123" || r.URL.Query().Get("pdf") != "1" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "%PDF-1.7 test")
	}))
	defer srv.Close()

	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	h.feat.PDF = nil // use the default fetcher
	h.feat.Cfg.API.Endpoints.Site = srv.URL

	err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42))
	if err != nil {
		t.Fatalf("default fetcher flow: %v", err)
	}
	if len(h.sender.calls) != 1 || len(h.sender.calls[0].Attachments) != 1 {
		t.Fatalf("expected sent mail with PDF, got %+v", h.sender.calls)
	}

	// non-PDF body (login page) → treated as unavailable: the mail goes out
	// WITHOUT any attachment (require_attach is false in this harness), and
	// garbage is never attached.
	h2 := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>log in please</html>")
	}))
	defer srv2.Close()
	h2.feat.PDF = nil
	h2.feat.Cfg.API.Endpoints.Site = srv2.URL
	if err := h2.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if len(h2.sender.calls) != 1 {
		t.Fatalf("expected a single send, got %+v", h2.sender.calls)
	}
	if len(h2.sender.calls[0].Attachments) != 0 {
		t.Fatalf("login page must not be attached, got %+v", h2.sender.calls[0].Attachments)
	}
}

func TestGoMailSenderValidation(t *testing.T) {
	if _, err := NewGoMailSender(config.Email{}); err == nil {
		t.Fatal("missing host must be rejected")
	}
	// auth requested (username set) without pass_env → rejected
	if _, err := NewGoMailSender(config.Email{SMTPHost: "smtp.example.com", From: "me@example.com", Username: "u"}); err == nil {
		t.Fatal("username without pass_env must be rejected")
	}
	cfg := config.Email{SMTPHost: "smtp.example.com", From: "me@example.com"}
	if _, err := NewGoMailSender(cfg); err != nil {
		t.Fatalf("no-auth relay config rejected: %v", err)
	}
}

func TestMessageIDAndBuildMessage(t *testing.T) {
	m, err := buildMessage(config.Email{From: "me@example.com"}, "Кандидат", "Привет",
		"id-123@from.test", []Attachment{{Filename: "resume.pdf", Content: []byte("%PDF")}})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	var buf strings.Builder
	if _, err := m.WriteTo(&buf); err != nil {
		t.Fatalf("render message: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Subject:", // subject header present (value is MIME-encoded)
		"Message-ID: <id-123@from.test>",
		"resume.pdf",      // attachment filename
		"application/pdf", // attachment content type header present
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered message missing %q:\n%s", want, out)
		}
	}
}

// --- Wave-3 review fixes ---

type countingPacer struct{ calls int }

func (p *countingPacer) Wait(context.Context, string) error { p.calls++; return nil }

// #4: pacer waits before every send attempt (1 initial + Retries).
func TestSendEmailPacerWaitsPerAttempt(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Email.From = "me@example.com"
		c.Email.Retries = 2
	})
	p := &countingPacer{}
	h.feat.Pacer = p
	h.sender.err = func(int) error { return transientErr{} }
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if p.calls != 3 {
		t.Fatalf("pacer calls = %d, want 3 (one per attempt)", p.calls)
	}
}

// #5: transient failures re-enqueue the same dedup key (the queue released
// it after dequeue) so the task is not lost.
func TestTransientRequeuesTask(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	h.sender.err = func(int) error { return transientErr{} }
	seedContact(t, h)

	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if n := h.feat.Queue.Len("acc"); n != 1 {
		t.Fatalf("transient failure must requeue the task, len=%d", n)
	}
	if got, ok := h.feat.Queue.Dequeue(t.Context(), "acc"); ok && got.DedupKey != "email:acc:777:42" {
		t.Fatalf("requeued dedup key = %q", got.DedupKey)
	}
}

// #8: lazy sender initialization is race-safe (sync.Once); under -race two
// goroutines must receive the same error and the same instance.
func TestSenderForConcurrent(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	h.feat.Sender = nil // force the lazy path
	h.feat.Cfg.Email.SMTPHost = "smtp.example.com"
	h.feat.Cfg.Email.PassEnv = "SOME_PASS_ENV"
	h.feat.Cfg.Email.Username = "user"

	const n = 8
	errs := make([]error, n)
	send := make([]Sender, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := h.feat.senderFor()
			errs[i], send[i] = err, s
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || send[i] == nil || send[i] != send[0] {
			t.Fatalf("goroutine %d: err=%v same=%v", i, errs[i], send[i] == send[0])
		}
	}
}

// #10: the intent key includes the account id (multi-account correctness).
func TestIntentKeyIncludesAccount(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Email.From = "me@example.com" })
	seedContact(t, h)
	if err := h.feat.HandleSendEmail(t.Context(), sendTask("hr@romashka.ru", 777, 42)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.IntentsBegin(t.Context(), "email:acc:777:42", "pending", "x"); !errors.Is(err, store.ErrIntentDone) {
		t.Fatalf("intent key must be email:<acc>:<emp>:<vac>, got %v", err)
	}
}
