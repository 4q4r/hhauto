// Package emaildup implements the duplicate-email follow-up feature: when an
// employer contact address is already known, a short reminder email is sent
// once per (employer, vacancy) with dedup by the store intent keys.
package emaildup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wneessen/go-mail"

	"hhauto/internal/config"
	"hhauto/internal/features"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/features/kit11"
	"hhauto/internal/proxy"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// FeatureName is the feature_state namespace.
const FeatureName = "emaildup"

// Behavior constants without a config knob.
const (
	smtpTimeout   = 30 * time.Second
	defaultSMTPTL = 587
	pdfTimeout    = 30 * time.Second
)

// SendPayload is the queue payload for queue.KindSendEmail.
type SendPayload struct {
	AccountID    string
	EmployerID   int64
	VacancyID    int64
	Email        string
	VacancyTitle string
	ResumeHash   string
}

// Message is the transport-agnostic email handed to a Sender.
type Message struct {
	From        string
	To          string
	Subject     string
	BodyText    string
	MessageID   string // deterministic, enables Message-ID based reconciliation
	Attachments []Attachment
}

// Attachment is one binary attachment.
type Attachment struct {
	Filename    string
	Content     []byte
	ContentType string
}

// Sender is the SMTP boundary. Tests inject fakes; production uses
// NewGoMailSender.
type Sender interface {
	Send(m Message) error
}

// Feature consumes queue.KindSendEmail tasks (WF8, disabled by default).
type Feature struct {
	Cfg    config.Config
	DB     *store.DB
	API    kit11.APIResolver // optional: used to discover the resume hash
	Queue  queue.Queue
	Notify webhook.Notifier
	Log    *slog.Logger
	Now    func() time.Time
	Sleep  func(time.Duration)
	Dialer func(accountID string) (proxy.Dialer, error)
	// Pacer spaces the SMTP send. nil = no-op (wave-3 review #4).
	Pacer featurekit.Pacer
	// Sender overrides SMTP transport (tests); lazily built from config
	// when nil — exactly once, race-safe (wave-3 review #8).
	Sender Sender

	senderOnce    sync.Once // guards builtSender/senderInitErr (review #8)
	builtSender   Sender
	senderInitErr error

	// PDF overrides resume-PDF retrieval (tests); default fetcher when nil.
	PDF func(ctx context.Context, accountID, resumeHash string) ([]byte, error)
}

// Name implements features.Feature.
func (f *Feature) Name() string { return "emaildup" }

// Enabled implements features.Feature.
func (f *Feature) Enabled() bool { return f.Cfg.Features.EmailDupEnabled }

// RegisterTasks implements features.Feature: emaildup is event-driven —
// contacts (WF6) enqueues KindSendEmail when an email is found and the
// auto-trigger is enabled. Nothing to seed on a schedule.
func (f *Feature) RegisterTasks(_ context.Context, q queue.Queue, _ *scheduler.Registry) error {
	f.Queue = q
	return nil
}

// Queue is retained by RegisterTasks (emaildup is an event-driven consumer).

