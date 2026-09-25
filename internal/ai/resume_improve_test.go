package ai

// T29 resume_improve: the ninth AI task — per-field resume improvement
// with caller-supplied system prompts (the autoclone feature resolves one
// prompt per field; builtin defaults live there, TOML overrides ride the
// improve config). RunWithSystem reuses the whole Run pipeline (guard,
// cache, budget, validation, post-checks, ledger) swapping only the system
// prompt; the prompt version becomes the content hash of the effective
// system so cache keys and ledger records change with every prompt edit.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hhauto/internal/config"
)

// improveModels is the single-profile model table for the improve tests.
func improveModels() map[string]config.AIModel {
	return map[string]config.AIModel{
		"main": {Provider: "p", Model: "test-model"},
	}
}

func improveTasks() map[string]config.AITask {
	return map[string]config.AITask{
		"resume_improve": {ModelProfile: "main"},
	}
}

func improveVerdict(t *testing.T, m map[string]any) string {
	t.Helper()
	m["confidence"] = 0.9
	m["needs_human"] = false
	m["needs_human_reason"] = nil
	m["injection_suspected"] = false
	m["unchanged"] = false
	m["used_facts"] = []string{}
	m["missing_data"] = []string{}
	for _, slot := range []string{"value_string", "value_strings", "value_amount"} {
		if _, ok := m[slot]; !ok {
			m[slot] = nil
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal verdict: %v", err)
	}
	return string(raw)
}

// The task must be a registered TaskName with a builtin prompt and a
// schema — ai.New fail-fasts otherwise (requireBuiltinCoverage).
func TestResumeImproveTaskRegistered(t *testing.T) {
	if _, ok := taskByName("resume_improve"); !ok {
		t.Fatal("resume_improve is not a registered task")
	}
	r := mustRunner(t, &scriptedProvider{}, improveModels(), improveTasks(), nil, nil)
	if _, ok := r.(FieldPromptRunner); !ok {
		t.Fatal("runner does not implement FieldPromptRunner")
	}
}

// RunWithSystem sends the caller's system prompt (plus TrustedBlocks) and
// records the ledger entry under the resume_improve task with a
// custom-hash prompt version.
func TestRunWithSystemUsesProvidedSystem(t *testing.T) {
	p := &scriptedProvider{contents: []string{improveVerdict(t, map[string]any{
		"value_string": "Улучшенный заголовок",
	})}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	verdict, dec, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "Ты улучшаешь поле title. Факты не изобретай.", ImproveInput{Field: "title"})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if !strings.Contains(p.lastSys, "поле title") {
		t.Fatalf("system prompt = %q", p.lastSys)
	}
	var out struct {
		ValueString string `json:"value_string"`
	}
	if err := json.Unmarshal(verdict, &out); err != nil {
		t.Fatalf("verdict: %v (%s)", err, verdict)
	}
	if out.ValueString != "Улучшенный заголовок" {
		t.Fatalf("value_string = %q", out.ValueString)
	}
	if dec.Task != TaskResumeImprove {
		t.Fatalf("ledger task = %q", dec.Task)
	}
	if !strings.HasPrefix(dec.PromptVer, "custom-") {
		t.Fatalf("promptVer = %q, want custom- hash", dec.PromptVer)
	}
}

// RunWithSystem on an unconfigured task is a hard error (the model profile
// binding is what makes the call possible).
func TestRunWithSystemUnknownTaskFails(t *testing.T) {
	r := mustRunner(t, &scriptedProvider{}, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	if _, _, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h", "r",
		TaskCoverLetter, "sys", nil); err == nil {
		t.Fatal("RunWithSystem with a task absent from [ai.tasks] must fail")
	}
}

// Deterministic grounding: an improved free-text value that introduces
// numbers absent from the trusted sources must force needs_human (facts
// are never invented).
func TestResumeImproveNumberGrounding(t *testing.T) {
	p := &scriptedProvider{contents: []string{improveVerdict(t, map[string]any{
		"value_string": "Опыт 7 лет в автоматизации",
	})}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	verdict, dec, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "sys", ImproveInput{
			Field:        "about",
			TrustedFacts: []string{"Опыт 3 года в автоматизации"},
		})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if !dec.NeedsHuman {
		t.Fatalf("invented number must force needs_human, verdict=%s", verdict)
	}
	var out struct {
		Reason string `json:"needs_human_reason"`
	}
	_ = json.Unmarshal(verdict, &out)
	if out.Reason != reasonNumberNotInSrc {
		t.Fatalf("reason = %q, want %q", out.Reason, reasonNumberNotInSrc)
	}
}

// A salary improvement whose amount is absent from the trusted facts is a
// fact risk: needs_human, never a silent accept.
func TestResumeImproveSalaryGrounding(t *testing.T) {
	p := &scriptedProvider{contents: []string{improveVerdict(t, map[string]any{
		"value_amount": 999999,
	})}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	verdict, dec, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "sys", ImproveInput{
			Field:        "salary",
			TrustedFacts: []string{"вилка 150000-200000"},
		})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if !dec.NeedsHuman {
		t.Fatalf("ungrounded salary must force needs_human, verdict=%s", verdict)
	}
	var out struct {
		Reason string `json:"needs_human_reason"`
	}
	_ = json.Unmarshal(verdict, &out)
	if out.Reason != reasonNumberNotInSrc {
		t.Fatalf("reason = %q, want %q", out.Reason, reasonNumberNotInSrc)
	}
}

// A grounded salary (the amount appears in the owner's facts) passes the
// post-check untouched.
func TestResumeImproveSalaryGroundedPasses(t *testing.T) {
	p := &scriptedProvider{contents: []string{improveVerdict(t, map[string]any{
		"value_amount": 180000,
	})}}
	r := mustRunner(t, p, improveModels(), improveTasks(), nil, nil)
	fpr := r.(FieldPromptRunner)
	_, dec, err := fpr.RunWithSystem(context.Background(), "acc", "resume", "h4sh", "run1",
		TaskResumeImprove, "sys", ImproveInput{
			Field:        "salary",
			TrustedFacts: []string{"целевая вилка 180000-200000"},
		})
	if err != nil {
		t.Fatalf("RunWithSystem: %v", err)
	}
	if dec.NeedsHuman {
		t.Fatalf("grounded salary must pass, verdict needs_human")
	}
}
