package hhapi

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// T80 wire contract (donor-derived): the response page carries the
// vacancyTests state; the apply POST submits the test fields on the same
// /applicant/vacancy_response/popup endpoint.

const vacancyTestsPage = `<html><head><title>Отклик</title></head><body>
<script>window.__INITIAL_STATE__={"vacancyTests":{"777":{"uidPk":"uid-777","guid":"guid-777","startTime":1727700000,"required":true,"tasks":[
{"id":"t1","description":"Готовы ли вы работать с Go?","candidateSolutions":[{"id":101,"text":"Да"},{"id":102,"text":"Нет"}]},
{"id":"t2","description":"Опыт с Docker?","candidateSolutions":[]}]}}}</script></body></html>`

func TestVacancyTestsParse(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/applicant/vacancy_response", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vacancyId") != "777" || r.URL.Query().Get("hhtmFrom") != "vacancy" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, vacancyTestsPage)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	vt, err := c.VacancyTests(ctx, 777)
	if err != nil {
		t.Fatalf("VacancyTests: %v", err)
	}
	if vt.UIDPk != "uid-777" || vt.GUID != "guid-777" || vt.StartTime != "1727700000" || !vt.Required {
		t.Fatalf("coordinates = %+v (number literals must render verbatim)", vt)
	}
	if len(vt.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want 2", vt.Tasks)
	}
	t1 := vt.Tasks[0]
	if t1.ID != "t1" || len(t1.Solutions) != 2 || t1.Solutions[0].ID != "101" || t1.Solutions[0].Text != "Да" {
		t.Fatalf("t1 = %+v", t1)
	}
	if t2 := vt.Tasks[1]; t2.Question != "Опыт с Docker?" || len(t2.Solutions) != 0 {
		t.Fatalf("t2 = %+v (open task: no solutions)", t2)
	}
}

// T83 live form (2026-10-03 capture): the real response page embeds the
// whole state HTML-escaped — every quote is &#34;, so the literal-key
// search misses it and the vacancy slipped to a 400 test-required apply.
// Fixture anonymized: ids → 777/uid-777, texts → neutral wording.
const vacancyTestsPageEscaped = `<html><head><title>Отклик</title></head><body>
<script type="text/json" id="HH-Lux-InitialState">{&#34;vacancyTests&#34;:{&#34;777&#34;:{&#34;uidPk&#34;:&#34;uid-777&#34;,&#34;guid&#34;:&#34;guid-777&#34;,&#34;startTime&#34;:&#34;1791000000&#34;,&#34;required&#34;:true,&#34;name&#34;:&#34;Вопросы к соискателю&#34;,&#34;description&#34;:&#34;Просим ответить на вопросы:&#34;,&#34;tasks&#34;:[{&#34;id&#34;:&#34;t1&#34;,&#34;description&#34;:&#34;Опишите ваш опыт.&#34;,&#34;open&#34;:true,&#34;multiple&#34;:false,&#34;candidateSolutions&#34;:[]},{&#34;id&#34;:&#34;t2&#34;,&#34;description&#34;:&#34;Перечислите знакомые вам технологии.&#34;,&#34;open&#34;:true,&#34;multiple&#34;:false,&#34;candidateSolutions&#34;:[]}]}}}</script>
</body></html>`

func TestVacancyTestsEscapedState(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/applicant/vacancy_response", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("vacancyId") != "777" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, vacancyTestsPageEscaped)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	vt, err := c.VacancyTests(ctx, 777)
	if err != nil {
		t.Fatalf("VacancyTests: %v (the escaped live form must parse)", err)
	}
	if vt.UIDPk != "uid-777" || vt.GUID != "guid-777" || vt.StartTime != "1791000000" || !vt.Required {
		t.Fatalf("coordinates = %+v", vt)
	}
	if len(vt.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want 2", vt.Tasks)
	}
	if vt.Tasks[0].ID != "t1" || len(vt.Tasks[0].Solutions) != 0 {
		t.Fatalf("t1 = %+v, want an open task", vt.Tasks[0])
	}
}

func TestVacancyTestsMissingState(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/applicant/vacancy_response", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><body>обычная страница без состояния</body></html>`)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	if _, err := c.VacancyTests(ctx, 777); !errors.Is(err, ErrVacancyTestUnavailable) {
		t.Fatalf("err = %v, want ErrVacancyTestUnavailable", err)
	}
}

func TestVacancyTestsCaptcha(t *testing.T) {
	env := newTestEnv(t)
	env.serve(env.mux1, "1", "GET", "/applicant/vacancy_response", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><body>showcaptcha</body></html>`)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	_, err := c.VacancyTests(ctx, 777)
	if !errors.Is(err, ErrCaptcha) {
		t.Fatalf("err = %v, want ErrCaptcha", err)
	}
}

func TestApplyCarriesTestFields(t *testing.T) {
	env := newTestEnv(t)
	var hhtmFrom, hhtmSource string
	env.serve(env.mux1, "1", "POST", "/applicant/vacancy_response/popup", func(w http.ResponseWriter, r *http.Request) {
		hhtmFrom = r.Header.Get("X-Hhtmfrom")
		hhtmSource = r.Header.Get("X-Hhtmsource")
		_, _ = io.WriteString(w, `{"success":true}`)
	})
	c := env.client()
	ctx, cancel := ctxb(t)
	defer cancel()
	ref, err := c.Apply(ctx, ApplyRequest{
		VacancyID: 777, ResumeHash: "hash_1",
		Test: &ApplyTest{
			UIDPk: "uid-777", GUID: "guid-777", StartTime: "1727700000", Required: true,
			Answers: []TestAnswer{
				{TaskID: "t1", SolutionID: "101"},
				{TaskID: "t2", Text: "Разворачивал сервисы в Docker в проде."},
			},
		},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ref.Vacancy != 777 {
		t.Fatalf("ref = %+v", ref)
	}
	body := env.lastBody("1:POST /applicant/vacancy_response/popup")
	for _, want := range []string{
		"resume_hash=hash_1", "vacancy_id=777",
		"uidPk=uid-777", "guid=guid-777", "startTime=1727700000",
		"testRequired=true", "ignore_postponed=true", "incomplete=false",
		"mark_applicant_visible_in_vacancy_country=false",
		"country_ids=%5B%5D", "lux=true", "withoutTest=no",
		"task_t1=101", "task_t2_text=",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("apply body %q misses %q", body, want)
		}
	}
	if hhtmFrom != "vacancy" || hhtmSource != "vacancy_response" {
		t.Fatalf("hhtm headers = %q/%q, want vacancy/vacancy_response", hhtmFrom, hhtmSource)
	}
}
