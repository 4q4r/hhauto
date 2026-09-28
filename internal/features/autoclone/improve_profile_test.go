package autoclone

// T41 profile.md wiring: the improve pipeline loads the trusted candidate
// context (internal/profile — profile text plus owner Q&A rules) once per
// run, rides it in every ImproveInput as trusted data, and substitutes
// the {{candidate_context}} placeholder of owner prompt overrides. A nil
// loader or a load failure degrades to an empty context — the improve run
// itself never fails on it.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/profile"
)

// capturingRunner records the ImproveInput per field on top of the
// scripted verdict behavior.
type capturingRunner struct {
	fakeImproveRunner
	inputs map[string]ai.ImproveInput
}

func newCapturingRunner(t *testing.T, field, value string) *capturingRunner {
	t.Helper()
	return &capturingRunner{
		fakeImproveRunner: fakeImproveRunner{verdicts: map[string]func() string{
			field: improveVerdictFor(t, map[string]any{"value_string": value}),
		}},
		inputs: map[string]ai.ImproveInput{},
	}
}

func (r *capturingRunner) RunWithSystem(ctx context.Context, acc, et, id, run string, task ai.TaskName, system string, input any) (json.RawMessage, ai.Decision, error) {
	in, ok := input.(ai.ImproveInput)
	if ok {
		r.inputs[in.Field] = in
	}
	return r.fakeImproveRunner.RunWithSystem(ctx, acc, et, id, run, task, system, input)
}

// The candidate context rides the input's trusted block (CandidateContext)
// for the profile text plus the stored owner Q&A rules; the resume digest
// section stays out (the live resume facts already ride per field).
func TestImproveCandidateContextRidesInput(t *testing.T) {
	run := newCapturingRunner(t, "title", "DevOps-инженер SRE")
	im := newImprover(t, baseImproveCfg(), newFakeEditor(t), run, testStore(t), nil)
	im.opt.ProfileFor = func(_ context.Context, accountID string) (profile.Profile, error) {
		return profile.Profile{
			AccountID:    accountID,
			ProfileText:  "Целевая вилка: 250000–300000 RUR",
			ResumeDigest: "resume digest that must not ride the improve context",
			Facts:        map[string]string{"переезд": "не рассматриваю офис"},
		}, nil
	}
	if _, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123"); err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	in, ok := run.inputs["title"]
	if !ok {
		t.Fatal("no captured input for title")
	}
	if !strings.Contains(in.CandidateContext, "Целевая вилка: 250000–300000 RUR") {
		t.Errorf("candidate context lacks the profile text: %q", in.CandidateContext)
	}
	if !strings.Contains(in.CandidateContext, "переезд") {
		t.Errorf("candidate context lacks the stored owner facts: %q", in.CandidateContext)
	}
	if strings.Contains(in.CandidateContext, "resume digest that must not ride") {
		t.Errorf("candidate context must not duplicate the resume digest: %q", in.CandidateContext)
	}
}

// The {{candidate_context}} placeholder of an owner prompt override is
// substituted with the rendered context.
func TestImproveCandidateContextPlaceholderSubstituted(t *testing.T) {
	cfg := baseImproveCfg()
	cfg.Prompts = map[string]config.AITask{
		"title": {PromptInline: "Улучшай title. Контекст кандидата:\n{{candidate_context}}\nФакты не изобретай."},
	}
	run := newCapturingRunner(t, "title", "DevOps-инженер SRE")
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	im, err := New(Options{
		API: newFakeEditor(t), Runner: run, DB: testStore(t), Registry: reg, Cfg: cfg,
		Log: testLoggerT(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	im.opt.ProfileFor = func(context.Context, string) (profile.Profile, error) {
		return profile.Profile{ProfileText: "19 лет, Таганрог, remote-only"}, nil
	}
	if _, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123"); err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	sys := run.systems["title"]
	if !strings.Contains(sys, "19 лет, Таганрог, remote-only") {
		t.Errorf("placeholder not substituted in the system prompt: %q", sys)
	}
	if strings.Contains(sys, "{{candidate_context}}") {
		t.Errorf("dangling placeholder left in the system prompt: %q", sys)
	}
}

// Without a profile (nil loader) the placeholder becomes an explicit
// "(профиль не предоставлен)" — never a dangling template marker.
func TestImproveCandidateContextPlaceholderWithoutProfile(t *testing.T) {
	cfg := baseImproveCfg()
	cfg.Prompts = map[string]config.AITask{
		"title": {PromptInline: "Улучшай title. Контекст кандидата:\n{{candidate_context}}\nФакты не изобретай."},
	}
	run := newCapturingRunner(t, "title", "DevOps-инженер SRE")
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	im, err := New(Options{
		API: newFakeEditor(t), Runner: run, DB: testStore(t), Registry: reg, Cfg: cfg,
		Log: testLoggerT(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123"); err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	sys := run.systems["title"]
	if !strings.Contains(sys, "(профиль не предоставлен)") {
		t.Errorf("absent profile must substitute an explicit note: %q", sys)
	}
	if in := run.inputs["title"]; in.CandidateContext != "" {
		t.Errorf("CandidateContext must stay empty without a profile, got %q", in.CandidateContext)
	}
}

// The wired base prompt (resume_improve v5, resolved by the CLI from
// [ai.tasks]) composes with the per-field prompt: base first, the field
// instructions after — the RunWithSystem contract.
func TestImproveComposesBasePromptWithFieldPrompt(t *testing.T) {
	run := newCapturingRunner(t, "title", "DevOps-инженер SRE")
	im := newImprover(t, baseImproveCfg(), newFakeEditor(t), run, testStore(t), nil)
	im.opt.BasePrompt = "BASE v5: ranking-optimization module contract."
	if _, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123"); err != nil {
		t.Fatalf("ImproveAnyResume: %v", err)
	}
	sys := run.systems["title"]
	if !strings.HasPrefix(sys, "BASE v5: ranking-optimization module contract.") {
		t.Errorf("system must start with the base prompt: %q", sys)
	}
	if !strings.Contains(sys, "Ты улучшаешь заголовок") {
		t.Errorf("system must carry the per-field prompt after the base: %q", sys)
	}
}

// A profile load failure degrades to an empty context: the run completes,
// the field still improves on the live resume facts.
func TestImproveCandidateContextLoadFailureDegrades(t *testing.T) {
	run := newCapturingRunner(t, "title", "DevOps-инженер SRE")
	im := newImprover(t, baseImproveCfg(), newFakeEditor(t), run, testStore(t), nil)
	im.opt.ProfileFor = func(context.Context, string) (profile.Profile, error) {
		return profile.Profile{}, errors.New("store down")
	}
	artifact, err := im.ImproveAnyResume(context.Background(), "acc1", "b4se123")
	if err != nil {
		t.Fatalf("ImproveAnyResume must survive a profile load failure: %v", err)
	}
	if in := run.inputs["title"]; in.CandidateContext != "" {
		t.Errorf("CandidateContext must be empty on load failure, got %q", in.CandidateContext)
	}
	if d := fieldDiffByLocator(t, artifact, "resume.title"); d.Status != StatusImproved {
		t.Errorf("title status = %s, want improved (degradation must not break the flow)", d.Status)
	}
}
