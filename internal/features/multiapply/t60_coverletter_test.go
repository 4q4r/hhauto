package multiapply

// T60: the configurable cover letter. The [features.multiapply.cover_letter]
// section switches the letter pipeline (off / template / ai); whenever a
// letter is built the FULL vacancy card is fetched and its complete field
// set rides into the AI prompt (vacancy_details dump) and the template
// placeholders alike.
import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/config"
)

// richVacancyCard is the fake hh vacancyView: name, salary range, experience,
// employment, schedule, work formats, key skills, area, address+metro,
// employer with company type, contacts, response instructions, secondary
// fields — plus the heavy decorative branded_description block.
func richVacancyCard(id int64) string {
	return fmt.Sprintf(`{
		"vacancyId":%[1]d,
		"name":"Backend-разработчик (Go)",
		"description":"<p>Разработка <b>платёжного</b> бэкенда.</p><script>alert(1)</script>",
		"salary":{"from":150000,"to":250000,"currency":"RUR","gross":false},
		"experience":{"id":"between1And3","name":"От 1 года до 3 лет"},
		"employment":{"id":"full","name":"Полная занятость"},
		"schedule":{"id":"fullDay","name":"Полный день"},
		"work_formats":[{"id":"REMOTE","name":"Удалённая работа"}],
		"key_skills":[{"name":"Go"},{"name":"PostgreSQL"}],
		"area":{"name":"Таганрог"},
		"address":{"city":"Таганрог","street":"Ленина","building":"1","metro":{"name":"Центр"}},
		"employer":{"id":9001,"name":"Тест-работодатель","type":{"id":"open","name":"Прямой работодатель"},"trusted":true},
		"contacts":[{"value":"+7 000 000-00-00","comment":"HR"},{"value":"@hrbot"}],
		"response_instructions":"Пишите в чат после 18:00",
		"accept_temporary":true,
		"has_tests":true,
		"alternate_url":"https://hh.ru/vacancy/%[1]d",
		"published_at":"2026-09-20T10:00:00+0300",
		"branded_description":{"template_id":1,"html":"<div>ОГРОМНЫЙ ФИРМЕННЫЙ HTML</div>"},
		"archived":false,
		"allow_messages":true
	}`, id)
}

func coverLetterSection(c *config.Config, mut func(*config.MultiApplyCoverLetter)) {
	cl := &c.Features.MultiApply.CoverLetter
	cl.Enabled = true
	if mut != nil {
		mut(cl)
	}
}

// lastLetterInput returns the last cover_letter task input as a generic map.
func lastLetterInput(t *testing.T, h *harness) map[string]any {
	t.Helper()
	last := ""
	for i, task := range h.runner.calls {
		if task == ai.TaskCoverLetter {
			last = h.runner.inputs[i]
		}
	}
	if last == "" {
		t.Fatalf("no cover_letter call in %v", h.runner.calls)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(last), &m); err != nil {
		t.Fatalf("letter input decode: %v", err)
	}
	return m
}

func TestCoverLetterOffModeSkipsLetters(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		coverLetterSection(c, func(cl *config.MultiApplyCoverLetter) { cl.Mode = config.CoverLetterModeOff })
	})
	twoVacancies(h)
	h.popupByVac[101] = popupWith(true, false, "ENABLED") // hh demands a letter
	h.popupByVac[102] = popupWith(true, false, "ENABLED")
	f := h.feature(t)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if got := h.applyCalls; len(got) != 0 {
		t.Fatalf("applyCalls = %v, want none (mode=off never generates letters)", got)
	}
	for _, task := range h.runner.calls {
		if task == ai.TaskCoverLetter {
			t.Fatalf("cover_letter called in off mode: %v", h.runner.calls)
		}
	}
	if m := metricsOf(t, h.db); m["apply.skip.letter_off"] != 2 {
		t.Fatalf("apply.skip.letter_off = %v, want 2", m["apply.skip.letter_off"])
	}
}

func TestCoverLetterTemplateModeNoAI(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		coverLetterSection(c, func(cl *config.MultiApplyCoverLetter) {
			cl.Mode = config.CoverLetterModeTemplate
			cl.Template = "Откликаюсь на вакансию {{.VacancyTitle}} ({{.EmployerName}}), навыки: {{.VacancyKeySkills}}."
		})
	})
	twoVacancies(h)
	h.popupByVac[101] = popupWith(true, false, "ENABLED")
	h.popupByVac[102] = popupWith(true, false, "ENABLED")
	f := h.feature(t)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if got := h.applyCalls; len(got) != 2 {
		t.Fatalf("applyCalls = %v, want [101 102]", got)
	}
	for i, b := range h.applyBodies {
		// The rendered letter carries the card data, URL-escaped in the form.
		if !strings.Contains(b, "%D0%9E%D1%82%D0%BA%D0%BB%D0%B8%D0%BA%D0%B0%D1%8E%D1%81%D1%8C") {
			t.Fatalf("apply body %d misses the rendered template: %q", i, b)
		}
		if !strings.Contains(b, "Backend-%D1%80%D0%B0%D0%B7%D1%80%D0%B0%D0%B1%D0%BE%D1%82%D1%87%D0%B8%D0%BA") {
			t.Fatalf("apply body %d misses the card vacancy title: %q", i, b)
		}
	}
	for _, task := range h.runner.calls {
		if task == ai.TaskCoverLetter {
			t.Fatalf("template mode must not call the AI, calls=%v", h.runner.calls)
		}
	}
}