// HandleSendEmail processes one KindSendEmail task through the WF8 gates.
func (f *Feature) HandleSendEmail(ctx context.Context, task queue.Task) error {
	p, ok := task.Payload.(SendPayload)
	if !ok {
		return fmt.Errorf("emaildup: bad payload type %T", task.Payload)
	}
	if !f.Cfg.Features.EmailDupEnabled {
		return nil
	}
	log := f.log()
	accountID := p.AccountID

	// Gate 1: recipient shape.
	email := strings.ToLower(strings.TrimSpace(p.Email))
	if !strings.Contains(email, "@") || !strings.Contains(strings.SplitN(email, "@", 2)[1], ".") {
		f.review(ctx, accountID, "email_invalid", "emaildup: recipient address is not valid",
			map[string]any{"employer_id": p.EmployerID, "email": p.Email})
		return nil
	}

	// Gate 2: per-employer once.
	if f.perEmployerOnce() {
		if sent, _, err := f.DB.FeatureStateGet(ctx, FeatureName, accountID, "sent:"+fmt.Sprint(p.EmployerID)); err == nil && sent != "" {
			log.Info("emaildup: employer already mailed, skipping", "employer", p.EmployerID)
			return nil
		}
	}

	// Gate 3: hourly and daily limits via metrics counters.
	if blocked, err := f.limitReached(ctx, accountID); err != nil {
		return err
	} else if blocked {
		_ = f.DB.MetricsIncr(ctx, "emaildup.skipped_limit", kit11.Day(f.now()))
		log.Warn("emaildup: rate limit reached, email skipped", "employer", p.EmployerID)
		return nil
	}

	// Attachment: fetch before arming the intent (no send attempt → no intent).
	var attachments []Attachment
	if f.attachResume() {
		pdf, hash, err := f.resumePDF(ctx, accountID, p.ResumeHash)
		if err != nil {
			if f.Cfg.Email.RequireAttach {
				f.review(ctx, accountID, "email_attachment_missing",
					"emaildup: resume PDF unavailable and attachment is required",
					map[string]any{"employer_id": p.EmployerID, "err": err.Error()})
				return nil
			}
			log.Warn("emaildup: resume PDF unavailable, sending without attachment", "err", err)
		} else {
			attachments = append(attachments, Attachment{
				Filename:    "resume-" + hash + ".pdf",
				Content:     pdf,
				ContentType: "application/pdf",
			})
		}
	}

	// Render subject/body.
	subject := renderTemplate(f.Cfg.Email.SubjectTemplate, defaultSubject, map[string]string{
		"vacancy_title": p.VacancyTitle,
		"vacancy_id":    fmt.Sprint(p.VacancyID),
		"resume_url":    ResumePDFURL(f.Cfg, p.ResumeHash),
	})
	body := renderTemplate(f.Cfg.Email.BodyTemplate, defaultBody, map[string]string{
		"vacancy_title": p.VacancyTitle,
		"vacancy_id":    fmt.Sprint(p.VacancyID),
		"resume_url":    ResumePDFURL(f.Cfg, p.ResumeHash),
	})

	// Idempotency intent BEFORE the side effect. Wave-3 review #10 (RULING
	// R8): the key includes the account id — multi-account correctness
	// outranks the letter of docs/PLAN.md §6.
	intentKey := fmt.Sprintf("email:%s:%d:%d", accountID, p.EmployerID, p.VacancyID)
	intentID, err := f.DB.IntentsBegin(ctx, intentKey, "pending", email)
	switch {
	case errors.Is(err, store.ErrIntentDone):
		// This employer+vacancy was already mailed (e.g. per_employer_once off).
		_ = f.DB.FeatureStateSet(ctx, FeatureName, accountID, "sent:"+fmt.Sprint(p.EmployerID), fmt.Sprint(p.VacancyID))
		return nil
	case errors.Is(err, store.ErrIntentPending):
		return nil // another attempt owns the send
	case err != nil:
		return fmt.Errorf("emaildup: intent: %w", err)
	}

	// Deterministic Message-ID: AMBIGUOUS outcomes reconcile by Message-ID.
	messageID := fmt.Sprintf("hhauto-%s@%s", intentID, domainOf(f.Cfg.Email.From))

	if f.Cfg.General.DryRun {
		// Dry-run leaves the intent re-armable (§6): done means "really
		// mailed" — marking it done here made the real run take the
		// ErrIntentDone repair path and record a sent-state without mail.
		_ = f.DB.IntentsMarkAbandoned(ctx, intentID)
		log.Info("emaildup: dry-run, send skipped", "to", email, "employer", p.EmployerID)
		return nil
	}

	m := Message{
		From:        f.Cfg.Email.From,
		To:          email,
		Subject:     subject,
		BodyText:    body,
		MessageID:   messageID,
		Attachments: attachments,
	}

	sender, err := f.senderFor()
	if err != nil {
		_ = f.DB.IntentsMarkFailed(ctx, intentID)
		return fmt.Errorf("emaildup: sender: %w", err)
	}

	retries := f.Cfg.Email.Retries
	if retries < 0 {
		retries = 0
	}
	var sendErr error
	for attempt := 0; attempt <= retries; attempt++ {
		// Pacer before EVERY send attempt (wave-3 review #4).
		if err := f.pace(ctx, accountID); err != nil {
			_ = f.DB.IntentsMarkFailed(ctx, intentID)
			return fmt.Errorf("emaildup: pacer: %w", err)
		}
		if attempt > 0 {
			f.sleep(time.Duration(attempt) * 5 * time.Second)
		}
		sendErr = sender.Send(m)
		if sendErr == nil {
			break
		}
		if smtpPermanent(sendErr) {
			break
		}
	}
	if sendErr == nil {
		if err := f.DB.IntentsMarkDone(ctx, intentID); err != nil {
			return err
		}
		_ = f.DB.FeatureStateSet(ctx, FeatureName, accountID, "sent:"+fmt.Sprint(p.EmployerID), fmt.Sprint(p.VacancyID))
		day := kit11.Day(f.now())
		_ = f.DB.MetricsIncr(ctx, "emaildup.sent", day)
		_ = f.DB.MetricsIncr(ctx, kit11.HourKey("emaildup.sent", f.now()), day)
		log.Info("emaildup: email sent", "to", email, "employer", p.EmployerID)
		return nil
	}

	_ = f.DB.IntentsMarkFailed(ctx, intentID)
	if smtpPermanent(sendErr) {
		// bounce → mark the contact invalid so WF6 does not re-trigger
		if err := f.markContactInvalid(ctx, accountID, p.EmployerID, email); err != nil {
			log.Error("emaildup: mark contact invalid", "err", err)
		}
		f.review(ctx, accountID, "email_send_failed", "emaildup: permanent rejection: "+sendErr.Error(),
			map[string]any{"employer_id": p.EmployerID, "email": email})
		return nil
	}
	// Transient failures exhausted: re-enqueue with the SAME dedup key — the
	// queue released it after dequeue, so without this the task would be
	// lost (wave-3 review #5). The intent is failed, so a re-run may begin
	// a fresh send attempt; the human still sees a review (once).
	f.requeue(ctx, task)
	f.reviewOnce(ctx, accountID, "email_send_failed",
		"emaildup: transient failures exhausted — task requeued",
		map[string]any{"employer_id": p.EmployerID, "email": email},
		"emailretry:"+intentKey)
	return nil
}

