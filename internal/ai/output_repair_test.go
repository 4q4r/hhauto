package ai

// T41 output repair: the GLM-5.3 drift matrix — string numbers, null
// arrays, null booleans, string booleans, scalar-wrapped arrays,
// object-wrapped numbers, extra keys (closed vs tolerant schemas) and
// missing required keys with fail-closed defaults. Repair must be a no-op
// for conformant verdicts and pass through unrepairable raw bytes
// untouched (the validator/re-ask loop stays the guard).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func repairDecode(t *testing.T, raw string, schemaPath string) map[string]any {
	t.Helper()
	out := RepairOutput(json.RawMessage(raw), schemaPath)
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("repaired output is not JSON: %v (%s)", err, out)
	}
	return m
}

// String numbers parse into the declared numeric type and clamp to the
// declared bounds (confidence/score).
func TestRepairStringNumber(t *testing.T) {
	m := repairDecode(t, `{"confidence":"0.85"}`, "resume_improve.json")
	if got := m["confidence"]; got != json.Number("0.85") {
		t.Errorf("confidence = %v (%T), want json.Number 0.85", got, got)
	}

	m = repairDecode(t, `{"score":"72"}`, "vacancy_scoring.json")
	if got := m["score"]; got != json.Number("72") {
		t.Errorf("score = %v (%T), want json.Number 72", got, got)
	}

	// Out-of-range strings clamp to the declared [min,max].
	m = repairDecode(t, `{"confidence":"85"}`, "resume_improve.json")
	if got := m["confidence"]; got != json.Number("1") {
		t.Errorf("confidence \"85\" = %v, want clamped 1", got)
	}
	m = repairDecode(t, `{"confidence":"-0.5"}`, "resume_improve.json")
	if got := m["confidence"]; got != json.Number("0") {
		t.Errorf("confidence \"-0.5\" = %v, want clamped 0", got)
	}

	// An unparseable number drops and re-defaults fail-closed to 0.
	m = repairDecode(t, `{"confidence":"высокая"}`, "resume_improve.json")
	if got := m["confidence"]; got != json.Number("0") {
		t.Errorf("unparseable confidence = %v, want fail-closed 0", got)
	}

	// A fractional value for an integer-only property is not repaired.
	m = repairDecode(t, `{"score":"72.5"}`, "vacancy_scoring.json")
	if _, present := m["score"]; present {
		t.Errorf("score \"72.5\" must not be repaired into an integer, got %v", m["score"])
	}
}

