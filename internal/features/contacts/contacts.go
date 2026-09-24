// Package contacts implements the employer-contact collection feature
// (WF6): employer cards and chat profiles are normalized, merged and stored
// as deduplicated contact rows feeding emaildup.
package contacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features"
	"hhauto/internal/features/emaildup"
	"hhauto/internal/features/kit11"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// FeatureName is the feature_state namespace.
const FeatureName = "contacts"

// CollectPayload is the queue payload for queue.KindCollectContacts.
// multiapply (T10) enqueues it after each APPLY; chatops enqueues it when a
// chat reveals the employer behind a vacancy.
type CollectPayload struct {
	AccountID    string
	EmployerID   int64
	VacancyID    int64
	VacancyTitle string
	ResumeHash   string // optional, forwarded to emaildup
}

// Collected summarizes one collection pass.
type Collected struct {
	Emails     []string
	Phones     []string
	Messengers []string
}

// Feature consumes KindCollectContacts tasks and triggers emaildup.
type Feature struct {
	Cfg    config.Config
	DB     *store.DB
	AI     ai.Runner
	API    kit11.APIResolver
	Queue  queue.Queue
	Notify webhook.Notifier
	Log    *slog.Logger
	Now    func() time.Time
}

// Name implements features.Feature.
func (f *Feature) Name() string { return "contacts" }

// Enabled implements features.Feature.
func (f *Feature) Enabled() bool { return f.Cfg.Features.Contacts.Enabled }

// RegisterTasks implements features.Feature. Collection is event-driven:
// multiapply enqueues KindCollectContacts after every APPLY and chatops
// enqueues it when a chat reveals an employer — there is nothing to seed on
// a schedule, so this only retains the queue handle.
func (f *Feature) RegisterTasks(_ context.Context, q queue.Queue, _ *scheduler.Registry) error {
	f.Queue = q
	return nil
}

// HandleCollectContacts processes one KindCollectContacts task.
func (f *Feature) HandleCollectContacts(ctx context.Context, task queue.Task) error {
	p, ok := task.Payload.(CollectPayload)
	if !ok {
		return fmt.Errorf("contacts: bad payload type %T", task.Payload)
	}
	api, err := f.API(p.AccountID)
	if err != nil {
		return fmt.Errorf("contacts: resolve api: %w", err)
	}
	collected, err := CollectForVacancy(ctx, f.DB, api, f.AI, p.AccountID, p.EmployerID, p.VacancyID)
	if err != nil {
		if kit11.IsTransient(err) {
			// Wave-3 review #5: the queue released the dedup key after
			// dequeue — re-enqueue the same task so it is not lost, and
			// clear the chatops marker so a future sync may re-enqueue too.
			f.clearCollectMarker(ctx, p.AccountID, p.EmployerID)
			f.requeue(ctx, task)
			return err
		}
		return fmt.Errorf("contacts: collect %d: %w", p.EmployerID, err)
	}

	if f.Notify != nil && (len(collected.Emails)+len(collected.Phones)+len(collected.Messengers)) > 0 {
		f.Notify.Emit(webhook.Event{
			Type:      webhook.EvContactCollected,
			AccountID: p.AccountID,
			Time:      f.now(),
			Message:   "Контакты работодателя собраны",
			Payload: map[string]any{
				"employer_id": p.EmployerID,
				"emails":      len(collected.Emails),
				"phones":      len(collected.Phones),
				"messengers":  len(collected.Messengers),
			},
		})
	}

	if len(collected.Emails) > 0 && f.Cfg.Features.Contacts.AutoEmailDup && f.Cfg.Features.EmailDupEnabled && f.Queue != nil {
		if err := f.Queue.Enqueue(ctx, queue.Task{
			AccountID: p.AccountID,
			Kind:      queue.KindSendEmail,
			Priority:  queue.PrioEmail,
			DedupKey:  fmt.Sprintf("email:%s:%d:%d", p.AccountID, p.EmployerID, p.VacancyID),
			Payload: SendPayloadAlias{
				AccountID:    p.AccountID,
				EmployerID:   p.EmployerID,
				VacancyID:    p.VacancyID,
				Email:        collected.Emails[0],
				VacancyTitle: p.VacancyTitle,
				ResumeHash:   p.ResumeHash,
			},
		}); err != nil {
			return fmt.Errorf("contacts: enqueue send_email: %w", err)
		}
	}
	return nil
}

