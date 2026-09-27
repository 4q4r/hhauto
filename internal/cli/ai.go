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

func buildAI(cfg *config.Config, db *store.DB, log *slog.Logger) (ai.Runner, error) {
	injectImproveTask(cfg)
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

	// Env-key fail-fast for every provider a configured model references.
	for modelID, m := range cfg.AI.Models {
		p, ok := cfg.AI.Providers[m.Provider]
		if !ok {
			continue // ai.New reports the reference error below, in order
		}
		if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
			return nil, fmt.Errorf("ai: models[%q] uses provider %q whose api_key_env %s is not set (AI features are enabled: set the variable or remove ai.tasks)",
				modelID, m.Provider, p.APIKeyEnv)
		}
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
	// T33: optional Jev prefilter. The key is resolved fail-fast like the
	// provider keys above — a half-configured prefilter must not start.
	if cfg.AI.Jev.Enabled {
		key := ""
		if cfg.AI.Jev.APIKeyEnv != "" {
			key = os.Getenv(cfg.AI.Jev.APIKeyEnv)
		}
		if key == "" {
			return nil, fmt.Errorf("ai: [ai.jev] enabled but api_key_env %s is not set (set the variable or disable ai.jev)", cfg.AI.Jev.APIKeyEnv)
		}
		tasks := make([]ai.TaskName, 0, len(cfg.AI.Jev.Tasks))
		for _, t := range cfg.AI.Jev.Tasks {
			tasks = append(tasks, ai.TaskName(t))
		}
		solver, err := ai.NewJevSolver(ai.NewJevClient(cfg.AI.Jev.Endpoint, key), cfg.AI.Jev.ConfidenceThreshold, tasks, log)
		if err != nil {
			return nil, fmt.Errorf("ai: jev: %w", err)
		}
		opts = append(opts, ai.WithJev(solver))
	}
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
