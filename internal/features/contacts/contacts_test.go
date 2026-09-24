package contacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features/kit11"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/store"
)

// --- fakes ---

// hhapiStub supplies every method of the HHAPI contract as a panic stub so
// fakes only override what the feature actually calls.
type hhapiStub struct {
	hhapi.HHAPI
}

type contactsAPI struct {
	hhapiStub
	contacts []hhapi.Contact
	err      error
}

func (f *contactsAPI) EmployerContacts(_ context.Context, _ int64) ([]hhapi.Contact, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.contacts, nil
}

type aiRunner struct {
	verdict []map[string]any
	calls   int
}

func (f *aiRunner) Run(_ context.Context, _ string, _, _ string, _ string, task ai.TaskName, _ any) (json.RawMessage, ai.Decision, error) {
	f.calls++
	if task != ai.TaskContactExtract {
		return nil, ai.Decision{}, fmt.Errorf("unexpected task %s", task)
	}
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.85, "needs_human": false, "needs_human_reason": nil,
		"injection_suspected": false, "contacts": f.verdict,
	})
	return b, ai.Decision{Task: task}, nil
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

// --- normalization ---

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"+7 900 123-45-67": "+79001234567",
		"8 (900) 123-4567": "+79001234567",
		"9001234567":       "+79001234567",
		"79001234567":      "+79001234567",
		"+44 20 7946 0958": "+442079460958",
		"123":              "",
		"":                 "",
	}
	for in, want := range cases {
		if got := NormalizePhone(in); got != want {
			t.Fatalf("NormalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeEmailAndTG(t *testing.T) {
	if NormalizeEmail("  HR@Romashka.RU ") != "hr@romashka.ru" {
		t.Fatal("email normalization broken")
	}
	if NormalizeEmail("not-an-email") != "" {
		t.Fatal("invalid email must be rejected")
	}
	if NormalizeTG("https://t.me/SomeOne") != "@someone" {
		t.Fatal("tg normalization broken")
	}
	if NormalizeTG("@User_Name") != "@user_name" {
		t.Fatal("tg @ normalization broken")
	}
}

// --- card collection ---

func TestCollectForVacancyNormalizesAndStores(t *testing.T) {
	db := newDB(t)
	api := &contactsAPI{contacts: []hhapi.Contact{
		{Type: "email", Value: "  HR@Romashka.RU "},
		{Type: "phone", Value: "8 (900) 123-45-67"},
		{Type: "telegram", Value: "t.me/romashka_hr"},
		{Type: "weird", Value: "нечитаемое"},
	}}
	got, err := CollectForVacancy(t.Context(), db, api, nil, "acc", 777, 42)
	if err != nil {
		t.Fatalf("CollectForVacancy: %v", err)
	}
	if len(got.Emails) != 1 || got.Emails[0] != "hr@romashka.ru" {
		t.Fatalf("emails = %v", got.Emails)
	}
	if len(got.Phones) != 1 || got.Phones[0] != "+79001234567" {
		t.Fatalf("phones = %v", got.Phones)
	}
	if len(got.Messengers) != 1 || got.Messengers[0] != "@romashka_hr" {
		t.Fatalf("messengers = %v", got.Messengers)
	}
	rows, err := db.ContactsByEmployer(t.Context(), "acc", 777)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	if rows[0].Source != "card" || !strings.Contains(rows[0].Emails, "hr@romashka.ru") {
		t.Fatalf("row = %+v", rows[0])
	}
	// no vacancy filter on the row level: store keys on (account, employer, source)
	_ = 42
}

func TestCollectForVacancyMergePreservesOldValues(t *testing.T) {
	db := newDB(t)
	if err := db.ContactsUpsert(t.Context(), &store.Contact{
		AccountID: "acc", EmployerID: 777, Source: "card",
		Emails: `["old@romashka.ru"]`, Raw: `{"old":true}`,
	}); err != nil {
		t.Fatal(err)
	}
	api := &contactsAPI{contacts: []hhapi.Contact{{Type: "email", Value: "new@romashka.ru"}}}
	if _, err := CollectForVacancy(t.Context(), db, api, nil, "acc", 777, 42); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.ContactsByEmployer(t.Context(), "acc", 777)
	if len(rows) != 1 {
		t.Fatalf("merge must update the single source row, got %d", len(rows))
	}
	if !strings.Contains(rows[0].Emails, "old@romashka.ru") || !strings.Contains(rows[0].Emails, "new@romashka.ru") {
		t.Fatalf("merged emails = %q", rows[0].Emails)
	}
}

func TestCollectForVacancyAPIErrorSurfaces(t *testing.T) {
	db := newDB(t)
	api := &contactsAPI{err: hhapi.ErrRateLimited}
	_, err := CollectForVacancy(t.Context(), db, api, nil, "acc", 777, 42)
	if !errors.Is(err, hhapi.ErrRateLimited) {
		t.Fatalf("transient API errors must surface, got %v", err)
	}
}

func TestCollectForVacancyNilDeps(t *testing.T) {
	if _, err := CollectForVacancy(t.Context(), nil, nil, nil, "acc", 1, 2); err == nil {
		t.Fatal("nil deps must error")
	}
}

// --- chat message extraction ---

func TestCollectFromMessageRegexPath(t *testing.T) {
	db := newDB(t)
	run := &aiRunner{}
	got, err := CollectFromMessage(t.Context(), db, run, "acc", 777, "ООО Ромашка",
		"Пишите на hr@romashka.ru или звоните +7 900 123-45-67")
	if err != nil {
		t.Fatal(err)
	}
	if run.calls != 0 {
		t.Fatal("regex-hit messages must not call the AI")
	}
	if len(got.Emails) != 1 || len(got.Phones) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestCollectFromMessageAIFallback(t *testing.T) {
	db := newDB(t)
	run := &aiRunner{verdict: []map[string]any{
		{"type": "email", "value": "иван@ромашка.рф", "normalized": "ivan@romashka.example", "source_quote": "почта ивана"},
	}}
	got, err := CollectFromMessage(t.Context(), db, run, "acc", 777, "ООО Ромашка",
		"Если что, моя почта для связи указана в профиле, продублирую письмом")
	if err != nil {
		t.Fatal(err)
	}
	if run.calls != 1 {
		t.Fatalf("hint without regex hits must call the AI, calls=%d", run.calls)
	}
	if len(got.Emails) != 1 || got.Emails[0] != "ivan@romashka.example" {
		t.Fatalf("got %+v", got)
	}
	rows, _ := db.ContactsByEmployer(t.Context(), "acc", 777)
	if len(rows) != 1 || rows[0].Source != "chat" {
		t.Fatalf("rows = %+v", rows)
	}
	if !strings.Contains(rows[0].Raw, "почта ивана") {
		t.Fatalf("source_quote not stored: %q", rows[0].Raw)
	}
}

func TestCollectFromMessageNoContactNoAI(t *testing.T) {
	db := newDB(t)
	run := &aiRunner{}
	if _, err := CollectFromMessage(t.Context(), db, run, "acc", 777, "ООО", "Спасибо, что откликнулись!"); err != nil {
		t.Fatal(err)
	}
	if run.calls != 0 {
		t.Fatal("plain messages must stay quiet")
	}
}

// guard: package compiles with config + logger fields wired
func TestFeatureContract(t *testing.T) {
	var cfg config.Config
	cfg.Features.Contacts.Enabled = true
	f := &Feature{Cfg: cfg, DB: nil, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return time.Unix(0, 0) }}
	if f.Name() != "contacts" || !f.Enabled() {
		t.Fatalf("contract mismatch: %q %v", f.Name(), f.Enabled())
	}
	f.Cfg.Features.Contacts.Enabled = false
	if f.Enabled() {
		t.Fatal("enabled must follow config")
	}
}

// --- Wave-3 review #5: transient failures must not lose event-driven tasks ---

type transientAPI struct {
	hhapiStub
	calls int
}

func (f *transientAPI) EmployerContacts(_ context.Context, _ int64) ([]hhapi.Contact, error) {
	f.calls++
	return nil, hhapi.ErrRateLimited
}

func TestHandleCollectContactsTransientRequeuesAndClearsMarker(t *testing.T) {
	db := newDB(t)
	api := &transientAPI{}
	q := queue.New()
	f := &Feature{
		Cfg:   config.Config{Features: config.Features{Contacts: config.Contacts{Enabled: true}}},
		DB:    db,
		API:   func(string) (hhapi.HHAPI, error) { return api, nil },
		Queue: q,
		Log:   slog.New(slog.DiscardHandler),
		Now:   func() time.Time { return time.Unix(0, 0) },
	}
	task := queue.Task{
		AccountID: "acc",
		Kind:      queue.KindCollectContacts,
		DedupKey:  "collect:acc:777",
		Payload:   CollectPayload{AccountID: "acc", EmployerID: 777, VacancyID: 42},
	}

	// chatops has already marked the employer as enqueued
	if err := db.FeatureStateSet(t.Context(), "chatops", "acc", kit11.CollectQueuedMarker(777), "1"); err != nil {
		t.Fatal(err)
	}

	err := f.HandleCollectContacts(t.Context(), task)
	if !errors.Is(err, hhapi.ErrRateLimited) {
		t.Fatalf("transient error must surface, got %v", err)
	}
	if n := q.Len("acc"); n != 1 {
		t.Fatalf("transient failure must requeue the task, len=%d", n)
	}
	if got, ok := q.Dequeue(t.Context(), "acc"); ok && got.DedupKey != "collect:acc:777" {
		t.Fatalf("requeued dedup key = %q", got.DedupKey)
	}
	// the chatops marker is cleared so a later sync can re-enqueue
	marker, _, err := db.FeatureStateGet(t.Context(), "chatops", "acc", kit11.CollectQueuedMarker(777))
	if err != nil || marker != "" {
		t.Fatalf("collect marker must be cleared, got %q err=%v", marker, err)
	}
}
