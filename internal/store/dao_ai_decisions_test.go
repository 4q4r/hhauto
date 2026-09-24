package store

import (
	"context"
	"testing"
	"time"
)

// AIDecisionsExperimentGroups backs `hhauto ai compare`: the schema has no
// variant column, so the two experiment keys are matched against prompt_ver
// or model, aggregated per (prompt_ver, model) inside the UTC day window.
func TestAIDecisionsExperimentGroups(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	today := time.Now().UTC().Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	lastWeek := time.Now().UTC().AddDate(0, 0, -8).Format("2006-01-02")

	add := func(ver, model string, needs bool, conf *float64, latencyMS int, cost float64, day string) {
		t.Helper()
		ts, err := time.Parse(time.RFC3339, day+"T12:00:00Z")
		if err != nil {
			t.Fatalf("bad test day %q: %v", day, err)
		}
		d := &AIDecision{
			AccountID: "acc-1", Task: "vacancy_scoring", Model: model, PromptVer: ver,
			Confidence: conf, NeedsHuman: needs, LatencyMS: latencyMS, CostUSD: cost,
			CreatedAt: ts,
		}
		if err := db.AIDecisionsAdd(ctx, d); err != nil {
			t.Fatalf("AIDecisionsAdd: %v", err)
		}
	}

	c := func(v float64) *float64 { return &v }

	// Experiment A (prompt_ver v1): 3 decisions, 1 needs_human, confidence
	// set twice (0.9 + 0.7), latency 100+200+300, cost 3×0.01.
	add("v1", "m1", false, c(0.9), 100, 0.01, today)
	add("v1", "m1", true, c(0.7), 200, 0.01, today)
	add("v1", "m1", false, nil, 300, 0.01, yesterday)
	// Experiment B (prompt_ver v2): 1 decision.
	add("v2", "m1", false, c(0.8), 50, 0.02, today)
	// Outside the window: same prompt_ver, but older than the period.
	add("v1", "m1", true, nil, 999, 9.99, lastWeek)
	// Unrelated group: matches neither key.
	add("other", "zz", true, nil, 1, 1, today)

	// The window starts yesterday: the three v1 rows are today+today+yesterday;
	// last week's v1 row and the unrelated group stay out.
	groups, err := db.AIDecisionsExperimentGroups(ctx, yesterday, [2]string{"v1", "v2"})
	if err != nil {
		t.Fatalf("AIDecisionsExperimentGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want 2 (v1/m1, v2/m1)", groups)
	}

	g := groups[0]
	if g.PromptVer != "v1" || g.Model != "m1" {
		t.Fatalf("group[0] = %s/%s, want v1/m1", g.PromptVer, g.Model)
	}
	if g.Decisions != 3 || g.NeedsHuman != 1 {
		t.Fatalf("group[0] decisions/needs_human = %d/%d, want 3/1", g.Decisions, g.NeedsHuman)
	}
	if g.ConfidenceN != 2 || g.ConfidenceSum < 1.5999 || g.ConfidenceSum > 1.6001 {
		t.Fatalf("group[0] confidence n/sum = %d/%v, want 2/1.6", g.ConfidenceN, g.ConfidenceSum)
	}
	if g.LatencyMS != 600 {
		t.Fatalf("group[0] latency = %d, want 600", g.LatencyMS)
	}
	if g.CostUSD < 0.0299 || g.CostUSD > 0.0301 {
		t.Fatalf("group[0] cost = %v, want 0.03", g.CostUSD)
	}

	g = groups[1]
	if g.PromptVer != "v2" || g.Model != "m1" || g.Decisions != 1 || g.NeedsHuman != 0 {
		t.Fatalf("group[1] = %+v, want v2/m1 1/0", g)
	}
	if g.ConfidenceN != 1 || g.ConfidenceSum < 0.7999 || g.ConfidenceSum > 0.8001 {
		t.Fatalf("group[1] confidence n/sum = %d/%v, want 1/0.8", g.ConfidenceN, g.ConfidenceSum)
	}
}