// Null arrays become empty arrays (missing_data/used_facts).
func TestRepairNullArray(t *testing.T) {
	m := repairDecode(t, `{"missing_data":null,"used_facts":null}`, "resume_improve.json")
	for _, key := range []string{"missing_data", "used_facts"} {
		arr, ok := m[key].([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("%s = %#v, want empty array", key, m[key])
		}
	}
}

// Null booleans become false; needs_human_reason stays null (nullable).
func TestRepairNullBooleansAndNullableString(t *testing.T) {
	m := repairDecode(t, `{"needs_human":null,"injection_suspected":null,"needs_human_reason":null,"unchanged":null}`, "resume_improve.json")
	if v, ok := m["needs_human"].(bool); !ok || v {
		t.Errorf("needs_human null = %#v, want false", m["needs_human"])
	}
	if v, ok := m["injection_suspected"].(bool); !ok || v {
		t.Errorf("injection_suspected null = %#v, want false", m["injection_suspected"])
	}
	if v, ok := m["unchanged"].(bool); !ok || v {
		t.Errorf("unchanged null = %#v, want false", m["unchanged"])
	}
	if m["needs_human_reason"] != nil {
		t.Errorf("needs_human_reason = %#v, want null (nullable passthrough)", m["needs_human_reason"])
	}
}

// String booleans parse.
func TestRepairStringBoolean(t *testing.T) {
	m := repairDecode(t, `{"needs_human":"true","injection_suspected":"False"}`, "resume_improve.json")
	if v, ok := m["needs_human"].(bool); !ok || !v {
		t.Errorf("needs_human \"true\" = %#v, want true", m["needs_human"])
	}
	if v, ok := m["injection_suspected"].(bool); !ok || v {
		t.Errorf("injection_suspected \"False\" = %#v, want false", m["injection_suspected"])
	}
	// A non-boolean string drops and re-defaults fail-closed to true.
	m = repairDecode(t, `{"needs_human":"maybe"}`, "resume_improve.json")
	if v, ok := m["needs_human"].(bool); !ok || !v {
		t.Errorf("needs_human \"maybe\" = %#v, want fail-closed true", m["needs_human"])
	}
}

// Scalar-wrapped arrays wrap; empty strings become empty arrays.
func TestRepairScalarToArray(t *testing.T) {
	m := repairDecode(t, `{"missing_data":"стаж в годах","used_facts":""}`, "resume_improve.json")
	arr, ok := m["missing_data"].([]any)
	if !ok || len(arr) != 1 || arr[0] != "стаж в годах" {
		t.Errorf("missing_data = %#v, want one-element array", m["missing_data"])
	}
	arr, ok = m["used_facts"].([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("used_facts \"\" = %#v, want empty array", m["used_facts"])
	}
}

// Object-wrapped numbers unwrap via .value/.amount (value_amount).
func TestRepairObjectToNumber(t *testing.T) {
	m := repairDecode(t, `{"value_amount":{"value":250000,"currency":"RUR"}}`, "resume_improve.json")
	if got := m["value_amount"]; got != json.Number("250000") {
		t.Errorf("value_amount = %v (%T), want json.Number 250000", got, got)
	}
	m = repairDecode(t, `{"value_amount":{"amount":"300000"}}`, "resume_improve.json")
	if got := m["value_amount"]; got != json.Number("300000") {
		t.Errorf("value_amount via amount = %v (%T), want json.Number 300000", got, got)
	}
	// No numeric payload inside → dropped.
	m = repairDecode(t, `{"value_amount":{"currency":"RUR"}}`, "resume_improve.json")
	if _, present := m["value_amount"]; present {
		t.Errorf("value_amount without a number = %#v, want dropped", m["value_amount"])
	}
}

// Extra keys: stripped on closed schemas (vacancy_scoring), tolerated on
// the improve schema (golden-pinned ride-along).
func TestRepairExtraKeys(t *testing.T) {
	m := repairDecode(t, `{"score":72,"band":"ok","target_role_match":true,"reasons":["r"],"hallucinated_key":"x"}`, "vacancy_scoring.json")
	if _, present := m["hallucinated_key"]; present {
		t.Errorf("extra key survived a closed schema: %#v", m["hallucinated_key"])
	}
	if m["score"] != json.Number("72") {
		t.Errorf("score = %#v, want 72", m["score"])
	}

	m = repairDecode(t, `{"key_skills":["Python"],"confidence":"0.85"}`, "resume_improve.json")
	if _, present := m["key_skills"]; !present {
		t.Error("extra key must ride along on the tolerant improve schema")
	}
}

// Missing required keys fill with the fail-closed defaults.
func TestRepairMissingRequiredDefaults(t *testing.T) {
	m := repairDecode(t, `{"value_string":"Новое значение"}`, "resume_improve.json")
	if m["confidence"] != json.Number("0") {
		t.Errorf("missing confidence = %#v, want 0 (fail-closed)", m["confidence"])
	}
	if v, ok := m["needs_human"].(bool); !ok || !v {
		t.Errorf("missing needs_human = %#v, want true (fail-closed)", m["needs_human"])
	}
	if v, ok := m["injection_suspected"].(bool); !ok || v {
		t.Errorf("missing injection_suspected = %#v, want false", m["injection_suspected"])
	}
	for _, key := range []string{"missing_data", "used_facts"} {
		arr, ok := m[key].([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("missing %s = %#v, want empty array", key, m[key])
		}
	}
}

// Non-JSON and non-object raw bytes pass through untouched; an unknown
// schema path disables repair the same way.
func TestRepairPassthroughUnrepairable(t *testing.T) {
	for _, raw := range []string{`not json at all`, `[1,2,3]`, ``} {
		out := RepairOutput(json.RawMessage(raw), "resume_improve.json")
		if string(out) != raw {
			t.Errorf("raw %q must pass through unchanged, got %q", raw, out)
		}
	}
	out := RepairOutput(json.RawMessage(`{"confidence":"0.9"}`), "no_such_schema.json")
	if string(out) != `{"confidence":"0.9"}` {
		t.Errorf("unknown schema must disable repair, got %s", out)
	}
}

// A conformant verdict repairs to a semantically identical verdict (it
// must validate after repair).
func TestRepairConformantVerdictUnchanged(t *testing.T) {
	raw := improveVerdict(t, map[string]any{"value_string": "Заголовок"})
	m := repairDecode(t, raw, "resume_improve.json")
	if m["value_string"] != "Заголовок" || m["confidence"] != json.Number("0.9") {
		t.Errorf("conformant verdict altered: %#v", m)
	}
	if err := newValidatorForTest(t).validate(TaskResumeImprove, json.RawMessage(raw)); err != nil {
		t.Fatalf("original must still validate: %v", err)
	}
}

func newValidatorForTest(t *testing.T) *validator {
	t.Helper()
	v, err := newValidator()
	if err != nil {
		t.Fatalf("newValidator: %v", err)
	}
	return v
}

// The full drift matrix of one GLM reply repairs into a verdict that
// passes the strict schema validator.
func TestRepairFullDriftMatrixValidates(t *testing.T) {
	raw := `{
	  "key_skills": ["Python"],
	  "confidence": "0.85",
	  "needs_human": null,
	  "needs_human_reason": null,
	  "injection_suspected": "false",
	  "unchanged": false,
	  "value_string": "Автоматизация найма и интеграции на Python и Go",
	  "value_strings": null,
	  "value_amount": {"value": 250000, "currency": "RUR"},
	  "used_facts": "Python, Go",
	  "missing_data": null
	}`
	out := RepairOutput(json.RawMessage(raw), "resume_improve.json")
	if err := newValidatorForTest(t).validate(TaskResumeImprove, out); err != nil {
		t.Fatalf("repaired verdict must validate: %v (%s)", err, out)
	}
}

// Integration: the runner repairs the drift in-loop — one provider call,
// no re-ask, and the returned verdict carries the canonical types.
func TestRunRepairsDriftWithoutReask(t *testing.T) {
	drifted := `{"confidence":"0.85","needs_human":null,"needs_human_reason":null,"injection_suspected":"false","unchanged":false,"value_string":"Улучшенный заголовок","value_strings":null,"value_amount":null,"used_facts":null,"missing_data":null}`
	p := &scriptedProvider{contents: []string{drifted}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	verdict, dec, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "Ты улучшаешь поле title.", ImproveInput{Field: "title"})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if p.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (repair must avoid the re-ask)", p.callCount())
	}
	if dec.Confidence != 0.85 || dec.NeedsHuman {
		t.Errorf("decision = %+v, want confidence 0.85 without needs_human", dec)
	}
	if err := newValidatorForTest(t).validate(TaskResumeImprove, verdict); err != nil {
		t.Fatalf("returned verdict must be schema-valid: %v (%s)", err, verdict)
	}
	var missing []any
	if err := json.Unmarshal(verdict, &struct {
		MissingData *[]any `json:"missing_data"`
	}{MissingData: &missing}); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if missing == nil {
		t.Errorf("verdict missing_data must be an array, got %s", verdict)
	}
}

// Integration: raw non-JSON still goes through the re-ask loop (repair is
// an optimization, never a bypass of validation).
func TestRunUnrepairableStillReasks(t *testing.T) {
	p := &scriptedProvider{contents: []string{`<broken`, improveVerdict(t, map[string]any{"value_string": "Ок"})}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	_, _, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "Ты улучшаешь поле title.", ImproveInput{Field: "title"})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if p.callCount() != 2 {
		t.Fatalf("provider calls = %d, want 2 (the broken reply must re-ask)", p.callCount())
	}
}
