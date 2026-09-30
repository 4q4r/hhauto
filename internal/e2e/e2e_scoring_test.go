package e2e

// E2E-T78 scoring policy: with ai_filter=true the fake-AI scorer decides
// into the band — an obvious hit (named vacancy) applies with
// needs_human=false, a data-dummy vacancy (no title) lands as ai_needs_human
// with a review item. Nothing else about the cascade changes.

import (
	"context"
	"testing"
	"time"
)

func TestE2E3ScoringNeedsHumanPolicy(t *testing.T) {
	env := newEnv(t, func(o *envOptions) {
		o.aiFilter = true
		o.extraTOML = "\n[ai.tasks.vacancy_scoring]\nmodel_profile = \"fast\"\n"
	})
	env.hh.extraUnnamedVacancy = true

	cancel, done := env.runEngine()
	// The unnamed vacancy must be skipped by the scorer before any popup;
	// the batch settles when both AI-passing vacancies (101 clean, 104
	// unseeded dup) are applied and the dummy is recorded as a skip.
	waitUntil(t, 30*time.Second, "scored batch settled", func() bool {
		return env.metricValue("apply.ok") == 2 &&
			env.metricValue("apply.skip.ai_needs_human") == 1 &&
			env.metricValue("apply.error") == 1
	})
	env.stopEngine(cancel, done)

	// Obvious hit → band ok, needs_human=false → applied.
	if app := env.application(vacClean); app == nil || app.Status != "applied" {
		t.Fatalf("clean application = %+v, want applied (the scorer decides ok)", app)
	}
	// Data dummy → needs_human → skipped before the popup, review item parked.
	if app := env.application(vacUnnamed); app != nil {
		t.Fatalf("dummy vacancy recorded an application: %+v", app)
	}
	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtx()
	open, err := env.db.ReviewItemsOpen(ctx, acctID, 10)
	if err != nil {
		t.Fatalf("review items: %v", err)
	}
	aiItems := 0
	for _, it := range open {
		if it.Kind == "ai_needs_human" {
			aiItems++
		}
	}
	if aiItems != 1 {
		t.Fatalf("open ai_needs_human review items = %d (%+v), want exactly one", aiItems, open)
	}
	// The scorer saw exactly the vacancies that reached F6 (101, 104, 105, 106).
	if got := env.ai.callCount("vacancy_scoring"); got != 4 {
		t.Fatalf("vacancy_scoring calls = %d, want 4", got)
	}
}
