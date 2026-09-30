package multiapply

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

// T80: a vacancy with an obligatory hh test is SOLVED in the apply flow
// (owner requirement: «Тест сам решать должен, а не с 400 падать»), not
// skipped. The wire contract (donor-derived, fail-closed everywhere):
// GET /applicant/vacancy_response carries the vacancyTests state,
// POST /applicant/vacancy_response/popup submits the answers together with
// the regular apply fields. Every solve failure falls back to the T77 skip.

// testPageFixture renders the fake-hh vacancy_response document whose
// embedded state carries the vacancy's test: one choice task and one open
// task (startTime and solution ids as raw JSON numbers — the parser must
// accept both string and number literals verbatim).
func testPageFixture(vacancy int64) string {
	return fmt.Sprintf(`<html><head><title>Отклик</title></head><body>
<script>window.__INITIAL_STATE__={"vacancyTests":{%q:{"uidPk":"uid-%d","guid":"guid-%d","startTime":1727700000,"required":true,"tasks":[
{"id":"t1","description":"Готовы ли вы работать с Go?","candidateSolutions":[{"id":101,"text":"Да"},{"id":102,"text":"Нет"}]},
{"id":"t2","description":"Ваш опыт с Docker в проде?","candidateSolutions":[]}]}}}</script>
</body></html>`, fmt.Sprint(vacancy), vacancy, vacancy)
}

// testVerdict is the test_answer verdict for the fixture: task t1 → «Да»
// (index 0), task t2 → a short factual open answer.
var testVerdict = `{"answers":[
{"question_id":"1","kind":"choice","choice_indices":[0],"open_text":null,"based_on_profile":false,"facts_used":[],"note":null},
{"question_id":"2","kind":"open","choice_indices":null,"open_text":"Разворачивал сервисы в Docker в проде.","based_on_profile":true,"facts_used":[],"note":null}],
"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false}`

func testVacancyWithPopup(t *testing.T, h *harness, id int64, popup string) {
	t.Helper()
	h.searchItems = []string{vacItem(id, "Go Developer", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[id] = popup
}

func testPopupHasTests() string {
	return `{"type":"POPUP","responseStatus":{"test":{"hasTests":true}},"letterRequired":false,"test":{"hasTests":true},"acceptAutoResponse":true,"chatWritePossibility":"ENABLED"}`
}

// TestTestSolvedApplyCompletes: the test is answered by the AI and submitted
// together with the apply; the vacancy lands in the ledger as applied.
func TestTestSolvedApplyCompletes(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777101, testPopupHasTests())
	h.testsByVac[777101] = testPageFixture(777101)
	h.runner.testAnswers = testVerdict

	h.run(t)

	if h.testsReqs[777101] != 1 {
		t.Fatalf("vacancy_response requests = %d, want 1 (the test must be fetched)", h.testsReqs[777101])
	}
	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777101 {
		t.Fatalf("applies = %v, want [777101] (solved test completes the apply)", h.applyCalls)
	}
	if len(h.applyBodies) != 1 {
		t.Fatalf("apply bodies = %d, want 1", len(h.applyBodies))
	}
	body := h.applyBodies[0]
	for _, want := range []string{"uidPk=uid-777101", "guid=guid-777101", "startTime=1727700000",
		"testRequired=true", "withoutTest=no", "lux=true", "task_t1=101", "task_t2_text="} {
		if !strings.Contains(body, want) {
			t.Fatalf("apply body %q misses %q", body, want)
		}
	}
	if !strings.Contains(body, "task_t2_text=Razvertyval") && !strings.Contains(body, "task_t2_text=") {
		t.Fatalf("apply body %q misses the open answer", body)
	}
	m := mustMetrics(t, h.db)
	if m["apply.test_solved"] != 1 {
		t.Fatalf("apply.test_solved = %d, want 1 (metrics %v)", m["apply.test_solved"], m)
	}
	if m["apply.skip.test_required"] != 0 {
		t.Fatalf("apply.skip.test_required = %d, want 0", m["apply.skip.test_required"])
	}
}

// TestTestFabricatedAnswerFallsBackToSkip: an AI answer that does not
// resolve to a real solution (post-check cut) never reaches hh — the
// vacancy falls back to the T77 skip, visibly.
func TestTestFabricatedAnswerFallsBackToSkip(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777102, testPopupHasTests())
	h.testsByVac[777102] = testPageFixture(777102)
	// Fabricated choice: index 5 does not exist (two options only).
	h.runner.testAnswers = strings.Replace(testVerdict, `"choice_indices":[0]`, `"choice_indices":[5]`, 1)

	h.run(t)

	sawTestAnswer := false
	for _, c := range h.runner.calls {
		if c == ai.TaskTestAnswer {
			sawTestAnswer = true
		}
	}
	if !sawTestAnswer {
		t.Fatalf("AI test_answer was never called — the solve must be attempted before the fallback")
	}
	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (a cut answer must not POST)", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1 (fallback skip)", m)
	}
}

// TestTestCaptchaSkipsWithoutSolving: a captcha on the test page has no
// solver (owner decision) — the vacancy falls back to the T77 skip and the
// AI is never spent.
func TestTestCaptchaSkipsWithoutSolving(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777103, testPopupHasTests())
	h.testsByVac[777103] = `<html><body><img src="/captcha/challenge"><form>showcaptcha</form></body></html>`

	h.run(t)

	if h.testsReqs[777103] != 1 {
		t.Fatalf("vacancy_response requests = %d, want 1", h.testsReqs[777103])
	}
	for _, c := range h.runner.calls {
		if c == ai.TaskTestAnswer {
			t.Fatal("AI test_answer must not run on a captcha page")
		}
	}
	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1", m)
	}
}

// TestTestTimeoutFallsBackToSkip: the solve runs under a time budget — an
// AI that never answers burns the budget and the vacancy falls back to the
// T77 skip (no POST, visible skip reason).
func TestTestTimeoutFallsBackToSkip(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777104, testPopupHasTests())
	h.testsByVac[777104] = testPageFixture(777104)
	h.runner.blockTestAnswer = true

	f := h.feature(t)
	f.opt.TestSolveTimeout = 1 // nanoseconds: the budget is already spent
	h.q, h.reg = queue.New(), scheduler.NewRegistry()
	if err := f.RegisterTasks(context.Background(), h.q, h.reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (timeout = fallback skip)", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1", m)
	}
}