// requeue puts the task back with its original dedup key (no-op when the
// queue handle is not wired).
func (f *Feature) requeue(ctx context.Context, task queue.Task) {
	if f.Queue == nil {
		return
	}
	if err := f.Queue.Enqueue(ctx, task); err != nil {
		f.log().Warn("emaildup: requeue failed", "task", task.ID, "err", err)
	}
}

// reviewOnce records a review item only when no marker exists yet.
func (f *Feature) reviewOnce(ctx context.Context, accountID, kind, reason string, payload map[string]any, marker string) {
	if exists, _, err := f.DB.FeatureStateGet(ctx, FeatureName, accountID, "marker:"+marker); err == nil && exists != "" {
		return
	}
	_ = f.DB.FeatureStateSet(ctx, FeatureName, accountID, "marker:"+marker, "1")
	f.review(ctx, accountID, kind, reason, payload)
}

// pace waits on the injected pacer before a mutation (nil = no-op).
func (f *Feature) pace(ctx context.Context, accountID string) error {
	if f.Pacer == nil {
		return nil
	}
	if err := f.Pacer.Wait(ctx, accountID); err != nil {
		return fmt.Errorf("emaildup: pacer wait: %w", err)
	}
	return nil
}

// --- gates & helpers ---

func (f *Feature) perEmployerOnce() bool {
	return f.Cfg.Email.PerEmployerOnce == nil || *f.Cfg.Email.PerEmployerOnce
}

func (f *Feature) attachResume() bool {
	return f.Cfg.Email.AttachResume == nil || *f.Cfg.Email.AttachResume
}

// limitReached consults the metrics counters for the configured hour/day caps.
func (f *Feature) limitReached(ctx context.Context, _ string) (bool, error) {
	day := kit11.Day(f.now())
	snap, err := f.DB.MetricsSnapshot(ctx, day)
	if err != nil {
		return false, fmt.Errorf("emaildup: metrics: %w", err)
	}
	if f.Cfg.Email.PerHourLimit > 0 && snap[kit11.HourKey("emaildup.sent", f.now())] >= f.Cfg.Email.PerHourLimit {
		return true, nil
	}
	if f.Cfg.Email.PerDayLimit > 0 && snap["emaildup.sent"] >= f.Cfg.Email.PerDayLimit {
		return true, nil
	}
	return false, nil
}

// markContactInvalid annotates the contact rows carrying the address so the
// collection pipeline stops treating it as deliverable.
func (f *Feature) markContactInvalid(ctx context.Context, accountID string, employerID int64, email string) error {
	rows, err := f.DB.ContactsByEmployer(ctx, accountID, employerID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !strings.Contains(r.Emails, email) {
			continue
		}
		raw := map[string]any{}
		if r.Raw != "" {
			_ = json.Unmarshal([]byte(r.Raw), &raw)
		}
		raw["email_invalid"] = email
		b, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		row := r
		row.Raw = string(b)
		if err := f.DB.ContactsUpsert(ctx, &row); err != nil {
			return err
		}
	}
	return nil
}

