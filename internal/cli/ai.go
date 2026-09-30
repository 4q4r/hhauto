package cli

// buildAI constructs the ai.Runner from [ai] config: one dialect provider
// per ai.providers entry, the store-backed decision ledger, the in-memory
// verdict cache and the daily-budget guard.
//
// Fail-fast rule (owner decision): when any AI task is configured, every
// api_key_env referenced by a provider must resolve to a non-empty env var
// — a half-configured AI feature must not start and later degrade into
// silent needs_human storms. With no tasks configured the Runner is nil:
// features treat that as "AI features off".

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/store"
)

// applyCoverLetterProfileOverride wires the T60 cover_letter section model
// profile onto the [ai.tasks.cover_letter] slot (empty = task default; a
// missing task has nothing to override).
func applyCoverLetterProfileOverride(cfg *config.Config) {
	cl := cfg.Features.MultiApply.CoverLetter
	if !cl.Enabled || cl.ModelProfile == "" {
		return
	}
	if t, ok := cfg.AI.Tasks[string(ai.TaskCoverLetter)]; ok {
		t.ModelProfile = cl.ModelProfile
		cfg.AI.Tasks[string(ai.TaskCoverLetter)] = t
	}
}

// providerKeysFailFast (T48) checks every provider a configured model
// references: a config-file api_key is exempt from the environment
// requirement; a provider with neither key source, or whose api_key_env
// is unset, is a startup error — a half-configured AI feature must not
// start and later degrade into silent needs_human storms.
func providerKeysFailFast(cfg *config.Config) error {
	for modelID, m := range cfg.AI.Models {
		p, ok := cfg.AI.Providers[m.Provider]
		if !ok {
			continue // ai.New reports the reference error below, in order
		}
		switch {
		case p.APIKey != "":
			// config-file key wins; no environment variable required.
		case p.APIKeyEnv == "":
			return fmt.Errorf("ai: models[%q] uses provider %q with neither api_key nor api_key_env (AI features are enabled: set one or remove ai.tasks)",
				modelID, m.Provider)
		case os.Getenv(p.APIKeyEnv) == "":
			return fmt.Errorf("ai: models[%q] uses provider %q whose api_key_env %s is not set (AI features are enabled: set the variable or remove ai.tasks)",
				modelID, m.Provider, p.APIKeyEnv)
		}
	}
	return nil
}

func buildAI(cfg *config.Config, db *store.DB, log *slog.Logger, forceImproveTask bool) (ai.Runner, error) {
	injectImproveTask(cfg, forceImproveTask)
	injectSetkaTasks(cfg)
	if len(cfg.AI.Tasks) == 0 {
		return nil, nil
	}

	providers := make(map[string]ai.Provider, len(cfg.AI.Providers))
	for id, p := range cfg.AI.Providers {
		var (
			prov ai.Provider
			err  error
		)
		switch p.Dialect {
		case ai.DialectOpenAIChat:
			prov, err = ai.NewOpenAIChatProvider(id, p)
		case ai.DialectOpenAIResponses:
			prov, err = ai.NewOpenAIResponsesProvider(id, p)
		case ai.DialectAnthropicMessages:
			prov, err = ai.NewAnthropicProvider(id, p)
		default:
			return nil, fmt.Errorf("ai: provider %q: unknown dialect %q", id, p.Dialect)
		}
		if err != nil {
			return nil, fmt.Errorf("ai: provider %q: %w", id, err)
		}
		providers[id] = prov
	}

	if err := providerKeysFailFast(cfg); err != nil {
		return nil, err
	}

	var ledger ai.LedgerStore
	if db != nil {
		ledger = aiLedgerBridge{db: db}
	}
	opts := []ai.RunnerOption{
		ai.WithBudget(ai.BudgetConfig{
			DailyTokens: cfg.AI.DailyBudgetTokens,
			DailyUSD:    cfg.AI.DailyBudgetUSD,
			OnExceed:    cfg.AI.OnBudgetExceed,
			Pricing:     cfg.AI.Pricing,
		}),
	}
	// T33/T60: the Jev endpoint is installed only when it IS the classifier
	// ([features.multiapply] classifier="jev" — an exclusive choice
	// enforced by config validation). The key is resolved fail-fast like
	// the provider keys above — a half-configured classifier must not
	// start. T74: the config-file api_key wins over api_key_env (the
	// owner runs keyless environments).
	if cfg.Features.MultiApply.Classifier == config.ClassifierJev && cfg.AI.Jev.Enabled {
		j := cfg.AI.Jev
		key := j.APIKey
		if key == "" && j.APIKeyEnv != "" {
			key = os.Getenv(j.APIKeyEnv)
		}
		if key == "" {
			return nil, fmt.Errorf("ai: [ai.jev] enabled but neither api_key nor api_key_env resolves to a key (set ai.jev.api_key in the config or export %s)", j.APIKeyEnv)
		}
		tasks := make([]ai.TaskName, 0, len(j.Tasks))
		for _, t := range j.Tasks {
			tasks = append(tasks, ai.TaskName(t))
		}
		client := ai.NewJevClient(j.Endpoint, key, ai.JevClientOptions{
			Wire:        j.Wire,
			Model:       j.Model,
			HTTPReferer: j.HTTPReferer,
			XTitle:      j.Title,
		})
		solver, err := ai.NewJevSolver(client, j.ConfidenceThreshold, tasks, log)
		if err != nil {
			return nil, fmt.Errorf("ai: jev: %w", err)
		}
		opts = append(opts, ai.WithJev(solver))
	}
	// T60: the cover_letter section model profile overrides the letter
	// task's own profile (empty = the task default).
	applyCoverLetterProfileOverride(cfg)
	runner, err := ai.New(
		providers,
		cfg.AI.Models,
		cfg.AI.Tasks,
		ledger,
		ai.NewMemoryCache(),
		log,
		opts...,
	)
	if err != nil {
		return nil, fmt.Errorf("ai: %w", err)
	}
	return runner, nil
}

