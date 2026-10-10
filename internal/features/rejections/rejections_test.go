package rejections

// T123 pipeline tests: fake discard-negotiations API + scripted AI runner →
// store + report. Covers: full happy path with evidence/guess marking,
// fabricated-verdict skip, re-analysis idempotency, and the empty case.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// --- fakes ---------------------------------------------------------------

type fakeAPI struct {
	discard   map[int][]hhapi.Negotiation
	histories map[string][]hhapi.ChatMessage
	cards     map[int64]*hhapi.Vacancy
	cardErr   map[int64]error
}

func (f *fakeAPI) NegotiationsDiscard(_ context.Context, page int) ([]hhapi.Negotiation, error) {
	return f.discard[page], nil
}

func (f *fakeAPI) ChatHistory(_ context.Context, chatID string) ([]hhapi.ChatMessage, error) {
	return f.histories[chatID], nil
}

func (f *fakeAPI) VacancyCard(_ context.Context, id int64) (*hhapi.Vacancy, error) {
	if err := f.cardErr[id]; err != nil {
		return nil, err
	}
	if c, ok := f.cards[id]; ok {
		return c, nil
	}
	// Mirror the real client contract: a missing card is an error,
	// never a nil card without one.
	return nil, errors.New("hhapi: vacancy card: not found")
}

type fakeRunner struct {
	verdicts []string
	calls    int
}

func (f *fakeRunner) Run(_ context.Context, _, _, _, _ string, _ ai.TaskName, _ any) (json.RawMessage, ai.Decision, error) {
	if f.calls >= len(f.verdicts) {
		return nil, ai.Decision{}, fmt.Errorf("fakeRunner: no verdict scripted for call %d", f.calls+1)
	}
	v := f.verdicts[f.calls]
	f.calls++
	return json.RawMessage(v), ai.Decision{}, nil
}

// --- helpers --------------------------------------------------------------

func testOptions(t *testing.T, api APIClient, runner ai.Runner, db *store.DB, out io.Writer) Options {
	t.Helper()
	return Options{
		AccountID: "main",
		API:       api,
		Runner:    runner,
		DB:        db,
		Profile:   profile.Profile{AccountID: "main", ResumeDigest: "опыт 4 года, PostgreSQL", ProfileText: "стек: Go, PostgreSQL"},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Out:       out,
		DataDir:   t.TempDir(),
		Now:       func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) },
	}
}

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

const verdictGrounded = `{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"root_cause":"Просят Kubernetes, которого нет в резюме","critique":"Письмо слишком общее","improved_letter":"Разрабатываю на Go 4 года, в основном PostgreSQL.","missing_skills":["Kubernetes","Terraform"],"about_me_rec":"","takeaway":"Добавить Kubernetes в навыки"}`
const verdictGuess = `{"confidence":0.6,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"root_cause":"Возможно, не сошлись по грейду","critique":"","improved_letter":"","missing_skills":[],"about_me_rec":"","takeaway":"Уточнить требования"}`

// --- tests ----------------------------------------------------------------

func TestAnalyzePipelineStoresVerdictsAndReports(t *testing.T) {
	api := &fakeAPI{
		discard: map[int][]hhapi.Negotiation{
			1: {
				// Live shape: the mixed list hh serves despite
				// filter=discard — the RESPONSE item must be skipped.
				{ID: "9004", VacancyID: 1004, State: "RESPONSE", UpdatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)},
				{ID: "9001", VacancyID: 1001, ChatID: "555001", State: "DISCARD", UpdatedAt: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)},
				{ID: "9002", VacancyID: 1002, State: "DISCARD", UpdatedAt: time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)},
			},
		},
		histories: map[string][]hhapi.ChatMessage{
			"555001": {
				{ID: "m1", Direction: "out", Text: "Здравствуйте, откликнулся"},
				{ID: "m2", Direction: "in", Text: "Здравствуйте! К сожалению, сейчас ищем более опытного специалиста по Kubernetes"},
			},
		},
		cards: map[int64]*hhapi.Vacancy{
			1001: {ID: 1001, Name: "DevOps-инженер", EmployerName: "Acme", Description: "<p>Нужен Kubernetes, Terraform.</p>"},
		},
	}
	runner := &fakeRunner{verdicts: []string{verdictGrounded, verdictGuess}}
	db := openTestDB(t)
	var out bytes.Buffer

	res, err := Analyze(context.Background(), testOptions(t, api, runner, db, &out))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Seen != 2 || res.Analyzed != 2 || res.SkippedExisting != 0 || res.SkippedAI != 0 {
		t.Fatalf("result = %+v", res)
	}
	if runner.calls != 2 {
		t.Fatalf("runner calls = %d, want 2 (the RESPONSE item is not a rejection)", runner.calls)
	}
	if res.Guesses != 1 {
		t.Fatalf("guesses = %d, want 1", res.Guesses)
	}

	rows, err := db.RejectionsList(context.Background(), "main")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	byVacancy := map[int64]store.Rejection{}
	for _, r := range rows {
		byVacancy[r.VacancyID] = r
	}
	if r := byVacancy[1001]; !r.EvidenceBased {
		t.Errorf("vacancy 1001: evidence_based = false, want true (employer words in chat)")
	}
	if r := byVacancy[1001].VacancyName; r != "DevOps-инженер" {
		t.Errorf("vacancy 1001 name = %q, want the card's name", r)
	}
	if r := byVacancy[1002]; r.EvidenceBased {
		t.Errorf("vacancy 1002: evidence_based = true, want false (no employer words)")
	}
	if got := byVacancy[1001].MissingSkills; len(got) != 2 || got[0] != "Kubernetes" {
		t.Errorf("missing_skills = %+v", got)
	}

	// Console report: evidence shares, guesses without shares, fix-plan.
	text := out.String()
	for _, want := range []string{"Разбор отказов", "навыки/стек", "Догадки", "Fix-plan", "Kubernetes"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	// The md report exists next to the data dir.
	if res.ReportPath == "" {
		t.Fatal("ReportPath empty")
	}
	if _, err := os.Stat(res.ReportPath); err != nil {
		t.Fatalf("md report: %v", err)
	}
}

