// T29 improve wiring: the [features.autoclone.improve] section rides the
// shared AI gateway. The model profile is declared once in the improve
// section; the CLI injects the matching [ai.tasks.resume_improve] slot
// (an explicit owner task entry wins) and constructs the improver with
// fail-fast validation of the field vocabulary.

package cli

import (
	"context"
	"fmt"
	"log/slog"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features/autoclone"
	"hhauto/internal/store"
)

// injectImproveTask ensures the resume_improve task exists when improve is
// enabled: the model profile always comes from the improve section (single
// source of truth); an explicit [ai.tasks.resume_improve] entry wins.
func injectImproveTask(cfg *config.Config) {
	if !cfg.Features.AutoClone.Improve.Enabled {
		return
	}
	if cfg.AI.Tasks == nil {
		cfg.AI.Tasks = map[string]config.AITask{}
	}
	if _, ok := cfg.AI.Tasks["resume_improve"]; !ok {
		cfg.AI.Tasks["resume_improve"] = config.AITask{
			ModelProfile: cfg.Features.AutoClone.Improve.ModelProfile,
		}
	}
}

// buildImprove constructs the clone improver from the improve config.
// Returns (nil, nil) when improve is disabled. Dry-run rides the general
// config flag: the diff is computed and logged, nothing is applied.
func buildImprove(cfg *config.Config, runner ai.Runner, db interface {
	ResumeClonesGet(ctx context.Context, cloneHash string) (*store.ResumeClone, error)
	ResumeClonesSetImproveDiff(ctx context.Context, cloneHash, diffJSON string) error
}, log *slog.Logger) (*autoclone.Improver, error) {
	improve := cfg.Features.AutoClone.Improve
	if !improve.Enabled {
		return nil, nil
	}
	fpr, ok := runner.(ai.FieldPromptRunner)
	if !ok {
		return nil, fmt.Errorf("autoclone: improve.enabled but the AI runner does not support per-field prompts (FieldPromptRunner)")
	}
	storeDB, ok := db.(*store.DB)
	if !ok {
		return nil, fmt.Errorf("autoclone: improve.enabled requires the store (lineage invariant)")
	}
	registry, err := autoclone.NewPromptRegistry(improve)
	if err != nil {
		return nil, err
	}
	return autoclone.New(autoclone.Options{
		Runner:   fpr,
		DB:       storeDB,
		Registry: registry,
		Cfg:      improve,
		DryRun:   cfg.General.DryRun,
		Log:      log,
	})
}
