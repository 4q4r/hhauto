//go:build live

package multiapply

// T85 LIVE SOLVE PROBE (manual, network + owner session + owner AI key
// required; never in the default gate):
//
//	go test -tags live -run TestLiveT85SolvePath -v ./internal/features/multiapply/
//
// Runs the REAL solve path (VacancyTests fetch → ai.TaskTestAnswer → the
// T85 post-check) for vacancy 137465889 — one of the four vacancies the
// 2026-10-03 daemon pass lost to the v2 escalation. The submission POST is
// deliberately NOT sent: the wire was already live-proven in T83
// (137339427), and this probe must stay read-only against hh. The owner's
// config, cookies and session are read-only inputs; the feature writes go
// to a throwaway DB.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

func TestLiveT85SolvePath(t *testing.T) {
	cfg, realDB, client := t83LoadAll(t)
	defer realDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	prof, err := profile.LoadUserContext(ctx, *cfg, "main", realDB, client)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}

	tasks := make(map[string]config.AITask, len(cfg.AI.Tasks)+1)
	for k, task := range cfg.AI.Tasks {
		tasks[k] = task
	}
	tasks[string(ai.TaskTestAnswer)] = cfg.AI.Tasks["cover_letter"] // owner's letter model profile

	probeDB, err := store.Open(ctx, fmt.Sprintf("%s/t85-solve.db", t.TempDir()), store.Options{})
	if err != nil {
		t.Fatalf("temp db: %v", err)
	}
	defer probeDB.Close()
	f := New(Options{
		Config: *cfg, DB: probeDB,
		Runner:     t83ProbeRunner(t, cfg, tasks),
		ProfileFor: func(context.Context, string) (profile.Profile, error) { return prof, nil },
		Log:        quietLogger(),
	})

	const vacancy = int64(137465889)
	v := hhapi.Vacancy{ID: vacancy}
	test, err := client.VacancyTests(ctx, vacancy)
	if err != nil {
		t.Skipf("vacancy test no longer fetchable (responded/expired?): %v", err)
	}
	kinds := 0
	for _, task := range test.Tasks {
		if len(task.Solutions) > 0 {
			kinds++
		}
	}
	t.Logf("fetched: required=%t tasks=%d (choice=%d)", test.Required, len(test.Tasks), kinds)

	solveCtx, cancel := f.solveContext(ctx)
	defer cancel()
	applyTest := f.solveTest(solveCtx, "t85-probe", "main", v, test, prof)
	cancel()
	if applyTest == nil {
		t.Fatalf("solve returned nil — the T85 post-check must answer choice questions")
	}
	raw, _ := json.Marshal(applyTest.Answers)
	var pretty []map[string]any
	_ = json.Unmarshal(raw, &pretty)
	t.Logf("solved %d/%d tasks (required=%t): %s", len(applyTest.Answers), len(test.Tasks),
		applyTest.Required, mustIndent(t, pretty))
	if len(applyTest.Answers) == 0 {
		t.Fatalf("zero answers — solve must answer choice questions (T85)")
	}
	if test.Required && len(applyTest.Answers) < len(test.Tasks) {
		t.Fatalf("required test answered %d/%d — the gate must have returned nil", len(applyTest.Answers), len(test.Tasks))
	}
}

func mustIndent(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
