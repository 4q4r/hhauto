package autoclone

// T29 field-model tests: the enum covers every mutable resume field, each
// spec maps to its update-payload side/key, and the validators enforce the
// hh contract (dates end≥start without overlaps, salary int≥0, string
// limits from the live conditions).
import (
	"encoding/json"
	"strings"
	"testing"

	"hhauto/internal/hhapi"
)

// The full mutable-field vocabulary: every id must resolve to a spec and
// every spec must carry side+key and a kind.
func TestFieldVocabularyComplete(t *testing.T) {
	for _, id := range AllFieldIDs() {
		spec, ok := Spec(id)
		if !ok {
			t.Fatalf("field %q has no spec", id)
		}
		if spec.Side != "resume" && spec.Side != "profile" {
			t.Fatalf("field %q: side %q", id, spec.Side)
		}
		if spec.Key == "" {
			t.Fatalf("field %q: empty key", id)
		}
		if spec.Kind == "" {
			t.Fatalf("field %q: empty kind", id)
		}
	}
	if len(AllFieldIDs()) < 20 {
		t.Fatalf("vocabulary too small (%d): the model must cover all mutable fields", len(AllFieldIDs()))
	}
}

// The AI-improvable subset and the not-improvable structured fields.
func TestAIPromptableFields(t *testing.T) {
	for _, id := range []string{"title", "about", "key_skills", "salary", "experience_description"} {
		spec, ok := Spec(id)
		if !ok || spec.Prompt == "" {
			t.Fatalf("field %q must be AI-improvable with a builtin prompt", id)
		}
	}
	// Dates are promptable ONLY through an explicit owner override.
	spec, ok := Spec("experience_dates")
	if !ok || spec.Prompt != "" {
		t.Fatal("experience_dates must have no builtin prompt (dates are not an AI field by default)")
	}
	// Dictionary/structured fields are never AI-improvable.
	for _, id := range []string{"professional_roles", "citizenship", "gender", "area", "language", "education_primary"} {
		spec, ok := Spec(id)
		if !ok {
			t.Fatalf("field %q missing from the model", id)
		}
		if spec.Prompt != "" {
			t.Fatalf("structured field %q must not carry an AI prompt", id)
		}
	}
}