// SendPayloadAlias is the queue payload type for emaildup (single definition
// lives in emaildup; this alias documents the hand-off).
type SendPayloadAlias = emaildup.SendPayload

// CollectForVacancy collects employer contacts from the employer card (and is
// the mandated entry point for T10/T12): api.EmployerContacts is normalized
// (E.164 phones, lowercase emails), merged with any previously stored row and
// deduplicated by the store UNIQUE(account, employer, source).
func CollectForVacancy(ctx context.Context, db *store.DB, api hhapi.HHAPI, _ ai.Runner,
	accountID string, employerID, vacancyID int64) (Collected, error) {
	if db == nil {
		return Collected{}, errors.New("contacts: db is nil")
	}
	if api == nil {
		return Collected{}, errors.New("contacts: api is nil")
	}
	card, err := api.EmployerContacts(ctx, employerID)
	if err != nil {
		return Collected{}, err
	}
	norm := normalizeAll(card)
	res := Collected{Emails: norm.emails, Phones: norm.phones, Messengers: norm.messengers}
	if len(norm.emails)+len(norm.phones)+len(norm.messengers) == 0 {
		return res, nil
	}
	if err := mergeUpsert(ctx, db, accountID, employerID, "card", norm, map[string]any{
		"employer_id": employerID, "vacancy_id": vacancyID, "source": "card",
	}); err != nil {
		return Collected{}, err
	}
	return res, nil
}

// CollectFromMessage parses contact signatures out of one inbound chat
// message: regex first (cheap, precise for well-formed values), AI fallback
// for unstructured hints. This is invoked by chatops per new inbound message
// when features.contacts.from_messages is enabled.
func CollectFromMessage(ctx context.Context, db *store.DB, runner ai.Runner,
	accountID string, employerID int64, employerName, text string) (Collected, error) {
	if db == nil {
		return Collected{}, errors.New("contacts: db is nil")
	}
	norm := extractWithRegex(text)
	if len(norm.emails)+len(norm.phones)+len(norm.messengers) == 0 {
		if !contactHint(text) || runner == nil {
			return Collected{}, nil
		}
		aiNorm, err := extractWithAI(ctx, runner, accountID, text)
		if err != nil {
			return Collected{}, err
		}
		norm = aiNorm
	}
	if len(norm.emails)+len(norm.phones)+len(norm.messengers) == 0 {
		return Collected{}, nil
	}
	if err := mergeUpsert(ctx, db, accountID, employerID, "chat", norm, map[string]any{
		"employer_id": employerID, "employer_name": employerName, "source": "chat",
	}); err != nil {
		return Collected{}, err
	}
	return Collected{Emails: norm.emails, Phones: norm.phones, Messengers: norm.messengers}, nil
}

// --- normalization ---

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	phoneRe = regexp.MustCompile(`\+?\d[\d\s\-()]{7,}\d`)
	tgRe    = regexp.MustCompile(`(?i)(?:t\.me/|@)([A-Za-z0-9_]{4,32})`)
)

type normalized struct {
	emails     []string
	phones     []string
	messengers []string
	quotes     []string
}

