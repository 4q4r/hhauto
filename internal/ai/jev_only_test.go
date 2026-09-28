package ai

// T46: Jev-only mode — for tasks listed in WithJevOnly the Jev prefilter
// verdict is FINAL: undecided (low confidence, confident-yes contacts),
// unavailable or schema-invalid Jev outcomes synthesize a needs_human
// review verdict instead of escalating to the LLM. Routing matrix under
// test:
//
//	jev-only + low confidence              → needs_human verdict, Source=jev, no LLM call
//	jev-only + endpoint error              → needs_human verdict, no LLM call
//	jev-only + schema-invalid verdict      → needs_human verdict, no LLM call
//	jev-only + confident yes contacts      → needs_human verdict, no LLM call
//	jev-only + decided                     → normal jev verdict (regression guard)
//	jev-only list empty / task not listed  → LLM escalation (T33 full-AI preserved)
//	WithJevOnly(non-jev task)              → New fails fast

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wantReview asserts the run ended as a jev-sourced needs_human verdict
// without a single LLM call.
func wantReview(t *testing.T, desc string, p *scriptedProvider, dec Decision, verdict json.RawMessage, wantReason string) {
	t.Helper()
	if dec.Source != SourceJev {
		t.Errorf("%s: decision source = %q, want %q", desc, dec.Source, SourceJev)
	}
	if !dec.NeedsHuman {
		t.Errorf("%s: decision needs_human = false, want true", desc)
	}
	if dec.Model != jevModelName {
		t.Errorf("%s: decision model = %q, want %q", desc, dec.Model, jevModelName)
	}
	if p.callCount() != 0 {
		t.Errorf("%s: LLM called %d times in jev-only mode, want 0", desc, p.callCount())
	}
	var v struct {
		Confidence float64 `json:"confidence"`
		NeedsHuman bool    `json:"needs_human"`
		Reason     string  `json:"needs_human_reason"`
	}
	if err := json.Unmarshal(verdict, &v); err != nil {
		t.Fatalf("%s: verdict decode: %v", desc, err)
	}
	if !v.NeedsHuman || v.Reason != wantReason {
		t.Errorf("%s: verdict needs_human=%v reason=%q, want true/%q", desc, v.NeedsHuman, v.Reason, wantReason)
	}
}

