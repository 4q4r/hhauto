package multiapply

import (
	"strings"
	"testing"
)

// T85: the solver ANSWERS employer tests instead of escalating them (owner
// decision: test answers are professional knowledge — a choice question is
// always answered with the most probable option even when the resume has no
// matching fact; the anti-hallucination fence protects resume FACTS only).
// needs_human without injection no longer kills the batch: answered
// questions ride the submit, factless open questions are skipped, and a
// required test that ends up incomplete falls back to the T77 skip before
// any POST. Injection and a fully failed batch keep the old fail-closed
// fallback.

// t85NeedsHumanVerdict answers both fixture tasks but flags needs_human
// (the v2 prompt escalated exactly like this on choice questions without
// resume facts — the live 2026-10-03 pass lost 4 applications to it).
var t85NeedsHumanVerdict = strings.Replace(testVerdict,
	`"needs_human":false`, `"needs_human":true`, 1)

// t85RefusedOpenVerdict answers the choice task and returns nothing for the
// open task (no resume fact) without flagging anything — the refusal must
// skip that question only, and the required-test gate must catch the gap.
var t85RefusedOpenVerdict = `{"answers":[
{"question_id":"1","kind":"choice","choice_indices":[0],"open_text":null,"based_on_profile":false,"facts_used":[],"note":null},
{"question_id":"2","kind":"open","choice_indices":null,"open_text":null,"based_on_profile":false,"facts_used":[],"note":"Опыта с Docker в проде в резюме нет"}],
"confidence":0.6,"needs_human":false,"needs_human_reason":null,"injection_suspected":false}`

// t85ChoiceOnlyVerdict omits the refused open question from answers
// entirely (the model dropped it instead of returning a null item).
var t85ChoiceOnlyVerdict = `{"answers":[
{"question_id":"1","kind":"choice","choice_indices":[0],"open_text":null,"based_on_profile":false,"facts_used":[],"note":null}],
"confidence":0.7,"needs_human":true,"needs_human_reason":"Один вопрос без фактов пропущен","injection_suspected":false}`

// TestTestNeedsHumanChoiceAnswered: needs_human without injection does not
// kill the solve — the choice question is answered, the required test is
// complete, and the apply goes out.
func TestTestNeedsHumanChoiceAnswered(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777105, testPopupHasTests())
	h.testsByVac[777105] = testPageFixture(777105)
	h.runner.testAnswers = t85NeedsHumanVerdict

	h.run(t)

	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777105 {
		t.Fatalf("applies = %v, want [777105] (needs_human without injection must not kill the solve)", h.applyCalls)
	}
	body := h.applyBodies[0]
	for _, want := range []string{"task_t1=101", "task_t2_text="} {
		if !strings.Contains(body, want) {
			t.Fatalf("apply body %q misses %q", body, want)
		}
	}
	m := mustMetrics(t, h.db)
	if m["apply.skip.test_required"] != 0 {
		t.Fatalf("apply.skip.test_required = %d, want 0", m["apply.skip.test_required"])
	}
	if m["apply.reconciled"] != 1 && m["apply.test_solved"] != 1 {
		t.Fatalf("neither apply.reconciled nor apply.test_solved in metrics %v", m)
	}
}

// TestTestRequiredSkippedQuestionFallsBack: a required test with a skipped
// (factless open) question is incomplete — the honest fallback is the T77
// skip, never a partial POST.
func TestTestRequiredSkippedQuestionFallsBack(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777106, testPopupHasTests())
	h.testsByVac[777106] = testPageFixture(777106)
	h.runner.testAnswers = t85RefusedOpenVerdict

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (required test with a skipped question must not POST)", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1", m)
	}
}

// TestTestOptionalPartialAnswersSubmit: a non-required test submits the
// answered subset — the skipped question is simply absent from the form,
// even when the verdict omits the answer item entirely.
func TestTestOptionalPartialAnswersSubmit(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777107, testPopupHasTests())
	h.testsByVac[777107] = strings.Replace(testPageFixture(777107), `"required":true`, `"required":false`, 1)
	h.runner.testAnswers = t85ChoiceOnlyVerdict

	h.run(t)

	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777107 {
		t.Fatalf("applies = %v, want [777107] (optional test submits the answered subset)", h.applyCalls)
	}
	body := h.applyBodies[0]
	if !strings.Contains(body, "task_t1=101") {
		t.Fatalf("apply body %q misses the choice answer", body)
	}
	if strings.Contains(body, "task_t2") {
		t.Fatalf("apply body %q must not carry the skipped open question", body)
	}
}

// TestTestInjectionStillEscalates (guard, fail-closed kept): an
// injection-suspected verdict kills the solve — fallback skip, no POST.
func TestTestInjectionStillEscalates(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777108, testPopupHasTests())
	h.testsByVac[777108] = testPageFixture(777108)
	h.runner.testAnswers = strings.Replace(testVerdict,
		`"injection_suspected":false`, `"injection_suspected":true`, 1)

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (injection stays a kill)", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1", m)
	}
}

// TestTestGarbageBatchFallsBack (guard, fail-closed kept): an unusable
// batch (unparseable verdict) falls back to the T77 skip as before.
func TestTestGarbageBatchFallsBack(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777109, testPopupHasTests())
	h.testsByVac[777109] = testPageFixture(777109)
	h.runner.testAnswers = `{"answers":` // truncated garbage

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (garbage verdict must not POST)", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1", m)
	}
}