// --- resume PDF ---

// ResumePDFURL builds the resume PDF URL from config (never hardcoded hosts):
// api.endpoints.override["resume_pdf"] may hold a template with {resume_id}.
func ResumePDFURL(cfg config.Config, resumeHash string) string {
	if tpl := strings.TrimSpace(cfg.API.Endpoints.Override["resume_pdf"]); tpl != "" {
		return strings.ReplaceAll(tpl, "{resume_id}", resumeHash)
	}
	site := strings.TrimRight(cfg.API.Endpoints.Site, "/")
	if site == "" {
		site = "https://hh.ru" // last-resort default mirrors config defaults
	}
	return site + "/resume/" + resumeHash + "?pdf=1"
}

// resumePDF retrieves the resume PDF through the default config-driven
// fetcher, or the injected PDF hook in tests.
func (f *Feature) resumePDF(ctx context.Context, accountID, resumeHash string) ([]byte, string, error) {
	if f.PDF != nil {
		b, err := f.PDF(ctx, accountID, resumeHash)
		return b, resumeHash, err
	}
	if resumeHash == "" {
		if f.API != nil {
			if api, aerr := f.API(accountID); aerr == nil {
				if resumes, lerr := api.ListResumes(ctx); lerr == nil && len(resumes) > 0 {
					resumeHash = resumes[0].HHID
				}
			}
		}
		if resumeHash == "" {
			return nil, "", errors.New("no resume hash available")
		}
	}
	url := ResumePDFURL(f.Cfg, resumeHash)

	var dialer proxy.Dialer
	if f.Dialer != nil {
		d, err := f.Dialer(accountID)
		if err != nil {
			return nil, "", fmt.Errorf("proxy dialer: %w", err)
		}
		dialer = d
	}
	tr := &http.Transport{Proxy: nil}
	if dialer != nil {
		tr.DialContext = dialer.DialContext
	}
	client := &http.Client{Transport: tr, Timeout: pdfTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("http %d", resp.StatusCode)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, "", err
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF")) {
		// login page / error page — never attach garbage
		return nil, "", errors.New("response is not a PDF")
	}
	return buf.Bytes(), resumeHash, nil
}

// --- templates ---

const (
	defaultSubject = "Кандидат по вакансии {vacancy_title}"
	defaultBody    = "Здравствуйте!\n\nОткликался на вакансию «{vacancy_title}» (id {vacancy_id}) на hh.ru. " +
		"Продублировал отклик на почту на случай, если сообщение в чате затерялось.\n\n" +
		"Резюме: {resume_url}\n\nС уважением."
)

func renderTemplate(tpl, fallback string, vars map[string]string) string {
	if strings.TrimSpace(tpl) == "" {
		tpl = fallback
	}
	out := tpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return "localhost"
}

// --- go-mail Sender implementation ---

// GoMailSender delivers via wneessen/go-mail. Security stance: transport
// encryption is mandatory — implicit TLS on port 465, mandatory STARTTLS on
// every other port (a plaintext-capable server is refused, never fallen back
// to). The SMTP password comes from the env var named by pass_env and is
// never logged or persisted.
type GoMailSender struct {
	cfg config.Email
}

// NewGoMailSender validates the SMTP configuration (fail fast at wiring).
func NewGoMailSender(cfg config.Email) (*GoMailSender, error) {
	if strings.TrimSpace(cfg.SMTPHost) == "" {
		return nil, errors.New("emaildup: email.smtp_host is required")
	}
	if strings.TrimSpace(cfg.From) == "" {
		return nil, errors.New("emaildup: email.from is required")
	}
	port := cfg.SMTPPort
	if port == 0 {
		port = defaultSMTPTL
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("emaildup: email.smtp_port %d out of range", cfg.SMTPPort)
	}
	if cfg.Username != "" && cfg.PassEnv == "" {
		return nil, errors.New("emaildup: email.pass_env is required when username is set (inline passwords are not allowed)")
	}
	return &GoMailSender{cfg: cfg}, nil
}