func TestJevOnlyLowConfidenceRoutesReviewNotLLM(t *testing.T) {
	fake := &fakeJev{conf: 0.5}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := &scriptedProvider{contents: []string{validClassVerdict}}
	ledger := &fakeLedger{}
	r := mustRunner(t, p, oneModel(""), jevTasks(), ledger, nil,
		WithJev(jevSolver(t, srv.URL, 0.85, jevAllTasks())),
		WithJevOnly(jevAllTasks()))
	verdict, dec, err := r.Run(context.Background(), "acc1", "message", "m1", "", TaskClassification,
		map[string]any{"message": map[string]any{"author": "HR", "text": "x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReview(t, "low confidence", p, dec, verdict, reasonJevUndecided)
	if len(ledger.recs) != 1 || ledger.recs[0].Source != SourceJev || !ledger.recs[0].NeedsHuman {
		t.Errorf("ledger records = %d, want 1 jev-sourced needs_human", len(ledger.recs))
	}
}

func TestJevOnlyEndpointErrorRoutesReview(t *testing.T) {
	fake := &fakeJev{conf: 0.9, status: http.StatusInternalServerError}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := &scriptedProvider{contents: []string{validClassVerdict}}
	r := mustRunner(t, p, oneModel(""), jevTasks(), &fakeLedger{}, nil,
		WithJev(jevSolver(t, srv.URL, 0.85, jevAllTasks())),
		WithJevOnly(jevAllTasks()))
	verdict, dec, err := r.Run(context.Background(), "acc1", "message", "m1", "", TaskClassification,
		map[string]any{"message": map[string]any{"author": "HR", "text": "x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReview(t, "endpoint 500", p, dec, verdict, reasonJevUndecided)
}

func TestJevOnlySchemaInvalidVerdictRoutesReview(t *testing.T) {
	// confidence 1.5 passes the solver threshold but fails the strict
	// schema (confidence <= 1) — a jev-only task must end in a review
	// verdict, never trust the malformed output or reach the LLM.
	fake := &fakeJev{conf: 1.5}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := &scriptedProvider{contents: []string{validClassVerdict}}
	r := mustRunner(t, p, oneModel(""), jevTasks(), &fakeLedger{}, nil,
		WithJev(jevSolver(t, srv.URL, 0.85, jevAllTasks())),
		WithJevOnly(jevAllTasks()))
	verdict, dec, err := r.Run(context.Background(), "acc1", "message", "m1", "", TaskClassification,
		map[string]any{"message": map[string]any{"author": "HR", "text": "x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReview(t, "schema-invalid verdict", p, dec, verdict, reasonSchemaInvalid)
}

func TestJevOnlyContactYesRoutesReview(t *testing.T) {
	// A confident "yes, there are contacts" is undecided for Jev (it cannot
	// extract values): jev-only routes it to review instead of the LLM.
	fake := &fakeJev{conf: 0.9, pick: func(q JevQuestion) any {
		if q.Type == JevQuestionNoul {
			return 1.0
		}
		return "phone"
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := &scriptedProvider{contents: []string{validContactVerdict}}
	r := mustRunner(t, p, oneModel(""), jevTasks(), &fakeLedger{}, nil,
		WithJev(jevSolver(t, srv.URL, 0.85, jevAllTasks())),
		WithJevOnly(jevAllTasks()))
	verdict, dec, err := r.Run(context.Background(), "acc1", "message", "contact", "", TaskContactExtract,
		map[string]any{"text": "пиши в телеграм @ivan", "source": "chat_message"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantReview(t, "contacts yes", p, dec, verdict, reasonJevUndecided)
}

func TestJevOnlyDecidedUnchanged(t *testing.T) {
	// A decided Jev verdict stays the final answer; the flag only changes
	// what happens when Jev does NOT decide.
	fake := &fakeJev{conf: 0.9}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := &scriptedProvider{contents: []string{validClassVerdict}}
	r := mustRunner(t, p, oneModel(""), jevTasks(), &fakeLedger{}, nil,
		WithJev(jevSolver(t, srv.URL, 0.85, jevAllTasks())),
		WithJevOnly(jevAllTasks()))
	verdict, dec, err := r.Run(context.Background(), "acc1", "message", "m1", "", TaskClassification,
		map[string]any{"message": map[string]any{"author": "HR", "text": "x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dec.Source != SourceJev || dec.NeedsHuman || dec.Confidence != 0.9 {
		t.Errorf("decision = %+v, want decided jev verdict", dec)
	}
	if p.callCount() != 0 {
		t.Errorf("LLM called %d times, want 0", p.callCount())
	}
	var v struct {
		Category string `json:"category"`
	}
	if err := json.Unmarshal(verdict, &v); err != nil || v.Category != "rejection" {
		t.Errorf("category = %q (err %v), want rejection", v.Category, err)
	}
}

func TestJevOnlyEmptyListMeansFullAI(t *testing.T) {
	// An empty jev-only list keeps the T33 semantics: low confidence
	// escalates to the LLM.
	fake := &fakeJev{conf: 0.5}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := &scriptedProvider{contents: []string{validClassVerdict}}
	r := mustRunner(t, p, oneModel(""), jevTasks(), &fakeLedger{}, nil,
		WithJev(jevSolver(t, srv.URL, 0.85, jevAllTasks())),
		WithJevOnly(nil))
	_, dec, err := r.Run(context.Background(), "acc1", "message", "m1", "", TaskClassification,
		map[string]any{"message": map[string]any{"author": "HR", "text": "x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantEscalated(t, "empty jev-only list", p, dec)
}

func TestJevOnlyNonMemberTaskEscalates(t *testing.T) {
	// Only the listed tasks are final: an unlisted enrolled task keeps the
	// T33 escalation on low confidence.
	fake := &fakeJev{conf: 0.5}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	p := &scriptedProvider{contents: []string{validClassVerdict}}
	r := mustRunner(t, p, oneModel(""), jevTasks(), &fakeLedger{}, nil,
		WithJev(jevSolver(t, srv.URL, 0.85, jevAllTasks())),
		WithJevOnly([]TaskName{TaskClassification}))
	_, dec, err := r.Run(context.Background(), "acc1", "chat", "7", "", TaskLinkDetection,
		map[string]any{"urls": []string{"https://forms.gle/x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantEscalated(t, "task not in jev_only list", p, dec)
}

func TestWithJevOnlyRejectsNonJevTask(t *testing.T) {
	p := &scriptedProvider{}
	_, err := New(map[string]Provider{"p": p}, oneModel(""), jevTasks(), &fakeLedger{}, nil, testLogger(),
		WithJevOnly([]TaskName{TaskVacancyScoring}))
	if err == nil || !strings.Contains(err.Error(), "not jev-compatible") {
		t.Fatalf("err = %v, want containing %q", err, "not jev-compatible")
	}
}
