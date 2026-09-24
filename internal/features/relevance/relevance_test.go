package relevance

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "rel.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fakeRunner is an ai.Runner stub returning canned verdicts and capturing
// the task input for assertions.
type fakeRunner struct {
	ai.Runner
	verdict string
	err     error
	calls   int
	input   string
}

func (r *fakeRunner) Run(ctx context.Context, accountID, entityType, entityID, runID string,
	task ai.TaskName, input any) (json.RawMessage, ai.Decision, error) {
	r.calls++
	b, _ := json.Marshal(input)
	r.input = string(b)
	if r.err != nil {
		return nil, ai.Decision{}, r.err
	}
	return json.RawMessage(r.verdict), ai.Decision{NeedsHuman: false}, nil
}

func scoringVerdict(score int, band string, needsHuman, injection bool) string {
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.9, "needs_human": needsHuman, "needs_human_reason": nil,
		"injection_suspected": injection, "score": score, "band": band,
		"reasons": []string{"r1"}, "used_facts": []string{"f1"}, "missing_data": []string{},
	})
	return string(b)
}

func vacancy(id int64, name string) hhapi.Vacancy {
	return hhapi.Vacancy{ID: id, Name: name, EmployerID: 9001, Description: "desc"}
}

func newFilter(t *testing.T, db *store.DB, runner ai.Runner) *Filter {
	t.Helper()
	prof := profile.Profile{
		AccountID:    "acc",
		ResumeDigest: "Резюме: Go, 5 лет",
		ProfileText:  "Ищу бэкенд-роли",
		Facts:        map[string]string{"target_roles": "Go-разработчик, Backend"},
	}
	return New(runner, db, "acc", "run1", prof,
		Rules{ExcludeWords: []string{"стажировка"}, TargetRoles: RolesFromFacts(prof)}, nil)
}

func TestScoreAboveThresholdPasses(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(75, "ok", false, false)}
	f := newFilter(t, db, runner)

	v, pass, reason := f.Pass(context.Background(), vacancy(777, "Go Dev"), "hash1", 60)
	if !pass || reason != "relevance_ok" {
		t.Fatalf("pass=%v reason=%q, want true/relevance_ok", pass, reason)
	}
	if v.Score != 75 || v.Band != "ok" {
		t.Fatalf("verdict = %+v", v)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestScoreBelowThresholdSkips(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(30, "weak", false, false)}
	f := newFilter(t, db, runner)

	_, pass, reason := f.Pass(context.Background(), vacancy(778, "Стажер"), "hash1", 60)
	if pass || reason != "score_below_threshold" {
		t.Fatalf("pass=%v reason=%q, want false/score_below_threshold", pass, reason)
	}
}

func TestNeedsHumanSkipsWithReviewItem(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(50, "gray", true, false)}
	f := newFilter(t, db, runner)

	_, pass, reason := f.Pass(context.Background(), vacancy(779, "Gray"), "hash1", 60)
	if pass || reason != "ai_needs_human" {
		t.Fatalf("pass=%v reason=%q, want false/ai_needs_human", pass, reason)
	}
	open, err := db.ReviewItemsOpen(context.Background(), "acc", 10)
	if err != nil || len(open) != 1 {
		t.Fatalf("open items = %d err=%v, want 1", len(open), err)
	}
}

func TestInjectionSkipsWithReviewItem(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(90, "strong", false, true)}
	f := newFilter(t, db, runner)

	_, pass, reason := f.Pass(context.Background(), vacancy(780, "Inject"), "hash1", 60)
	if pass || reason != "ai_injection" {
		t.Fatalf("pass=%v reason=%q, want false/ai_injection", pass, reason)
	}
	if open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10); len(open) != 1 {
		t.Fatalf("open items = %d, want 1", len(open))
	}
}

func TestVerdictCachedByVacancyAndResume(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(80, "strong", false, false)}
	f := newFilter(t, db, runner)

	if _, pass, _ := f.Pass(context.Background(), vacancy(781, "A"), "hashX", 60); !pass {
		t.Fatalf("first pass = false")
	}
	if _, pass, _ := f.Pass(context.Background(), vacancy(781, "A"), "hashX", 60); !pass {
		t.Fatalf("second pass = false")
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (second served from feature_state cache)", runner.calls)
	}
	// Different resume → different cache key → runner runs again.
	if _, _, _ = f.Pass(context.Background(), vacancy(781, "A"), "hashY", 60); runner.calls != 2 {
		t.Fatalf("runner calls = %d, want 2 (other resume)", runner.calls)
	}
}

func TestCachedNeedsHumanSkipsWithoutDuplicateReview(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(45, "gray", true, false)}
	f := newFilter(t, db, runner)

	_, p1, _ := f.Pass(context.Background(), vacancy(782, "G"), "hash1", 60)
	_, p2, _ := f.Pass(context.Background(), vacancy(782, "G"), "hash1", 60)
	if p1 || p2 {
		t.Fatalf("both passes must skip")
	}
	if open, _ := db.ReviewItemsOpen(context.Background(), "acc", 10); len(open) != 1 {
		t.Fatalf("open items = %d, want 1 (no duplicate from cache)", len(open))
	}
}

func TestRunnerErrorFailsClosed(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{err: errors.New("budget hard stop")}
	f := newFilter(t, db, runner)

	_, pass, reason := f.Pass(context.Background(), vacancy(783, "X"), "hash1", 60)
	if pass || reason != "ai_error" {
		t.Fatalf("pass=%v reason=%q, want false/ai_error (fail-closed)", pass, reason)
	}
}

func TestInputCarriesVacancyProfileAndRules(t *testing.T) {
	db := testDB(t)
	runner := &fakeRunner{verdict: scoringVerdict(70, "ok", false, false)}
	f := newFilter(t, db, runner)

	f.Pass(context.Background(), vacancy(784, "Go Backend"), "hash1", 60)
	for _, want := range []string{`"vacancy"`, `"resume_digest"`, `"profile_text"`, `"facts"`, "Go Backend", "стажировка", "Go-разработчик"} {
		if !strings.Contains(runner.input, want) {
			t.Fatalf("input missing %q in %s", want, runner.input)
		}
	}
}