func TestAnalyzePipelineSkipsFabricatedVerdict(t *testing.T) {
	api := &fakeAPI{
		discard: map[int][]hhapi.Negotiation{
			1: {{ID: "9001", VacancyID: 1001, VacancyName: "DevOps", EmployerName: "Acme", State: "DISCARD", UpdatedAt: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)}},
		},
	}
	// The real pipeline flags invented numbers as needs_human; the feature
	// must not store such a verdict.
	verdict := `{"confidence":0.9,"needs_human":true,"needs_human_reason":"number_not_in_sources","injection_suspected":false,"root_cause":"x","critique":"","improved_letter":"Сократил уязвимости на 45%.","missing_skills":[],"about_me_rec":"","takeaway":""}`
	runner := &fakeRunner{verdicts: []string{verdict}}
	db := openTestDB(t)
	var out bytes.Buffer

	res, err := Analyze(context.Background(), testOptions(t, api, runner, db, &out))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.SkippedAI != 1 || res.Analyzed != 0 {
		t.Fatalf("result = %+v", res)
	}
	rows, _ := db.RejectionsList(context.Background(), "main")
	if len(rows) != 0 {
		t.Fatalf("fabricated verdict stored: %+v", rows)
	}
}

func TestAnalyzePipelineIsIdempotent(t *testing.T) {
	api := &fakeAPI{
		discard: map[int][]hhapi.Negotiation{
			1: {{ID: "9001", VacancyID: 1001, VacancyName: "DevOps", EmployerName: "Acme", State: "DISCARD", UpdatedAt: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)}},
		},
	}
	runner := &fakeRunner{verdicts: []string{verdictGrounded}}
	db := openTestDB(t)
	var out bytes.Buffer
	opt := testOptions(t, api, runner, db, &out)

	if _, err := Analyze(context.Background(), opt); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := Analyze(context.Background(), opt)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Analyzed != 0 || res.SkippedExisting != 1 || res.Seen != 1 {
		t.Fatalf("second run result = %+v", res)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (already-analyzed must not burn tokens)", runner.calls)
	}
	rows, _ := db.RejectionsList(context.Background(), "main")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

func TestAnalyzePipelineEmptyCase(t *testing.T) {
	api := &fakeAPI{discard: map[int][]hhapi.Negotiation{}}
	runner := &fakeRunner{}
	db := openTestDB(t)
	var out bytes.Buffer

	res, err := Analyze(context.Background(), testOptions(t, api, runner, db, &out))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Seen != 0 || res.Analyzed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if runner.calls != 0 {
		t.Fatalf("runner called %d times on empty input", runner.calls)
	}
	if !strings.Contains(out.String(), "не найдено") {
		t.Errorf("empty report = %q", out.String())
	}
}

func TestAnalyzeWindowExcludesOldRejections(t *testing.T) {
	api := &fakeAPI{
		discard: map[int][]hhapi.Negotiation{
			1: {{ID: "9001", VacancyID: 1001, VacancyName: "Old", EmployerName: "Acme", State: "DISCARD", UpdatedAt: time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC)}},
		},
	}
	runner := &fakeRunner{}
	db := openTestDB(t)
	var out bytes.Buffer
	opt := testOptions(t, api, runner, db, &out)
	opt.Days = 30

	res, err := Analyze(context.Background(), opt)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Seen != 0 || res.Analyzed != 0 || runner.calls != 0 {
		t.Fatalf("out-of-window rejection analyzed: %+v", res)
	}
}

func TestAnalyzeCardFetchFailureStillAnalyzes(t *testing.T) {
	api := &fakeAPI{
		discard: map[int][]hhapi.Negotiation{
			1: {{ID: "9001", VacancyID: 1001, VacancyName: "DevOps-инженер", EmployerName: "Acme", State: "DISCARD", UpdatedAt: time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)}},
		},
		cardErr: map[int64]error{1001: errors.New("gone")},
	}
	runner := &fakeRunner{verdicts: []string{verdictGuess}}
	db := openTestDB(t)
	var out bytes.Buffer

	res, err := Analyze(context.Background(), testOptions(t, api, runner, db, &out))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Analyzed != 1 {
		t.Fatalf("result = %+v, want analysis to proceed without the card", res)
	}
	rows, _ := db.RejectionsList(context.Background(), "main")
	if len(rows) != 1 || rows[0].VacancyName != "DevOps-инженер" {
		t.Fatalf("rows = %+v", rows)
	}
}