// valueAt unwraps the condition format for assertions.
func condStringsOf(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	if raw == nil {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("decode condition value: %v", err)
	}
	var out []string
	for _, p := range parts {
		var s string
		if err := json.Unmarshal(p["string"], &s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

func baseContent() *hhapi.ResumeContent {
	return &hhapi.ResumeContent{
		Resume: map[string]json.RawMessage{
			"title":     json.RawMessage(`[{"string":"DevOps-инженер"}]`),
			"skills":    json.RawMessage(`[{"string":"Автоматизация"}]`),
			"keySkills": json.RawMessage(`[{"string":"Linux"},{"string":"Docker"}]`),
			"salary":    json.RawMessage(`[{"amount":0,"currency":"RUR"}]`),
		},
		Profile: map[string]json.RawMessage{
			"experience": json.RawMessage(`[
				{"companyName":"Альфа","position":"Инженер","description":"Занимался CI","startDate":"2020-01-01","endDate":"2022-05-01"},
				{"companyName":"Бета","position":"Старший инженер","description":"Занимался CI/CD","startDate":"2022-06-01","endDate":null}
			]`),
		},
		Screens:    []string{"title", "key_skills", "about", "experience"},
		Conditions: map[string]json.RawMessage{},
	}
}

// Text normalization: trims, enforces the live condition limits with the
// static title fallback.
func TestValidateText(t *testing.T) {
	c := baseContent()
	spec, _ := Spec("title")

	got, err := ValidateValue(spec, c, "  Новый заголовок  ")
	if err != nil {
		t.Fatalf("ValidateValue: %v", err)
	}
	if s := condStringsOf(t, got); len(s) != 1 || s[0] != "Новый заголовок" {
		t.Fatalf("title value = %s", got)
	}

	// Static title fallback: minlength 2, maxlength 100 (hh's own limits).
	if _, err := ValidateValue(spec, c, "А"); err == nil {
		t.Fatal("1-char title must be rejected")
	}
	long := strings.Repeat("ы", 101)
	if _, err := ValidateValue(spec, c, long); err == nil {
		t.Fatal("101-char title must be rejected")
	}

	// Live conditions override the static fallback: a 150-char title is
	// fine when hh says maxlength 150.
	c.Conditions["title"] = json.RawMessage(`{"parts":[{"string":{"minlength":2,"maxlength":150}}]}`)
	if _, err := ValidateValue(spec, c, strings.Repeat("ы", 150)); err != nil {
		t.Fatalf("150-char title under maxlength 150: %v", err)
	}
}

func TestValidateKeySkillsList(t *testing.T) {
	c := baseContent()
	spec, _ := Spec("key_skills")
	got, err := ValidateValue(spec, c, []string{"Linux", " Docker ", "", "Linux", "K8s"})
	if err != nil {
		t.Fatalf("ValidateValue: %v", err)
	}
	if s := condStringsOf(t, got); len(s) != 3 || s[0] != "Linux" || s[2] != "K8s" {
		t.Fatalf("keySkills = %v (trim + dedupe + drop-empty expected)", s)
	}
	// maxcount from live conditions.
	c.Conditions["keySkills"] = json.RawMessage(`{"parts":[{"string":{"maxcount":2}}]}`)
	if _, err := ValidateValue(spec, c, []string{"a", "b", "c"}); err == nil {
		t.Fatal("3 skills under maxcount 2 must be rejected")
	}
}

func TestValidateSalary(t *testing.T) {
	c := baseContent()
	spec, _ := Spec("salary")

	got, err := ValidateValue(spec, c, 180000)
	if err != nil {
		t.Fatalf("ValidateValue: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(got, &arr); err != nil || len(arr) != 1 {
		t.Fatalf("salary value = %s", got)
	}
	if arr[0]["amount"].(float64) != 180000 || arr[0]["currency"] != "RUR" {
		t.Fatalf("salary = %s (currency must ride from the current value)", got)
	}
	if _, err := ValidateValue(spec, c, -5); err == nil {
		t.Fatal("negative salary must be rejected")
	}
	if _, err := ValidateValue(spec, c, 1.5); err == nil {
		t.Fatal("fractional salary must be rejected")
	}
	// Currency: a non-default currency rides from the current value.
	c.Resume["salary"] = json.RawMessage(`[{"amount":100000,"currency":"USD"}]`)
	got, err = ValidateValue(spec, c, 2000)
	if err != nil {
		t.Fatalf("ValidateValue USD: %v", err)
	}
	if err := json.Unmarshal(got, &arr); err != nil || arr[0]["currency"] != "USD" {
		t.Fatalf("salary currency = %s", got)
	}
}

func TestValidateExperienceDates(t *testing.T) {
	c := baseContent()
	spec, _ := Spec("experience_dates")

	// end < start rejected.
	bad := map[string]*string{"startDate": strp("2021-01-01"), "endDate": strp("2020-01-01")}
	if _, err := ValidateValue(spec, c, bad); err == nil {
		t.Fatal("end < start must be rejected")
	}
	// Overlap with another item rejected (item 1 ends 2022-05-01).
	overlap := map[string]*string{"startDate": strp("2022-01-01"), "endDate": strp("2023-01-01")}
	if _, err := ValidateValue(spec, c, overlap); err == nil {
		t.Fatal("overlapping ranges must be rejected")
	}
	// Valid replacement for item 0 (its own range shifted; excluded from
	// the overlap check).
	ok := map[string]*string{"startDate": strp("2020-02-01"), "endDate": strp("2022-04-01")}
	got, err := ValidateValueAt(spec, c, 0, ok)
	if err != nil {
		t.Fatalf("valid dates: %v", err)
	}
	var m map[string]*string
	if err := json.Unmarshal(got, &m); err != nil || m["startDate"] == nil {
		t.Fatalf("dates value = %s", got)
	}
	// Bad format rejected.
	if _, err := ValidateValue(spec, c, map[string]*string{"startDate": strp("01.2020"), "endDate": nil}); err == nil {
		t.Fatal("bad date format must be rejected")
	}
}

func TestValidateStructuredPassthrough(t *testing.T) {
	c := baseContent()
	spec, _ := Spec("professional_roles")
	got, err := ValidateValue(spec, c, []int{160, 96})
	if err != nil {
		t.Fatalf("structured list: %v", err)
	}
	var ids []float64
	if err := json.Unmarshal(got, &ids); err != nil || len(ids) != 2 {
		t.Fatalf("roles value = %s", got)
	}
	if _, err := ValidateValue(spec, c, "не-массив"); err == nil {
		t.Fatal("non-array structured value must be rejected")
	}
}

func strp(s string) *string { return &s }

// Screen resolution: candidate screens intersected with the resume's
// actual screens; unknown → all screens (the SPA posts the whole form from
// every screen, so any real screen persists the payload).
func TestScreensFor(t *testing.T) {
	c := baseContent()
	if got := screensFor("title", c); len(got) != 1 || got[0] != "title" {
		t.Fatalf("title screens = %v", got)
	}
	if got := screensFor("experience_dates", c); len(got) != 1 || got[0] != "experience" {
		t.Fatalf("experience screens = %v", got)
	}
	// No intersection → all screens.
	if got := screensFor("citizenship", c); len(got) != len(c.Screens) {
		t.Fatalf("fallback screens = %v, want all %v", got, c.Screens)
	}
}
