package cli

// T29 wiring tests: the improve section injects its [ai.tasks.resume_improve]
// slot (model profile from [features.autoclone.improve]) and the improver
// is constructed only for a fully-wired improve config.
import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/config"
)

func improveWiringCfg() *config.Config {
	var c config.Config
	c.Features.AutoClone.Enabled = true
	c.Features.AutoClone.Improve = config.AutoCloneImprove{
		Enabled:      true,
		Fields:       []string{"title", "about"},
		ModelProfile: "main",
	}
	return &c
}

// The resume_improve task slot is injected with the improve model profile;
// an existing explicit [ai.tasks.resume_improve] wins (owner override).
func TestInjectImproveTask(t *testing.T) {
	cfg := improveWiringCfg()
	cfg.AI.Tasks = map[string]config.AITask{}
	injectImproveTask(cfg)
	got, ok := cfg.AI.Tasks["resume_improve"]
	if !ok || got.ModelProfile != "main" {
		t.Fatalf("injected task = %+v", got)
	}

	// Explicit owner config wins over the injection.
	cfg.AI.Tasks["resume_improve"] = config.AITask{ModelProfile: "custom"}
	injectImproveTask(cfg)
	if cfg.AI.Tasks["resume_improve"].ModelProfile != "custom" {
		t.Fatalf("explicit task overwritten: %+v", cfg.AI.Tasks["resume_improve"])
	}

	// Improve disabled: nothing injected.
	cfg2 := improveWiringCfg()
	cfg2.Features.AutoClone.Improve.Enabled = false
	cfg2.AI.Tasks = map[string]config.AITask{}
	injectImproveTask(cfg2)
	if _, ok := cfg2.AI.Tasks["resume_improve"]; ok {
		t.Fatal("disabled improve must not inject the task")
	}
}

// buildImprove: no runner / disabled config → nil improver; a broken field
// vocabulary fails fast at startup.
func TestBuildImprove(t *testing.T) {
	cfg := improveWiringCfg()
	cfg.AI.Tasks = map[string]config.AITask{}
	injectImproveTask(cfg)

	// Disabled → nil.
	cfg.Features.AutoClone.Improve.Enabled = false
	if imp, err := buildImprove(cfg, nil, nil, testLoggerT29()); err != nil || imp != nil {
		t.Fatalf("disabled improve = %v, %v", imp, err)
	}

	// Enabled but the runner does not implement FieldPromptRunner → error.
	cfg.Features.AutoClone.Improve.Enabled = true
	if _, err := buildImprove(cfg, stubRunnerT29{}, nil, testLoggerT29()); err == nil {
		t.Fatal("improve with a plain runner must fail (no FieldPromptRunner)")
	}

	// Unknown field fails at wiring.
	cfg.Features.AutoClone.Improve.Fields = []string{"bogus"}
	stub := &fieldPromptRunnerStubT29{}
	if _, err := buildImprove(cfg, stub, nil, testLoggerT29()); err == nil {
		t.Fatal("unknown improve field must fail at wiring")
	}
}

type stubRunnerT29 struct{}

func (stubRunnerT29) Run(context.Context, string, string, string, string, ai.TaskName, any) (json.RawMessage, ai.Decision, error) {
	return nil, ai.Decision{}, nil
}

type fieldPromptRunnerStubT29 struct{ stubRunnerT29 }

func (fieldPromptRunnerStubT29) RunWithSystem(context.Context, string, string, string, string, ai.TaskName, string, any) (json.RawMessage, ai.Decision, error) {
	return nil, ai.Decision{}, nil
}

func testLoggerT29() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
