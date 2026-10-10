package config

// T115: the search-item pre-filter knobs — both default-off (the
// userLabels semantics are undocumented; conservative), an explicit TOML
// value is respected.

import (
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

func TestMultiApplyPrefilterDefaultsOff(t *testing.T) {
	var c Config
	applyDefaults(&c)
	if c.Features.MultiApply.SkipWhenLabeled {
		t.Error("skip_when_labeled must default to false (off)")
	}
	if c.Features.MultiApply.MaxTotalResponses != 0 {
		t.Errorf("max_total_responses default = %d, want 0 (off)", c.Features.MultiApply.MaxTotalResponses)
	}
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestMultiApplyPrefilterExplicitValues(t *testing.T) {
	var c Config
	if err := toml.Unmarshal([]byte(`
[features.multiapply]
skip_when_labeled = true
max_total_responses = 50
`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	applyDefaults(&c) // the Load pipeline; explicit values survive (zero-fill only)
	if !c.Features.MultiApply.SkipWhenLabeled {
		t.Error("skip_when_labeled = false, want true")
	}
	if c.Features.MultiApply.MaxTotalResponses != 50 {
		t.Errorf("max_total_responses = %d, want 50", c.Features.MultiApply.MaxTotalResponses)
	}
	if err := Validate(&c); err != nil {
		t.Fatalf("explicit values must validate: %v", err)
	}
}
