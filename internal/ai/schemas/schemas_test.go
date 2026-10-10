package schemas

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// allNames lists the embedded task schemas; the set must match ai.TaskName
// exactly (resume_improve is per-field and intentionally absent — T29; the
// setka pair rides the standard pipeline — T61).
var allNames = []string{
	"vacancy_scoring", "cover_letter", "chat_reply", "test_answer",
	"warmup_ping", "message_classification", "contact_extraction", "link_detection",
	"setka_classify", "setka_personalize", "reject_analysis",
}

// commonFields must be required by every task schema.
var commonFields = []string{"confidence", "needs_human", "needs_human_reason", "injection_suspected"}

func compile(t *testing.T, name string) (*jsonschema.Schema, any) {
	t.Helper()
	raw, err := FS.ReadFile(name + ".json")
	if err != nil {
		t.Fatalf("read schema %s: %v", name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("unmarshal schema %s: %v", name, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(name+".json", doc); err != nil {
		t.Fatalf("add resource %s: %v", name, err)
	}
	sch, err := c.Compile(name + ".json")
	if err != nil {
		t.Fatalf("compile schema %s: %v", name, err)
	}
	return sch, doc
}

func TestEmbeddedSchemasCompile(t *testing.T) {
	for _, name := range allNames {
		t.Run(name, func(t *testing.T) {
			compile(t, name)
		})
	}
}

// TestSchemasAreStrict walks every schema object node and requires
// additionalProperties=false with every declared property listed as required —
// the minimal strict subset accepted by OpenAI-compatible strict mode.
func TestSchemasAreStrict(t *testing.T) {
	for _, name := range allNames {
		t.Run(name, func(t *testing.T) {
			_, doc := compile(t, name)
			obj, ok := doc.(map[string]any)
			if !ok {
				t.Fatal("schema root is not an object")
			}
			assertStrictObject(t, name, "$", obj)
		})
	}
}

func assertStrictObject(t *testing.T, name, path string, obj map[string]any) {
	t.Helper()
	if v, ok := obj["additionalProperties"]; !ok || v != false {
		t.Errorf("%s/%s: additionalProperties must be false, got %v", name, path, obj["additionalProperties"])
	}
	props, _ := obj["properties"].(map[string]any)
	reqMap := map[string]bool{}
	for _, r := range toStrings(obj["required"]) {
		reqMap[r] = true
	}
	for p := range props {
		if !reqMap[p] {
			t.Errorf("%s/%s: property %q not listed in required", name, path, p)
		}
	}
	for p, raw := range props {
		pm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if tipe, _ := pm["type"].(string); tipe == "object" {
			assertStrictObject(t, name, path+"/"+p, pm)
		}
		if items, ok := pm["items"].(map[string]any); ok && items["type"] == "object" {
			assertStrictObject(t, name, path+"/"+p+"/items", items)
		}
	}
}

func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestCommonFieldsRequired(t *testing.T) {
	for _, name := range allNames {
		t.Run(name, func(t *testing.T) {
			_, doc := compile(t, name)
			obj := doc.(map[string]any)
			required := map[string]bool{}
			for _, r := range toStrings(obj["required"]) {
				required[r] = true
			}
			for _, f := range commonFields {
				if !required[f] {
					t.Errorf("%s: common field %q not required", name, f)
				}
				if _, ok := obj["properties"].(map[string]any)[f]; !ok {
					t.Errorf("%s: common field %q missing from properties", name, f)
				}
			}
		})
	}
}

// TestSchemaValidatesGoldenVerdicts feeds one known-good verdict per task.
func TestSchemaValidatesGoldenVerdicts(t *testing.T) {
	good := map[string]string{
		"vacancy_scoring":        `{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"score":72,"band":"ok","target_role_match":true,"reasons":["match"],"red_flags":[],"missing_data":[]}`,
		"cover_letter":           `{"confidence":0.95,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Здравствуйте! Готов обсудить задачу и опыт.","mode":"ai_company","used_facts":[{"fact":"Go","source":"resume","quote":"Go"}],"missing_data":["англ. уровень"]}`,
		"chat_reply":             `{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"intent":"ack","reply":"Спасибо, рассмотрю.","used_facts":[]}`,
		"test_answer":            `{"confidence":0.85,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"answers":[{"question_id":"Q1","kind":"open","choice_indices":null,"open_text":"3 года","based_on_profile":true,"facts_used":[],"note":null}]}`,
		"warmup_ping":            `{"confidence":0.8,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"message":"Здравствуйте! Уточните, пожалуйста, статус.","should_send":true,"question_form":true,"used_facts":[]}`,
		"message_classification": `{"confidence":0.92,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"category":"rejection","evidence_quote":null}`,
		"contact_extraction":     `{"confidence":0.88,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"contacts":[{"type":"email","value":"hr@example.com","normalized":"hr@example.com","source_quote":"hr@example.com"}]}`,
		"link_detection":         `{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"links":[{"url":"https://forms.gle/x","kind":"google_form","risk":"medium","recommendation":"review","evidence_quote":null}],"summary":"1 ссылка: форма — на проверку."}`,
	}
	for name, verdict := range good {
		t.Run(name, func(t *testing.T) {
			sch, _ := compile(t, name)
			var inst any
			d := json.NewDecoder(strings.NewReader(verdict))
			d.UseNumber()
			if err := d.Decode(&inst); err != nil {
				t.Fatalf("decode verdict: %v", err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Fatalf("golden verdict rejected: %v", err)
			}
		})
	}
}

// TestSchemaRejectsExtraFields proves the strictness is enforced by the
// validator, not just declared in JSON.
func TestSchemaRejectsExtraFields(t *testing.T) {
	sch, _ := compile(t, "warmup_ping")
	var inst any
	d := json.NewDecoder(strings.NewReader(`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"message":"привет","evil":"x"}`))
	d.UseNumber()
	if err := d.Decode(&inst); err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("expected additional property to be rejected")
	}
}
