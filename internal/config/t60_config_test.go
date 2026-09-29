package config

// T60: one classifier instead of the cascade + the configurable cover
// letter. [features.multiapply].classifier picks THE classification stage
// (owner decision: jev OR llm, never one after another); the
// [features.multiapply.cover_letter] section switches the letter pipeline.
import (
	"strings"
	"testing"
)

func TestClassifierDefaultIsLLM(t *testing.T) {
	var c Config
	applyDefaults(&c)
	if c.Features.MultiApply.Classifier != "" {
		t.Errorf("classifier default = %q, want unset (\"\" behaves as llm)", c.Features.MultiApply.Classifier)
	}
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("unset classifier with disabled [ai.jev] must validate (llm default): %v", err)
	}
}

func TestClassifierExclusive(t *testing.T) {
	jevOn := func(c *Config) {
		c.AI.Jev = AIJev{
			Enabled:             true,
			Endpoint:            "https://jev.example.com/v1/systemone",
			APIKeyEnv:           "JEV_API_KEY",
			ConfidenceThreshold: 0.9,
			Tasks:               []string{"message_classification"},
		}
	}

	// jev chosen, configured: valid.
	c := validConfig()
	jevOn(c)
	c.Features.MultiApply.Classifier = ClassifierJev
	if err := Validate(c); err != nil {
		t.Fatalf("classifier=jev with [ai.jev] enabled must validate: %v", err)
	}

	// llm chosen, jev disabled: valid (the default install).
	c = validConfig()
	c.Features.MultiApply.Classifier = ClassifierLLM
	if err := Validate(c); err != nil {
		t.Fatalf("classifier=llm with disabled [ai.jev] must validate: %v", err)
	}

	for _, tc := range []struct {
		name       string
		classifier string
		jev        bool
		want       string
	}{
		{
			name:       "both configured: llm choice with [ai.jev] enabled",
			classifier: ClassifierLLM,
			jev:        true,
			want:       "exactly one classifier",
		},
		{
			name:       "both configured: no choice with [ai.jev] enabled",
			classifier: "",
			jev:        true,
			want:       "exactly one classifier",
		},
		{
			name:       "jev chosen but not configured",
			classifier: ClassifierJev,
			jev:        false,
			want:       "ai.jev.enabled",
		},
		{
			name:       "unknown classifier value",
			classifier: "gpt",
			jev:        false,
			want:       "must be one of",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			if tc.jev {
				jevOn(c)
			}
			c.Features.MultiApply.Classifier = tc.classifier
			err := Validate(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCoverLetterSectionDefaultsInert(t *testing.T) {
	var c Config
	applyDefaults(&c)
	if c.Features.MultiApply.CoverLetter.Enabled {
		t.Error("cover_letter.enabled must default to false (section inert = current behavior)")
	}
	if c.Features.MultiApply.CoverLetter.Mode != "" {
		t.Errorf("cover_letter.mode default = %q, want unset (\"\" behaves as ai)", c.Features.MultiApply.CoverLetter.Mode)
	}
}

func TestValidateCoverLetter(t *testing.T) {
	enabled := func(cl *MultiApplyCoverLetter) {
		cl.Enabled = true
	}

	// ai mode with an empty template: valid (current behavior).
	c := validConfig()
	enabled(&c.Features.MultiApply.CoverLetter)
	if err := Validate(c); err != nil {
		t.Fatalf("enabled cover_letter in ai mode must validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*MultiApplyCoverLetter)
		want string
	}{
		{
			name: "unknown mode",
			mut:  func(cl *MultiApplyCoverLetter) { cl.Mode = "magic" },
			want: "must be one of",
		},
		{
			name: "template mode without template",
			mut:  func(cl *MultiApplyCoverLetter) { cl.Mode = CoverLetterModeTemplate },
			want: "required when mode = template",
		},
		{
			name: "unknown model profile",
			mut: func(cl *MultiApplyCoverLetter) {
				cl.ModelProfile = "nope"
			},
			want: "unknown model",
		},
		{
			name: "model profile without the letter task",
			mut: func(cl *MultiApplyCoverLetter) {
				cl.ModelProfile = ""
			},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			cc := &c.Features.MultiApply.CoverLetter
			enabled(cc)
			tc.mut(cc)
			err := Validate(c)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want containing %q", err, tc.want)
			}
		})
	}

	// model_profile must reference a configured model AND the letter task
	// that would use it.
	c = validConfig()
	c.AI.Models = map[string]AIModel{"letter-model": {Provider: "p", Model: "m"}}
	c.AI.Providers = map[string]AIProvider{"p": {Dialect: "openai-chat", APIKeyEnv: "K"}}
	cl := &c.Features.MultiApply.CoverLetter
	enabled(cl)
	cl.ModelProfile = "letter-model"
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "ai.tasks.cover_letter") {
		t.Fatalf("model_profile without ai.tasks.cover_letter = %v, want task-required error", err)
	}
	c.AI.Tasks = map[string]AITask{"cover_letter": {ModelProfile: "letter-model"}}
	if err := Validate(c); err != nil {
		t.Fatalf("model_profile with the letter task must validate: %v", err)
	}

	// A disabled section may stay half-filled (existing convention).
	c = validConfig()
	c.Features.MultiApply.CoverLetter = MultiApplyCoverLetter{
		Mode:         "magic",
		ModelProfile: "nope",
	}
	if err := Validate(c); err != nil {
		t.Fatalf("disabled cover_letter section must not be validated: %v", err)
	}
}