// Send composes and delivers one message.
func (s *GoMailSender) Send(m Message) error {
	msg, err := buildMessage(s.cfg, m.Subject, m.BodyText, m.MessageID, m.Attachments)
	if err != nil {
		return err
	}
	if err := msg.From(m.From); err != nil {
		return fmt.Errorf("emaildup: from address: %w", err)
	}
	if err := msg.To(m.To); err != nil {
		return fmt.Errorf("emaildup: to address: %w", err)
	}

	port := s.cfg.SMTPPort
	if port == 0 {
		port = defaultSMTPTL
	}
	opts := []mail.Option{
		mail.WithPort(port),
		mail.WithTimeout(smtpTimeout),
	}
	if port == 465 {
		// Implicit TLS from the first byte; no plaintext fallback.
		opts = append(opts, mail.WithSSLPort(false))
	} else {
		// STARTTLS mandatory: refuse to send when the server does not offer it.
		opts = append(opts, mail.WithTLSPortPolicy(mail.TLSMandatory))
	}
	password := os.Getenv(s.cfg.PassEnv)
	if s.cfg.Username != "" && password != "" {
		opts = append(opts,
			mail.WithSMTPAuth(mail.SMTPAuthPlain),
			mail.WithUsername(s.cfg.Username),
			mail.WithPassword(password))
	}
	client, err := mail.NewClient(s.cfg.SMTPHost, opts...)
	if err != nil {
		return fmt.Errorf("emaildup: smtp client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), smtpTimeout+5*time.Second)
	defer cancel()
	if err := client.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("emaildup: smtp send: %w", err)
	}
	return nil
}

// buildMessage assembles the go-mail message (exported logic kept internal
// for deterministic Message-ID and attachment tests).
func buildMessage(cfg config.Email, subject, body, messageID string, attachments []Attachment) (*mail.Msg, error) {
	msg := mail.NewMsg()
	msg.Subject(subject)
	msg.SetBodyString(mail.TypeTextPlain, body)
	if messageID != "" {
		msg.SetMessageIDWithValue(messageID)
	}
	_ = cfg
	for _, a := range attachments {
		opts := []mail.FileOption{}
		if a.ContentType != "" {
			opts = append(opts, mail.WithFileContentType(mail.ContentType(a.ContentType)))
		}
		if err := msg.AttachReader(a.Filename, bytes.NewReader(a.Content), opts...); err != nil {
			return nil, fmt.Errorf("emaildup: attach %s: %w", a.Filename, err)
		}
	}
	return msg, nil
}

// smtpPermanent classifies SMTP failures: 5xx-class rejections and auth
// failures will never succeed on retry; timeouts, 4xx and unknown errors are
// treated as transient.
func smtpPermanent(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	s := err.Error()
	for _, code := range []string{"501 ", "502 ", "503 ", "530 ", "535 ", "550 ", "551 ", "552 ", "553 ", "554 ", "5.1.0", "5.1.1", "5.7.1"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return false
}

// --- small helpers ---

// senderFor returns the injected Sender or builds the go-mail sender from
// config exactly once (sync.Once — wave-3 review #8). The Once-owned fields
// are written only inside Do, so late readers race with nothing; a
// construction error is sticky: every caller receives the same error.
func (f *Feature) senderFor() (Sender, error) {
	if f.Sender != nil {
		return f.Sender, nil
	}
	f.senderOnce.Do(func() {
		s, err := NewGoMailSender(f.Cfg.Email)
		if err != nil {
			f.senderInitErr = err
			return
		}
		f.builtSender = s
	})
	if f.senderInitErr != nil {
		return nil, f.senderInitErr
	}
	return f.builtSender, nil
}

func (f *Feature) sleep(d time.Duration) {
	if f.Sleep != nil {
		f.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (f *Feature) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

func (f *Feature) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Feature) review(ctx context.Context, accountID, kind, reason string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		b = []byte("{}")
	}
	if _, err := f.DB.ReviewItemsAdd(ctx, accountID, kind, reason, string(b)); err != nil {
		f.log().Error("emaildup: review add", "kind", kind, "err", err)
	}
	if f.Notify != nil {
		f.Notify.Emit(webhook.Event{
			Type:      webhook.EvNeedsHuman,
			AccountID: accountID,
			Time:      f.now(),
			Message:   reason,
			Payload:   payload,
		})
	}
}

// Compile-time conformance with the feature-module contract.
var _ features.Feature = (*Feature)(nil)