func normalizeAll(in []hhapi.Contact) normalized {
	var n normalized
	for _, c := range in {
		v := strings.TrimSpace(c.Value)
		if v == "" {
			continue
		}
		switch strings.ToLower(c.Type) {
		case "email":
			if e := NormalizeEmail(v); e != "" {
				n.emails = appendUnique(n.emails, e)
			}
		case "phone":
			if p := NormalizePhone(v); p != "" {
				n.phones = appendUnique(n.phones, p)
			}
		case "telegram":
			if t := NormalizeTG(v); t != "" {
				n.messengers = appendUnique(n.messengers, t)
			}
		default:
			// unknown type: sniff by value shape
			switch {
			case strings.Contains(v, "@"):
				if e := NormalizeEmail(v); e != "" {
					n.emails = appendUnique(n.emails, e)
				}
			case strings.HasPrefix(v, "+") || strings.HasPrefix(v, "8"):
				if p := NormalizePhone(v); p != "" {
					n.phones = appendUnique(n.phones, p)
				}
			}
		}
	}
	return n
}

// NormalizeEmail lowercases and shape-validates an address; "" when invalid.
func NormalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !emailRe.MatchString(s) || len(s) > 254 {
		return ""
	}
	return s
}

// NormalizePhone reduces a value to E.164 with a Russian numbering bias
// (hh.ru is a Russian platform): 8XXXXXXXXXX / 7XXXXXXXXXX → +7XXXXXXXXXX,
// 10-digit local numbers get +7, anything else is kept as "+" + digits.
func NormalizePhone(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	switch {
	case len(d) == 11 && strings.HasPrefix(d, "8"):
		d = "7" + d[1:]
	case len(d) == 10:
		d = "7" + d
	}
	if len(d) < 8 || len(d) > 15 {
		return ""
	}
	return "+" + d
}

// NormalizeTG canonicalizes a telegram handle to @handle (lowercase).
func NormalizeTG(s string) string {
	s = strings.TrimSpace(s)
	m := tgRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return "@" + strings.ToLower(m[1])
}

// extractWithRegex pulls well-formed contacts from free text.
func extractWithRegex(text string) normalized {
	var n normalized
	for _, m := range emailRe.FindAllString(text, -1) {
		if e := NormalizeEmail(m); e != "" {
			n.emails = appendUnique(n.emails, e)
		}
	}
	for _, m := range phoneRe.FindAllString(text, -1) {
		if p := NormalizePhone(m); p != "" {
			n.phones = appendUnique(n.phones, p)
		}
	}
	for _, m := range tgRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 {
			n.messengers = appendUnique(n.messengers, "@"+strings.ToLower(m[1]))
		}
	}
	return n
}

// contactHint reports whether the text plausibly contains a contact worth an
// AI extraction pass (regex found nothing, but the wording suggests one).
func contactHint(text string) bool {
	t := strings.ToLower(text)
	for _, kw := range []string{"телеграм", "telegram", "тг", "телефон", "позвон", "звонит", "связ", "контакт", "почт", "напиш"} {
		if strings.Contains(t, kw) {
			return true
		}
	}
	return false
}

