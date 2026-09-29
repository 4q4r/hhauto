package cli

// T60: the [features.multiapply.cover_letter] model profile overrides the
// cover_letter task's own [ai.tasks] profile (empty = task default).
import (
	"testing"

	"hhauto/internal/config"
)

func TestApplyCoverLetterProfileOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Features.MultiApply.CoverLetter = config.MultiApplyCoverLetter{
		Enabled:      true,
		ModelProfile: "letter-model",
	}
	cfg.AI.Tasks = map[string]config.AITask{"cover_letter": {ModelProfile: "task-default"}}

	applyCoverLetterProfileOverride(cfg)
	if got := cfg.AI.Tasks["cover_letter"].ModelProfile; got != "letter-model" {
		t.Fatalf("task profile = %q, want the section override", got)
	}

	// Section disabled: no override.
	cfg2 := &config.Config{}
	cfg2.AI.Tasks = map[string]config.AITask{"cover_letter": {ModelProfile: "task-default"}}
	applyCoverLetterProfileOverride(cfg2)
	if got := cfg2.AI.Tasks["cover_letter"].ModelProfile; got != "task-default" {
		t.Fatalf("disabled section overwrote the task profile: %q", got)
	}

	// Empty override: task default preserved.
	cfg3 := &config.Config{}
	cfg3.Features.MultiApply.CoverLetter = config.MultiApplyCoverLetter{Enabled: true}
	cfg3.AI.Tasks = map[string]config.AITask{"cover_letter": {ModelProfile: "task-default"}}
	applyCoverLetterProfileOverride(cfg3)
	if got := cfg3.AI.Tasks["cover_letter"].ModelProfile; got != "task-default" {
		t.Fatalf("empty override changed the task profile: %q", got)
	}

	// Missing letter task: nothing to override, no panic.
	cfg4 := &config.Config{}
	cfg4.Features.MultiApply.CoverLetter = config.MultiApplyCoverLetter{
		Enabled: true, ModelProfile: "letter-model",
	}
	cfg4.AI.Tasks = map[string]config.AITask{}
	applyCoverLetterProfileOverride(cfg4)
}
