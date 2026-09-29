package config

// T58: [features.autoclone.improve].experience_replace — the fork-gap
// closer for the edit contract (live T55: upsert-by-id forks edited
// experience records; the replace flow deletes the replaced generations).
// Owner decision: default true (the owner's clone already grew duplicates
// from improve applies); an explicit false restores the raw T51 behavior.

import (
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestApplyDefaultsT58ExperienceReplace(t *testing.T) {
	var c Config
	applyDefaults(&c)
	if c.Features.AutoClone.Improve.ExperienceReplace == nil || !*c.Features.AutoClone.Improve.ExperienceReplace {
		t.Fatalf("features.autoclone.improve.experience_replace = nil/false, want default true")
	}
}

func TestExperienceReplaceTOMLExplicitFalse(t *testing.T) {
	var c Config
	if err := toml.Unmarshal([]byte(`
[features.autoclone.improve]
experience_replace = false
`), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	applyDefaults(&c)
	if c.Features.AutoClone.Improve.ExperienceReplace == nil || *c.Features.AutoClone.Improve.ExperienceReplace {
		t.Fatalf("explicit experience_replace=false must survive defaults, got %v", c.Features.AutoClone.Improve.ExperienceReplace)
	}
}