// extractWithAI runs contact_extraction for unstructured text.
func extractWithAI(ctx context.Context, runner ai.Runner, accountID, text string) (normalized, error) {
	var n normalized
	input := map[string]any{"text": text, "source": "chat_message"}
	raw, _, err := runner.Run(ctx, accountID, "message", "contact", "", ai.TaskContactExtract, input)
	if err != nil {
		return n, fmt.Errorf("contacts: ai extraction: %w", err)
	}
	var verdict struct {
		ai.Common
		Contacts []struct {
			Type        string `json:"type"`
			Value       string `json:"value"`
			Normalized  string `json:"normalized"`
			SourceQuote string `json:"source_quote"`
		} `json:"contacts"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return n, fmt.Errorf("contacts: ai verdict decode: %w", err)
	}
	for _, c := range verdict.Contacts {
		v := c.Normalized
		if v == "" {
			v = c.Value
		}
		switch strings.ToLower(c.Type) {
		case "email":
			if e := NormalizeEmail(v); e != "" {
				n.emails = appendUnique(n.emails, e)
			}
		case "phone":
			if p := NormalizePhone(v); p != "" {
				n.phones = appendUnique(n.phones, p)
			}
		case "messenger":
			if t := NormalizeTG(v); t != "" {
				n.messengers = appendUnique(n.messengers, t)
			}
		}
		if c.SourceQuote != "" {
			n.quotes = append(n.quotes, c.SourceQuote)
		}
	}
	return n, nil
}

// --- persistence (merge + dedup) ---

// mergeUpsert merges the newly found values into the per-source contact row
// (UNIQUE account+employer+source): previously collected values are never
// lost, duplicates collapse.
func mergeUpsert(ctx context.Context, db *store.DB, accountID string, employerID int64,
	source string, n normalized, raw map[string]any) error {
	existing, err := db.ContactsByEmployer(ctx, accountID, employerID)
	if err != nil {
		return fmt.Errorf("contacts: list existing: %w", err)
	}
	var prev *store.Contact
	for i := range existing {
		if existing[i].Source == source {
			prev = &existing[i]
			break
		}
	}
	var oldEmails, oldPhones, oldMsgrs []string
	var oldRaw map[string]any
	if prev != nil {
		// Values live in the dedicated columns; parse them (JSON lists).
		_ = json.Unmarshal([]byte(prev.Emails), &oldEmails)
		_ = json.Unmarshal([]byte(prev.Phones), &oldPhones)
		_ = json.Unmarshal([]byte(prev.Messengers), &oldMsgrs)
		if prev.Raw != "" {
			_ = json.Unmarshal([]byte(prev.Raw), &oldRaw)
		}
	}
	emails := mergeUnique(oldEmails, n.emails)
	phones := mergeUnique(oldPhones, n.phones)
	msgrs := mergeUnique(oldMsgrs, n.messengers)

	raw["emails_found"] = emails
	raw["phones_found"] = phones
	raw["messengers_found"] = msgrs
	if len(n.quotes) > 0 {
		raw["source_quotes"] = n.quotes
	}
	if oldRaw != nil {
		if q, ok := oldRaw["source_quotes"]; ok {
			raw["source_quotes"] = mergeUnique(toStrings(q), n.quotes)
		}
	}
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		rawJSON = []byte("{}")
	}

	row := store.Contact{
		AccountID:   accountID,
		EmployerID:  employerID,
		Source:      source,
		Phones:      listJSON(phones),
		Emails:      listJSON(emails),
		Messengers:  listJSON(msgrs),
		Raw:         string(rawJSON),
		CollectedAt: time.Now(),
	}
	if err := db.ContactsUpsert(ctx, &row); err != nil {
		return fmt.Errorf("contacts: upsert: %w", err)
	}
	return nil
}

func listJSON(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func mergeUnique(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, v := range b {
		out = appendUnique(out, v)
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	return append(list, v)
}

func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// requeue puts the task back with its original dedup key (no-op when the
// queue handle is not wired).
func (f *Feature) requeue(ctx context.Context, task queue.Task) {
	if f.Queue == nil {
		return
	}
	if err := f.Queue.Enqueue(ctx, task); err != nil {
		f.log().Warn("contacts: requeue failed", "task", task.ID, "err", err)
	}
}

// clearCollectMarker resets the chatops "already enqueued" marker for the
// employer (chatops reads it before enqueueing a collect task).
func (f *Feature) clearCollectMarker(ctx context.Context, accountID string, employerID int64) {
	if err := f.DB.FeatureStateSet(ctx, "chatops", accountID, kit11.CollectQueuedMarker(employerID), ""); err != nil {
		f.log().Warn("contacts: clear collect marker failed", "employer", employerID, "err", err)
	}
}

// log helper
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

// Compile-time conformance with the feature-module contract.
var _ features.Feature = (*Feature)(nil)
