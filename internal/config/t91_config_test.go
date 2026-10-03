package config

import (
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

// T91: the verdict caches of vacancy_scoring and cover_letter are alive by
// default (24h) — the owner pays real money and 60-100s per vacancy to
// re-roll the model on identical inputs (T86 report: 601/601 cache misses
// in the live DB). Other tasks are untouched: their caching was never
// asked for, so nil (no default) stays.
func TestApplyDefaultsVerdictCacheTTLT91(t *testing.T) {
	c := &Config{AI: AI{Tasks: map[string]AITask{
		"vacancy_scoring": {ModelProfile: "m"},
		"cover_letter":    {ModelProfile: "m"},
		"resume_improve":  {ModelProfile: "m"},
		"test_answer":     {ModelProfile: "m"},
	}}}
	applyDefaults(c)

	if got := c.AI.Tasks["vacancy_scoring"].CacheTTL(); got != 24 {
		t.Errorf("vacancy_scoring cache_ttl_hours = %d, want default 24", got)
	}
	if got := c.AI.Tasks["cover_letter"].CacheTTL(); got != 24 {
		t.Errorf("cover_letter cache_ttl_hours = %d, want default 24", got)
	}
	if c.AI.Tasks["resume_improve"].CacheTTLHours != nil {
		t.Errorf("resume_improve cache_ttl_hours = %v, want untouched (no default requested)",
			*c.AI.Tasks["resume_improve"].CacheTTLHours)
	}
	if c.AI.Tasks["test_answer"].CacheTTLHours != nil {
		t.Errorf("test_answer cache_ttl_hours = %v, want untouched (no default requested)",
			*c.AI.Tasks["test_answer"].CacheTTLHours)
	}
}

// R1-pattern guard: an explicit 0 in TOML means "disabled" and must
// survive applyDefaults — a naive int-default table cannot distinguish
// the zero value from "unset" and would overwrite it with 24. An explicit
// non-zero value must survive too.
func TestApplyDefaultsRespectsExplicitCacheTTLT91(t *testing.T) {
	zero := 0
	twelve := 12
	c := &Config{AI: AI{Tasks: map[string]AITask{
		"vacancy_scoring": {ModelProfile: "m", CacheTTLHours: &zero},
		"cover_letter":    {ModelProfile: "m", CacheTTLHours: &twelve},
	}}}
	applyDefaults(c)

	if got := c.AI.Tasks["vacancy_scoring"].CacheTTL(); got != 0 {
		t.Errorf("explicit cache_ttl_hours = 0 became %d, want 0 (disabled)", got)
	}
	if got := c.AI.Tasks["cover_letter"].CacheTTL(); got != 12 {
		t.Errorf("explicit cache_ttl_hours = 12 became %d, want 12", got)
	}
}

// The TOML path itself: an explicit cache_ttl_hours = 0 in the config file
// decodes to a non-nil pointer to 0 (not to "unset") and survives
// applyDefaults — the backwards-compatible way to keep the cache disabled.
func TestLoadExplicitZeroCacheTTLSurvivesT91(t *testing.T) {
	const doc = `
[ai.tasks.vacancy_scoring]
model_profile = "m"
cache_ttl_hours = 0
`
	var c Config
	if err := toml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	applyDefaults(&c)

	tsk := c.AI.Tasks["vacancy_scoring"]
	if tsk.CacheTTLHours == nil {
		t.Fatal("explicit cache_ttl_hours = 0 decoded to unset (nil): the explicit disable is dead")
	}
	if got := tsk.CacheTTL(); got != 0 {
		t.Errorf("explicit cache_ttl_hours = 0 became %d after defaults, want 0 (disabled)", got)
	}
}
