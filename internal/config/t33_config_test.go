package config

// T33: [ai.jev] — optional Jev (System One) prefilter: defaults, validation
// and enabled-state rules (endpoint URL, api_key_env, task allow-list,
// confidence threshold).
import (
	"strings"
	"testing"
)

func TestJevDefaults(t *testing.T) {
	var c Config
	applyDefaults(&c)
	j := c.AI.Jev
	if j.Enabled {
		t.Error("ai.jev.enabled must default to false (full-AI mode)")
	}
	if j.Endpoint != "https://api.typesafe.ai/v1/systemone" {
		t.Errorf("default endpoint = %q", j.Endpoint)
	}
	if j.ConfidenceThreshold != 0.85 {
		t.Errorf("default confidence_threshold = %v, want 0.85", j.ConfidenceThreshold)
	}
	if len(j.Tasks) != 0 {
		t.Errorf("default tasks = %v, want empty", j.Tasks)
	}
}

func TestValidateJev(t *testing.T) {
	c := validConfig()
	if err := Validate(c); err != nil {
		t.Fatalf("zero-value [ai.jev] (disabled) must validate: %v", err)
	}

	c = validConfig()
	c.AI.Jev = AIJev{
		Enabled:             true,
		Endpoint:            "https://jev.example.com/v1/systemone",
		APIKeyEnv:           "JEV_API_KEY",
		ConfidenceThreshold: 0.9,
		Tasks:               []string{"message_classification", "link_detection", "contact_extraction"},
	}
	if err := Validate(c); err != nil {
		t.Fatalf("enabled [ai.jev] happy path must validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*AIJev)
		want string
	}{
		{
			name: "enabled without api_key_env",
			mut:  func(j *AIJev) { j.APIKeyEnv = "" },
			want: "api_key_env",
		},
		{
			name: "enabled without endpoint",
			mut:  func(j *AIJev) { j.Endpoint = "" },
			want: "ai.jev.endpoint",
		},
		{
			name: "ftp endpoint",
			mut:  func(j *AIJev) { j.Endpoint = "ftp://jev.example.com" },
			want: "absolute http(s) URL",
		},
		{
			name: "hostless endpoint",
			mut:  func(j *AIJev) { j.Endpoint = "not a url" },
			want: "absolute http(s) URL",
		},
		{
			name: "unknown task",
			mut:  func(j *AIJev) { j.Tasks = []string{"message_classification", "vacancy_scoring"} },
			want: "not a jev-compatible task",
		},
		{
			name: "no tasks",
			mut:  func(j *AIJev) { j.Tasks = nil },
			want: "at least one task",
		},
		{
			name: "threshold above 1",
			mut:  func(j *AIJev) { j.ConfidenceThreshold = 1.5 },
			want: "(0,1]",
		},
		{
			name: "threshold 0",
			mut:  func(j *AIJev) { j.ConfidenceThreshold = 0 },
			want: "(0,1]",
		},
		{
			name: "negative threshold",
			mut:  func(j *AIJev) { j.ConfidenceThreshold = -0.1 },
			want: "(0,1]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.AI.Jev = AIJev{
				Enabled:             true,
				Endpoint:            "https://jev.example.com/v1/systemone",
				APIKeyEnv:           "JEV_API_KEY",
				ConfidenceThreshold: 0.9,
				Tasks:               []string{"message_classification"},
			}
			tc.mut(&c.AI.Jev)
			err := Validate(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want containing %q", err, tc.want)
			}
		})
	}

	// Disabled sections stay permissive: a task typo is still reported,
	// but key/threshold requirements do not apply.
	c = validConfig()
	c.AI.Jev = AIJev{Endpoint: "https://jev.example.com", Tasks: []string{"bogus"}}
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "not a jev-compatible task") {
		t.Fatalf("disabled section with unknown task = %v", err)
	}
	c = validConfig()
	c.AI.Jev = AIJev{ConfidenceThreshold: 0} // 0 threshold is fine while disabled
	if err := Validate(c); err != nil {
		t.Fatalf("disabled section with zero threshold must validate: %v", err)
	}
}