func TestCoverLetterAIFullVacancyContext(t *testing.T) {
	// Golden: the letter prompt carries the complete vacancy field set —
	// name, salary range, experience, employment, schedule, work formats,
	// key skills, area, address+metro, employer type, contacts, description
	// as text, secondary fields — and never the raw HTML or the dropped
	// heavy blocks.
	h := newHarness(t, func(c *config.Config) {
		coverLetterSection(c, nil) // mode "" behaves as ai
	})
	twoVacancies(h)
	h.popupByVac[101] = popupWith(true, false, "ENABLED")
	h.popupByVac[102] = popupWith(true, false, "ENABLED")
	f := h.feature(t)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if got := h.cardReqs[101]; got != 1 {
		t.Fatalf("vacancy card fetched %d times, want 1", got)
	}
	m := lastLetterInput(t, h)

	details, ok := m["vacancy_details"].(map[string]any)
	if !ok {
		t.Fatalf("letter input misses vacancy_details: %v", m)
	}
	for field, want := range map[string]string{
		"title":         "Backend-разработчик (Go)",
		"experience":    "От 1 года до 3 лет",
		"employment":    "Полная занятость",
		"schedule":      "Полный день",
		"work_formats":  "Удалённая работа",
		"key_skills":    "Go, PostgreSQL",
		"area":          "Таганрог",
		"employer_name": "Тест-работодатель",
		"employer_type": "Прямой работодатель",
	} {
		if got, _ := details[field].(string); got != want {
			t.Errorf("vacancy_details.%s = %q, want %q", field, got, want)
		}
	}
	for _, tc := range []struct{ field, want string }{
		{"salary", "150000"},
		{"salary", "250000"},
		{"address", "Центр"},
		{"contacts", "+7 000 000-00-00"},
		{"contacts", "@hrbot"},
		{"description", "платёжного"},
	} {
		got, _ := details[tc.field].(string)
		if !strings.Contains(got, tc.want) {
			t.Errorf("vacancy_details.%s = %q, want containing %q", tc.field, got, tc.want)
		}
	}
	desc, _ := details["description"].(string)
	for _, banned := range []string{"<p>", "<script>", "alert(1)"} {
		if strings.Contains(desc, banned) {
			t.Errorf("description keeps HTML %q: %q", banned, desc)
		}
	}
	other, ok := details["other"].(map[string]any)
	if !ok {
		t.Fatalf("vacancy_details misses the other-fields bucket: %v", details)
	}
	if ri, _ := other["response_instructions"].(string); ri != "Пишите в чат после 18:00" {
		t.Errorf("other.response_instructions = %q", ri)
	}
	if _, dup := other["description"]; dup {
		t.Error("other duplicates the description (it rides as text already)")
	}
	if _, dup := other["salary"]; dup {
		t.Error("other duplicates the named salary field")
	}
	for _, heavy := range []string{"branded_description"} {
		if _, found := other[heavy]; found {
			t.Errorf("other keeps the heavy field %q", heavy)
		}
	}

	// The vacancy summary block stays slim: no raw JSON, no raw HTML.
	vac, ok := m["vacancy"].(map[string]any)
	if !ok {
		t.Fatalf("letter input misses the vacancy block: %v", m)
	}
	if _, has := vac["raw"]; has {
		t.Error("letter input vacancy block carries the raw JSON")
	}
	if d, _ := vac["description"].(string); strings.Contains(d, "<") {
		t.Error("letter input vacancy block carries raw HTML description")
	}
}

func TestCoverLetterCardFailureSkipsVacancy(t *testing.T) {
	// Fail-closed: a letter cannot be built without the full vacancy, so
	// the letter-required vacancy is skipped (never applied letterless).
	h := newHarness(t, func(c *config.Config) {
		coverLetterSection(c, nil)
	})
	twoVacancies(h)
	h.popupByVac[101] = popupWith(true, false, "ENABLED")
	h.popupByVac[102] = popupWith(true, false, "ENABLED")
	h.cardErr[101] = 500
	f := h.feature(t)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if got := h.applyCalls; len(got) != 1 || got[0] != 102 {
		t.Fatalf("applyCalls = %v, want [102] (101 skipped on card failure)", got)
	}
	if m := metricsOf(t, h.db); m["apply.skip.vacancy_card_failed"] != 1 {
		t.Fatalf("apply.skip.vacancy_card_failed = %v, want 1", m["apply.skip.vacancy_card_failed"])
	}
}

func TestVacancyDetailsHTMLToText(t *testing.T) {
	got := htmlToText("<p>Первый абзац.</p>\n<br>Второй <b>с жирным</b> &amp; символом.<script>bad()</script>")
	for _, banned := range []string{"<", ">", "script", "bad()"} {
		if strings.Contains(got, banned) {
			t.Errorf("htmlToText keeps %q: %q", banned, got)
		}
	}
	for _, want := range []string{"Первый абзац.", "Второй", "жирным", "& символом."} {
		if !strings.Contains(got, want) {
			t.Errorf("htmlToText = %q, want containing %q", got, want)
		}
	}
}
