package config

// T46: [ai.jev].jev_only_tasks — tasks where the Jev verdict is FINAL (no
// LLM fallback: undecided outcomes route to human review). Validation: the
// allowed values mirror ai.jev.tasks and every entry must be enrolled; the
// list is optional — empty means full-AI escalation preserved (no default
// fill, so an unset key keeps the T33 behavior).

import (
	"strings"
	"testing"
)

func TestT46JevOnlyTasksValidation(t *testing.T) {
	// Empty/unset list is valid in every state and must not be auto-filled.
	var zero Config
	applyDefaults(&zero)
	if len(zero.AI.Jev.JevOnlyTasks) != 0 {
		t.Errorf("applyDefaults filled jev_only_tasks = %v, want empty (empty = full-AI)", zero.AI.Jev.JevOnlyTasks)
	}

	c := validConfig()
	c.AI.Jev = AIJev{
		Enabled:             true,
		Endpoint:            "https://jev.example.com/v1/systemone",
		APIKeyEnv:           "JEV_API_KEY",
		ConfidenceThreshold: 0.9,
		Tasks:               []string{"message_classification", "link_detection"},
	}
	if err := Validate(c); err != nil {
		t.Fatalf("jev_only_tasks unset must validate: %v", err)
	}

	c.AI.Jev.JevOnlyTasks = []string{"message_classification"}
	if err := Validate(c); err != nil {
		t.Fatalf("enrolled jev_only subset must validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*AIJev)
		want string
	}{
		{
			name: "unknown task",
			mut:  func(j *AIJev) { j.JevOnlyTasks = []string{"vacancy_scoring"} },
			want: "not a jev-compatible task",
		},
		{
			name: "typo task",
			mut:  func(j *AIJev) { j.JevOnlyTasks = []string{"message_classifcation"} },
			want: "not a jev-compatible task",
		},
		{
			name: "not enrolled in ai.jev.tasks",
			mut: func(j *AIJev) {
				j.JevOnlyTasks = []string{"message_classification", "contact_extraction"}
			},
			want: "not enrolled in ai.jev.tasks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.AI.Jev = AIJev{
				Enabled:             true,
				Endpoint:            "https://jev.example.com/v1/systemone",
				APIKeyEnv:           "JEV_API_KEY",
				ConfidenceThreshold: 0.9,
				Tasks:               []string{"message_classification", "link_detection"},
			}
			tc.mut(&c.AI.Jev)
			err := Validate(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want containing %q", err, tc.want)
			}
		})
	}

	// The enrollment rule applies while disabled too: the option would be a
	// silent no-op the day the prefilter is switched on.
	c = validConfig()
	c.AI.Jev = AIJev{
		Endpoint:     "https://jev.example.com/v1/systemone",
		Tasks:        []string{"message_classification"},
		JevOnlyTasks: []string{"link_detection"},
	}
	if err := Validate(c); err == nil || !strings.Contains(err.Error(), "not enrolled in ai.jev.tasks") {
		t.Fatalf("disabled section with unenrolled jev_only task = %v", err)
	}
}