// aiLedgerBridge maps the ai.LedgerStore seam onto the store DAOs. The
// budget guard's SumTokensDay has no error return, so a ledger read failure
// degrades to "0 used" with an ERROR log: budget enforcement may over-admit
// for the day, which is observable in ai_decisions.
type aiLedgerBridge struct{ db *store.DB }

func (b aiLedgerBridge) Add(rec *ai.DecisionRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	score := rec.Score
	var scorePtr *float64
	if score != 0 {
		scorePtr = &score
	}
	conf := rec.Confidence
	var confPtr *float64
	if conf != 0 {
		confPtr = &conf
	}
	return b.db.AIDecisionsAdd(ctx, &store.AIDecision{
		ID: rec.ID, CreatedAt: rec.CreatedAt,
		AccountID: rec.AccountID, Task: string(rec.Task),
		EntityType: rec.EntityType, EntityID: rec.EntityID, RunID: rec.RunID,
		Model: rec.Model, PromptVer: rec.PromptVer, InputDigest: rec.InputDigest,
		Verdict:    string(rec.Verdict),
		Score:      scorePtr,
		Confidence: confPtr,
		NeedsHuman: rec.NeedsHuman, Injection: rec.Injection,
		LatencyMS: int(rec.LatencyMS), TokensIn: rec.TokensIn, TokensOut: rec.TokensOut,
		CacheHit: rec.CacheHit, CostUSD: rec.CostUSD, Outcome: rec.Outcome,
	})
}

func (b aiLedgerBridge) SumTokensDay(day string) (int, float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tokens, usd, err := b.db.AIDecisionsSumTokensDayAll(ctx, day)
	if err != nil {
		slog.Default().Error("cli: ai ledger day sum failed; budget guard degrades to 0", "err", err)
		return 0, 0
	}
	return tokens, usd
}

// injectSetkaTasks (T61) ensures the setka_classify / setka_personalize
// tasks exist when [features.setka] is enabled: the model profiles are
// declared once in the setka section (single source of truth); an explicit
// [ai.tasks.*] entry wins. prompt_file overrides ride along like any task
// file override (the PromptRegistry fails fast on an unreadable file).
func injectSetkaTasks(cfg *config.Config) {
	s := cfg.Features.Setka
	if !s.Enabled {
		return
	}
	if cfg.AI.Tasks == nil {
		cfg.AI.Tasks = map[string]config.AITask{}
	}
	if _, ok := cfg.AI.Tasks[string(ai.TaskSetkaClassify)]; !ok && s.Classify.ModelProfile != "" {
		cfg.AI.Tasks[string(ai.TaskSetkaClassify)] = config.AITask{
			ModelProfile: s.Classify.ModelProfile,
			PromptFile:   s.Classify.PromptFile,
		}
	}
	if _, ok := cfg.AI.Tasks[string(ai.TaskSetkaPersonalize)]; !ok && s.Personalize.ModelProfile != "" {
		cfg.AI.Tasks[string(ai.TaskSetkaPersonalize)] = config.AITask{
			ModelProfile: s.Personalize.ModelProfile,
			PromptFile:   s.Personalize.PromptFile,
		}
	}
}
