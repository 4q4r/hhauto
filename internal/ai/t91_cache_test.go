package ai

import (
	"context"
	"testing"

	"hhauto/internal/config"
)

// T91 runner-level guards: the config defaults (24h for vacancy_scoring and
// cover_letter) materialize as CacheTTLHours=ptr(24) before the runner sees
// the tasks map, so these tests pin the contract the runner must keep after
// the int→*int field change: 24h caches identical verdicts, explicit 0 and
// unset (nil — what untouched tasks keep) stay disabled, and the prompt
// version is part of the cache key.

const validCoverLetterVerdictT91 = `{"confidence":0.95,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Здравствуйте! Готов обсудить задачу и опыт.","mode":"ai_company","used_facts":[{"fact":"Go","source":"resume","quote":"Go"}],"missing_data":["англ. уровень"]}`

func ttlTasks(task TaskName, ttl *int) map[string]config.AITask {
	return map[string]config.AITask{string(task): {ModelProfile: "profile", CacheTTLHours: ttl}}
}

// The default 24h TTL: a repeated scoring of an identical input hits the
// cache without a second provider (LLM) call.
func TestRunDefaultTTLCacheHitScoringT91(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict}}
	r := mustRunner(t, p, oneModel(""), ttlTasks(TaskVacancyScoring, intTTL(24)), nil, NewMemoryCache())
	ctx := context.Background()
	input := map[string]any{"vacancy": "same"}
	if _, dec1, err := r.Run(ctx, "a", "vacancy", "7", "r1", TaskVacancyScoring, input); err != nil {
		t.Fatalf("first Run: %v", err)
	} else if dec1.CacheHit {
		t.Error("first run must be a miss")
	}
	if _, dec2, err := r.Run(ctx, "a", "vacancy", "7", "r2", TaskVacancyScoring, input); err != nil {
		t.Fatalf("second Run: %v", err)
	} else if !dec2.CacheHit {
		t.Error("second identical run must be a cache hit with the 24h default")
	}
	if p.callCount() != 1 {
		t.Errorf("provider calls = %d, want 1 (LLM must not be re-rolled)", p.callCount())
	}
}

// cover_letter is the second task with the 24h default: same contract.
func TestRunDefaultTTLCacheHitCoverLetterT91(t *testing.T) {
	p := &scriptedProvider{contents: []string{validCoverLetterVerdictT91}}
	r := mustRunner(t, p, oneModel(""), ttlTasks(TaskCoverLetter, intTTL(24)), nil, NewMemoryCache())
	ctx := context.Background()
	input := map[string]any{"vacancy": "same"}
	if _, _, err := r.Run(ctx, "a", "letter", "7", "r1", TaskCoverLetter, input); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	_, dec2, err := r.Run(ctx, "a", "letter", "7", "r2", TaskCoverLetter, input)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !dec2.CacheHit || p.callCount() != 1 {
		t.Errorf("cover_letter second run: cacheHit=%v providerCalls=%d, want hit/1", dec2.CacheHit, p.callCount())
	}
}

// An explicit cache_ttl_hours = 0 (backwards compatible) keeps the cache
// disabled: every run re-rolls the model.
func TestRunExplicitZeroDisablesCacheT91(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict, validScoringVerdict}}
	r := mustRunner(t, p, oneModel(""), ttlTasks(TaskVacancyScoring, intTTL(0)), nil, NewMemoryCache())
	ctx := context.Background()
	input := map[string]any{"vacancy": "same"}
	if _, _, err := r.Run(ctx, "a", "vacancy", "7", "r1", TaskVacancyScoring, input); err != nil {
		t.Fatal(err)
	}
	_, dec2, err := r.Run(ctx, "a", "vacancy", "7", "r2", TaskVacancyScoring, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec2.CacheHit || p.callCount() != 2 {
		t.Errorf("explicit zero: cacheHit=%v providerCalls=%d, want miss/2 (disabled)", dec2.CacheHit, p.callCount())
	}
}

// Untouched tasks keep nil (no default): nil must stay "disabled" at the
// runner level after the *int change.
func TestRunUnsetTTLDisablesCacheT91(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict, validScoringVerdict}}
	r := mustRunner(t, p, oneModel(""), ttlTasks(TaskVacancyScoring, nil), nil, NewMemoryCache())
	ctx := context.Background()
	input := map[string]any{"vacancy": "same"}
	if _, _, err := r.Run(ctx, "a", "vacancy", "7", "r1", TaskVacancyScoring, input); err != nil {
		t.Fatal(err)
	}
	_, dec2, err := r.Run(ctx, "a", "vacancy", "7", "r2", TaskVacancyScoring, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec2.CacheHit || p.callCount() != 2 {
		t.Errorf("unset TTL: cacheHit=%v providerCalls=%d, want miss/2 (no default for untouched tasks)", dec2.CacheHit, p.callCount())
	}
}

// The prompt version is part of the cache key: bumping it (T78 v3→v4, T85
// v2→v3 prompt bumps do exactly this) invalidates cached verdicts.
func TestCacheKeyPromptVersionChangesKeyT91(t *testing.T) {
	model := config.AIModel{Model: "m1", MaxOutputTokens: 500, Temperature: 0.2}
	input := []byte(`{"x":1}`)
	k1 := cacheKey(TaskVacancyScoring, "v3", model, "", []byte(`{}`), input)
	k2 := cacheKey(TaskVacancyScoring, "v4", model, "", []byte(`{}`), input)
	if k1 == k2 {
		t.Error("different prompt versions must produce different cache keys")
	}
}

// End to end: two runners sharing one cache store but resolving different
// prompt versions — the second run is a miss and re-calls the provider.
func TestRunPromptVerBumpInvalidatesCacheT91(t *testing.T) {
	prompt := "SYSTEM PROMPT BODY"
	tasks := func(ver string) map[string]config.AITask {
		return map[string]config.AITask{
			"vacancy_scoring": {ModelProfile: "profile", CacheTTLHours: intTTL(24), PromptInline: prompt, PromptVersion: ver},
		}
	}
	cache := NewMemoryCache()
	p := &scriptedProvider{contents: []string{validScoringVerdict, validScoringVerdict}}
	r1 := mustRunner(t, p, oneModel(""), tasks("v3"), nil, cache)
	r2 := mustRunner(t, p, oneModel(""), tasks("v4"), nil, cache)
	ctx := context.Background()
	input := map[string]any{"vacancy": "same"}
	if _, _, err := r1.Run(ctx, "a", "vacancy", "7", "r1", TaskVacancyScoring, input); err != nil {
		t.Fatal(err)
	}
	_, dec2, err := r2.Run(ctx, "a", "vacancy", "7", "r2", TaskVacancyScoring, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec2.CacheHit || p.callCount() != 2 {
		t.Errorf("prompt bump: cacheHit=%v providerCalls=%d, want miss/2 (invalidated)", dec2.CacheHit, p.callCount())
	}
}

func intTTL(h int) *int { return &h }
