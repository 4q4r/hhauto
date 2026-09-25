package config

// T29: [features.autoclone] clone_original_action + the [features.autoclone.improve]
// section (per-field AI improvement of the clone).
import (
	"strings"
	"testing"
)

func baseAutoCloneConfig() *Config {
	var c Config
	applyDefaults(&c)
	c.Features.AutoClone.Enabled = true
	return &c
}

func TestValidateCloneOriginalAction(t *testing.T) {
	for _, action := range []string{"", "keep", "hide", "delete"} {
		c := baseAutoCloneConfig()
		c.Features.AutoClone.CloneOriginalAction = action
		if err := Validate(c); err != nil {
			t.Fatalf("clone_original_action %q: %v", action, err)
		}
	}
	c := baseAutoCloneConfig()
	c.Features.AutoClone.CloneOriginalAction = "explode"
	if err := Validate(c); err == nil {
		t.Fatal("unknown clone_original_action must be rejected")
	}
}

func TestDefaultCloneOriginalActionIsKeep(t *testing.T) {
	var c Config
	applyDefaults(&c)
	if c.Features.AutoClone.CloneOriginalAction != "keep" {
		t.Fatalf("default clone_original_action = %q, want keep", c.Features.AutoClone.CloneOriginalAction)
	}
}

// improve.enabled requires a model profile, a non-empty field list and the
// ai task slot; prompts must reference improve fields and never set both
// inline and file.
func TestValidateImprove(t *testing.T) {
	c := baseAutoCloneConfig()
	c.Features.AutoClone.Improve = AutoCloneImprove{
		Enabled:      true,
		Fields:       []string{"title", "about", "experience_description"},
		ModelProfile: "main",
	}
	if err := Validate(c); err != nil {
		t.Fatalf("valid improve config rejected: %v", err)
	}

	// Missing model profile.
	c.Features.AutoClone.Improve.ModelProfile = ""
	if err := Validate(c); err == nil {
		t.Fatal("improve without model_profile must be rejected")
	}

	// Empty fields.
	c.Features.AutoClone.Improve.ModelProfile = "main"
	c.Features.AutoClone.Improve.Fields = nil
	if err := Validate(c); err == nil {
		t.Fatal("improve without fields must be rejected")
	}

	// Duplicate fields.
	c.Features.AutoClone.Improve.Fields = []string{"title", "title"}
	if err := Validate(c); err == nil {
		t.Fatal("duplicate improve fields must be rejected")
	}
	c.Features.AutoClone.Improve.Fields = []string{"title", "about"}

	// inline and file are mutually exclusive.
	c.Features.AutoClone.Improve.Prompts = map[string]AITask{
		"title": {PromptInline: "x", PromptFile: "/tmp/p.md"},
	}
	if err := Validate(c); err == nil {
		t.Fatal("prompt inline+file must be rejected")
	}

	// always_ask fields must be improve fields.
	c.Features.AutoClone.Improve.Prompts = nil
	c.Features.AutoClone.Improve.AlwaysAsk = []string{"salary_not_in_fields"}
	if err := Validate(c); err == nil {
		t.Fatal("always_ask outside improve fields must be rejected")
	}
	c.Features.AutoClone.Improve.AlwaysAsk = []string{"about"}
	if err := Validate(c); err != nil {
		t.Fatalf("valid always_ask rejected: %v", err)
	}
}

// A prompt override for an unknown field is a config error (fail fast —
// silent dead overrides hide owner intent).
func TestValidateImprovePromptUnknownField(t *testing.T) {
	c := baseAutoCloneConfig()
	c.Features.AutoClone.Improve = AutoCloneImprove{
		Enabled:      true,
		Fields:       []string{"title"},
		ModelProfile: "main",
		Prompts:      map[string]AITask{"about": {PromptInline: "x"}},
	}
	err := Validate(c)
	if err == nil {
		t.Fatal("prompt override for a field not in improve.fields must be rejected")
	}
	if !strings.Contains(err.Error(), "about") {
		t.Fatalf("error must name the offending field: %v", err)
	}
}

// improve disabled: the section may stay half-filled without error.
func TestValidateImproveDisabledTolerant(t *testing.T) {
	c := baseAutoCloneConfig()
	c.Features.AutoClone.Improve = AutoCloneImprove{Fields: []string{"title"}}
	if err := Validate(c); err != nil {
		t.Fatalf("disabled improve must not be validated: %v", err)
	}
}
