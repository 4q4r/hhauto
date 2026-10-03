package multiapply

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"hhauto/internal/config"
)

// T83: for test-gated vacancies hh answers the apply popup with a refusal
// envelope ({"type":"test-required","redirect_uri":...}) instead of the
// responseStatus modal, and embeds the test state HTML-escaped on the
// response page. The T77 gate read HasTests=false off the refusal and the
// live apply died with 400 {"error":"test-required"}. The gate must treat
// the refusal as the test flag; the T80 solver then does its job. Fixtures
// anonymized from the 2026-10-03 live capture (ids → 777*, texts neutral).

func testPopupTestRequiredRefusal() string {
	return `{"type":"test-required","redirect_uri":"/applicant/vacancy_response?vacancyId=777105&startedWithQuestion=false"}`
}

// testEscapedPageFixture renders the live response-page form: the state is
// HTML-escaped, two open tasks.
func testEscapedPageFixture(vacancy int64) string {
	return fmt.Sprintf(`<html><head><title>Отклик</title></head><body>
<script type="text/json" id="HH-Lux-InitialState">{&#34;vacancyTests&#34;:{&#34;%[1]d&#34;:{&#34;uidPk&#34;:&#34;uid-%[1]d&#34;,&#34;guid&#34;:&#34;guid-%[1]d&#34;,&#34;startTime&#34;:&#34;1791000000&#34;,&#34;required&#34;:true,&#34;name&#34;:&#34;Вопросы к соискателю&#34;,&#34;description&#34;:&#34;Просим ответить на вопросы:&#34;,&#34;tasks&#34;:[{&#34;id&#34;:&#34;t1&#34;,&#34;description&#34;:&#34;Опишите ваш опыт.&#34;,&#34;open&#34;:true,&#34;multiple&#34;:false,&#34;candidateSolutions&#34;:[]},{&#34;id&#34;:&#34;t2&#34;,&#34;description&#34;:&#34;Перечислите знакомые вам технологии.&#34;,&#34;open&#34;:true,&#34;multiple&#34;:false,&#34;candidateSolutions&#34;:[]}]}}}</script>
</body></html>`, vacancy)
}

// testOpenVerdict answers both open tasks of testEscapedPageFixture from
// facts (the live form: open questions only).
var testOpenVerdict = `{"answers":[
{"question_id":"1","kind":"open","choice_indices":null,"open_text":"Опыт описан в резюме.","based_on_profile":true,"facts_used":[],"note":null},
{"question_id":"2","kind":"open","choice_indices":null,"open_text":"Go, Python, Docker.","based_on_profile":true,"facts_used":[],"note":null}],
"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false}`

// TestPopupRefusalSolvesTest: the test-required refusal envelope routes the
// vacancy into the T80 solve flow; the apply POST carries the answers.
func TestPopupRefusalSolvesTest(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777105, testPopupTestRequiredRefusal())
	h.testsByVac[777105] = testEscapedPageFixture(777105)
	h.runner.testAnswers = testOpenVerdict

	h.run(t)

	if h.testsReqs[777105] != 1 {
		t.Fatalf("vacancy_response requests = %d, want 1 (the refusal must fetch the test)", h.testsReqs[777105])
	}
	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777105 {
		t.Fatalf("applies = %v, want [777105] (solved test completes the apply)", h.applyCalls)
	}
	for _, want := range []string{"uidPk=uid-777105", "guid=guid-777105", "startTime=1791000000",
		"testRequired=true", "task_t1_text=", "task_t2_text="} {
		if !strings.Contains(h.applyBodies[0], want) {
			t.Fatalf("apply body %q misses %q", h.applyBodies[0], want)
		}
	}
	if m := mustMetrics(t, h.db); m["apply.test_solved"] != 1 {
		t.Fatalf("apply.test_solved = %d, want 1 (metrics %v)", m["apply.test_solved"], m)
	}
}

// TestPopupRefusalUnreadableTestSkips: the refusal with an unreadable test
// page falls back to the T77 skip — the apply never fires.
func TestPopupRefusalUnreadableTestSkips(t *testing.T) {
	h := newHarness(t, nil)
	testVacancyWithPopup(t, h, 777106, testPopupTestRequiredRefusal())
	h.testsByVac[777106] = `<html><body>страница без состояния</body></html>`

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (unreadable test = fallback skip)", h.applyCalls)
	}
	if m := mustMetrics(t, h.db)["apply.skip.test_required"]; m != 1 {
		t.Fatalf("apply.skip.test_required = %d, want 1", m)
	}
}

// TestApplyTestRequired400IsTerminalSkip (T83 last line): when the POST is
// still refused with test-required (detection missed), the vacancy settles
// as the T77 terminal skip — never the ✗ console error line, never an
// apply.error metric.
func TestApplyTestRequired400IsTerminalSkip(t *testing.T) {
	h := newHarness(t, nil)
	buf := h.withConsole(t, func(c *config.Console) { c.Skips = "all" })
	h.searchItems = []string{vacItem(777107, "QA Engineer", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[777107] = popupWith(false, false, "ENABLED") // popup: no tests — hh disagrees at POST time
	h.applyErr = func(_ int, _ int64, _ string) error {
		return &respErr{status: http.StatusBadRequest, body: `{"error":"test-required"}`}
	}

	h.run(t)

	if len(h.applyCalls) != 1 {
		t.Fatalf("applies = %v, want exactly 1 (the POST happened)", h.applyCalls)
	}
	if out := buf.String(); strings.Contains(out, "✗ Ошибка отклика") {
		t.Fatalf("test-required must not render the error line: %q", out)
	}
	if m := mustMetrics(t, h.db); m["apply.error"] != 0 || m["apply.skip.test_required"] != 1 {
		t.Fatalf("metrics = %v, want apply.error=0 and apply.skip.test_required=1", m)
	}
}
