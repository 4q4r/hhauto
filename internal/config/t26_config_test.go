package config

import (
	"strings"
	"testing"
)

// T26: letter_mode on runs and the [features.autoclone] section.

func TestApplyDefaultsT26(t *testing.T) {
	var c Config
	applyDefaults(&c)

	// Defaults apply to a real run entry.
	c.Runs = map[string]Run{"r1": {Enabled: true, Account: "acc1", ResumeID: "h", MaxPerDay: 5}}
	applyDefaults(&c)
	if c.Runs["r1"].LetterMode != LetterModeAICompany {
		t.Fatalf("runs.r1.letter_mode = %q, want %q", c.Runs["r1"].LetterMode, LetterModeAICompany)
	}
	if c.Features.AutoClone.MaxClonesPerResume != 2 {
		t.Fatalf("features.autoclone.max_clones_per_resume = %d, want 2", c.Features.AutoClone.MaxClonesPerResume)
	}
	if c.Features.AutoClone.MaxCloneDepth != 1 {
		t.Fatalf("features.autoclone.max_clone_depth = %d, want 1", c.Features.AutoClone.MaxCloneDepth)
	}
	if c.Features.AutoClone.ReapplyMode != ReapplyNewOnly {
		t.Fatalf("features.autoclone.reapply_mode = %q, want %q", c.Features.AutoClone.ReapplyMode, ReapplyNewOnly)
	}
	if c.Features.AutoClone.SkipRejectionDays != 30 {
		t.Fatalf("features.autoclone.skip_rejection_days = %d, want 30", c.Features.AutoClone.SkipRejectionDays)
	}
	if c.Features.AutoClone.FinishTimeoutMin != 10 {
		t.Fatalf("features.autoclone.finish_timeout_min = %d, want 10", c.Features.AutoClone.FinishTimeoutMin)
	}
	if c.Features.AutoClone.Autolift == nil || !*c.Features.AutoClone.Autolift {
		t.Fatalf("features.autoclone.autolift = nil/false, want default true")
	}
}

func TestValidateLetterModeEnum(t *testing.T) {
	base := func() *Config {
		c := Config{}
		applyDefaults(&c)
		c.Log.RedactSecrets = true
		c.Accounts = map[string]Account{"acc1": {UserAgent: "ua"}}
		c.Runs = map[string]Run{"r1": {Enabled: true, Account: "acc1", ResumeID: "h", MaxPerDay: 5}}
		return &c
	}
	for _, mode := range []string{LetterModeTemplate, LetterModeTemplateAI, LetterModeAICompany} {
		c := base()
		run := c.Runs["r1"]
		run.LetterMode = mode
		run.LetterTemplate = "шаблон" // required by template* modes
		c.Runs["r1"] = run
		if err := Validate(c); err != nil {
			t.Fatalf("letter_mode %q rejected: %v", mode, err)
		}
	}
	c := base()
	run := c.Runs["r1"]
	run.LetterMode = "gpt_magic"
	c.Runs["r1"] = run
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "letter_mode") {
		t.Fatalf("err = %v, want letter_mode enum rejection", err)
	}

	// template / template_ai are meaningless without a template: reject at
	// startup instead of silently sending AI letters or applying bare.
	for _, mode := range []string{LetterModeTemplate, LetterModeTemplateAI} {
		c := base()
		run := c.Runs["r1"]
		run.LetterMode = mode
		c.Runs["r1"] = run
		if err := Validate(c); err == nil || !strings.Contains(err.Error(), "letter_template") {
			t.Fatalf("letter_mode %q without letter_template: err = %v, want rejection", mode, err)
		}
		run.LetterTemplate = "шаблон"
		c.Runs["r1"] = run
		if err := Validate(c); err != nil {
			t.Fatalf("letter_mode %q with template rejected: %v", mode, err)
		}
	}

	// The clone-run override carries the same enum.
	c = base()
	c.Features.AutoClone.Enabled = true
	c.Features.AutoClone.RunOverride.LetterMode = "bogus"
	err = Validate(c)
	if err == nil || !strings.Contains(err.Error(), "run_override") {
		t.Fatalf("err = %v, want run_override.letter_mode rejection", err)
	}
}

func TestValidateAutoClone(t *testing.T) {
	base := func() *Config {
		c := Config{}
		applyDefaults(&c)
		c.Log.RedactSecrets = true
		c.Accounts = map[string]Account{"acc1": {UserAgent: "ua"}}
		c.Runs = map[string]Run{"r1": {Enabled: true, Account: "acc1", ResumeID: "h", MaxPerDay: 5}}
		c.Features.AutoClone.Enabled = true
		return &c
	}
	if err := Validate(base()); err != nil {
		t.Fatalf("valid autoclone config rejected: %v", err)
	}
	for _, mode := range []string{ReapplyAll, ReapplyNewOnly, ReapplySkipRecentRejections} {
		c := base()
		c.Features.AutoClone.ReapplyMode = mode
		if err := Validate(c); err != nil {
			t.Fatalf("reapply_mode %q rejected: %v", mode, err)
		}
	}
	c := base()
	c.Features.AutoClone.ReapplyMode = "sometimes"
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "reapply_mode") {
		t.Fatalf("err = %v, want reapply_mode enum rejection", err)
	}
	c = base()
	c.Features.AutoClone.MaxClonesPerResume = -1
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "max_clones_per_resume") {
		t.Fatalf("err = %v, want negative max_clones_per_resume rejection", err)
	}
	c = base()
	c.Features.AutoClone.MaxCloneDepth = -1
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "max_clone_depth") {
		t.Fatalf("err = %v, want negative max_clone_depth rejection", err)
	}
	c = base()
	c.Features.AutoClone.SkipRejectionDays = -5
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "skip_rejection_days") {
		t.Fatalf("err = %v, want negative skip_rejection_days rejection", err)
	}
}
