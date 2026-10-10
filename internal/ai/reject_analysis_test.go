package ai

// T123: the reject_analysis task — grounding post-check on improved_letter
// (same number-leak class as cover_letter) and prompt coverage.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hhauto/internal/aiprompts"
	"hhauto/internal/config"
)

const rejectVerdictWithNumbers = `{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"root_cause":"Отказ по грейду","critique":"Письмо слишком общее","improved_letter":"Сократил уязвимости на 45%, готов к переезду.","missing_skills":["Kubernetes"],"about_me_rec":"","takeaway":"Уточнить грейд"}`

func TestRunRejectAnalysisNumberNotInSources(t *testing.T) {
	p := &scriptedProvider{contents: []string{rejectVerdictWithNumbers}}
	tasks := map[string]config.AITask{"reject_analysis": {ModelProfile: "profile"}}
	r, err := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"vacancy":       map[string]any{"name": "DevOps-инженер"},
		"resume_digest": "опыт 4 года, PostgreSQL",
		"profile_text":  "ищу удалёнку",
	}
	verdict, dec, err := r.Run(context.Background(), "a", "negotiation", "1", "r", TaskRejectionAnalysis, input)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.NeedsHuman {
		t.Errorf("decision = %+v, want needs_human", dec)
	}
	if reasonOf(verdict) != "number_not_in_sources" {
		t.Errorf("reason = %q, want number_not_in_sources", reasonOf(verdict))
	}
}

func TestRunRejectAnalysisGroundedLetterPasses(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"root_cause":"Отказ по грейду","critique":"Письмо слишком общее","improved_letter":"Разрабатываю на Go 4 года, в основном PostgreSQL.","missing_skills":["Kubernetes"],"about_me_rec":"","takeaway":"Уточнить грейд"}`}}
	tasks := map[string]config.AITask{"reject_analysis": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"resume_digest": "опыт 4 года коммерческой разработки",
		"profile_text":  "стек: PostgreSQL",
	}
	verdict, dec, err := r.Run(context.Background(), "a", "negotiation", "1", "r", TaskRejectionAnalysis, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec.NeedsHuman {
		t.Errorf("grounded letter flagged: reason = %q", reasonOf(verdict))
	}
	var v struct {
		RootCause string `json:"root_cause"`
	}
	if err := json.Unmarshal(verdict, &v); err != nil {
		t.Fatal(err)
	}
	if v.RootCause != "Отказ по грейду" {
		t.Errorf("root_cause = %q", v.RootCause)
	}
}

func TestRejectAnalysisBuiltinPrompt(t *testing.T) {
	reg, err := NewPromptRegistry(map[string]config.AITask{"reject_analysis": {}}, aiprompts.FS)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	body, ver, err := reg.SystemPrompt(TaskRejectionAnalysis)
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}
	if len(body) < 200 {
		t.Errorf("prompt body too short (%d bytes)", len(body))
	}
	if !strings.Contains(body, "JSON") {
		t.Error("prompt does not mention JSON output")
	}
	if ver == "" {
		t.Error("empty prompt version")
	}
}
