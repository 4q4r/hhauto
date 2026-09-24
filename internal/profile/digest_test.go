package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureRaw(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "resume_sample.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return json.RawMessage(b)
}

func assertContains(t *testing.T, got string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(got, sub) {
			t.Errorf("text does not contain %q\ntext:\n%s", sub, got)
		}
	}
}

func assertNotContains(t *testing.T, got string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(got, sub) {
			t.Errorf("text unexpectedly contains %q\ntext:\n%s", sub, got)
		}
	}
}

func TestBuildResumeDigestFromFixture(t *testing.T) {
	d, err := BuildResumeDigest(fixtureRaw(t))
	if err != nil {
		t.Fatalf("BuildResumeDigest: %v", err)
	}
	assertContains(t, d,
		"Резюме «Senior Go-разработчик» (опубликовано)",
		"Регион: Москва",
		"Зарплата: 350000 RUR",
		"Общий опыт: 7 лет",
		"Занятость: Полная занятость, Удаленная работа",
		"График: Полный день, Гибкий график",
		"Релокация: не готов к переезду (Санкт-Петербург)",
		"Командировки: Редкие командировки",
		"Ключевые навыки: Go, PostgreSQL, Kafka, Docker, Kubernetes",
		"О себе: Опыт highload, Kafka, Kubernetes.",
		"Языки: Русский — родной; Английский — B2 — Средне-продвинутый",
		"- Ведущий Go-разработчик, ООО «Ромашка», Москва, 2021-03 — по настоящее время",
		"Разработка биллинг-сервиса на Go и PostgreSQL",
		"- Backend-разработчик, АО «Василёк», Москва, 2018-06 — 2021-02",
	)
}

func TestBuildResumeDigestDeterministic(t *testing.T) {
	raw := fixtureRaw(t)
	d1, err := BuildResumeDigest(raw)
	if err != nil {
		t.Fatalf("first digest: %v", err)
	}
	d2, err := BuildResumeDigest(raw)
	if err != nil {
		t.Fatalf("second digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("digest is not deterministic:\n---\n%s\n---\n%s", d1, d2)
	}
}

func TestBuildResumeDigestMinimal(t *testing.T) {
	// resumes/mine list-item shape: title/status only, no experience blocks.
	raw := json.RawMessage(`{"title":"Тестировщик","status":{"id":"published","name":"опубликовано"}}`)
	d, err := BuildResumeDigest(raw)
	if err != nil {
		t.Fatalf("BuildResumeDigest: %v", err)
	}
	assertContains(t, d, "Резюме «Тестировщик» (опубликовано)")
	assertNotContains(t, d, "Зарплата", "Опыт работы:", "Ключевые навыки")
}

func TestBuildResumeDigestAliasesAndSingulars(t *testing.T) {
	raw := json.RawMessage(`{
		"title": "Dev",
		"key_skills": [{"name": "Go"}, {"name": "SQL"}],
		"languages": [{"name": "English", "level": {"name": "B2"}}],
		"employment": {"id": "full", "name": "Полная занятость"},
		"schedule": {"id": "fullDay", "name": "Полный день"}
	}`)
	d, err := BuildResumeDigest(raw)
	if err != nil {
		t.Fatalf("BuildResumeDigest: %v", err)
	}
	assertContains(t, d,
		"Ключевые навыки: Go, SQL",
		"Языки: English — B2",
		"Занятость: Полная занятость",
		"График: Полный день",
	)
}

func TestBuildResumeDigestEmptyInput(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("   \n")} {
		if _, err := BuildResumeDigest(raw); err == nil {
			t.Errorf("BuildResumeDigest(%q) = nil error, want error", string(raw))
		}
	}
}

func TestBuildResumeDigestMalformedAndNonObject(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"invalid": json.RawMessage(`{"title": `),
		"array":   json.RawMessage(`[1,2,3]`),
		"string":  json.RawMessage(`"just a string"`),
	} {
		if _, err := BuildResumeDigest(raw); err == nil {
			t.Errorf("%s: BuildResumeDigest = nil error, want error", name)
		}
	}
}

func TestBuildResumeDigestEmptyObject(t *testing.T) {
	d, err := BuildResumeDigest(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("BuildResumeDigest({}): %v", err)
	}
	if d != "" {
		t.Fatalf("digest of empty object = %q, want empty", d)
	}
}

func TestBuildResumeDigestClipsLongText(t *testing.T) {
	long := strings.Repeat("слово ", 100) // 600 runes
	raw := json.RawMessage(`{
		"title": "Clip",
		"skills": "` + strings.TrimSuffix(long, " ") + `",
		"experience": [{
			"position": "P",
			"company": "C",
			"start": "2020-01-01",
			"end": null,
			"description": "` + strings.TrimSuffix(long, " ") + `"
		}]
	}`)
	d, err := BuildResumeDigest(raw)
	if err != nil {
		t.Fatalf("BuildResumeDigest: %v", err)
	}
	assertContains(t, d, strings.TrimSuffix(long, " ")[:290]) // head survives
	if tail := strings.TrimSuffix(long, " ")[540:]; strings.Contains(d, tail) {
		t.Errorf("long text was not clipped; digest contains its tail:\n%s", d)
	}
	assertContains(t, d, "…")
}

func TestFormatMonths(t *testing.T) {
	cases := map[int]string{
		84: "7 лет",
		7:  "7 мес.",
		14: "1 год 2 мес.",
		21: "1 год 9 мес.",
		11: "11 мес.",
		1:  "1 мес.",
		2:  "2 мес.",
		5:  "5 мес.",
		12: "1 год",
		23: "1 год 11 мес.",
	}
	for months, want := range cases {
		if got := formatMonths(months); got != want {
			t.Errorf("formatMonths(%d) = %q, want %q", months, got, want)
		}
	}
	if got := formatMonths(0); got != "" {
		t.Errorf("formatMonths(0) = %q, want empty", got)
	}
}
