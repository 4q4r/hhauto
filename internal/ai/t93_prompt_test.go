package ai

// T93 (а): the resume_improve base prompt carries the positioning-
// inheritance anchor and the family-preserving few-shot (RC-3), and the
// version bump invalidates the prompt-ver cache key.

import (
	"strings"
	"testing"

	"hhauto/internal/aiprompts"
	"hhauto/internal/config"
)

func TestResumeImprovePromptV6PositioningAnchor(t *testing.T) {
	reg, err := NewPromptRegistry(map[string]config.AITask{"resume_improve": {}}, aiprompts.FS)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	body, version, err := reg.SystemPrompt(TaskResumeImprove)
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}
	if version != "v6" {
		t.Fatalf("prompt version = %q, want v6 (the cache-ver bump)", version)
	}
	for _, want := range []string{
		"POSITIONING INHERITANCE",
		"Позиционирование семьи",
		"DevOps-инженер (CI/CD, Docker, PostgreSQL)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("prompt body misses %q", want)
		}
	}
	// RC-3: the backend-biased few-shot is gone — a devops-family resume
	// must not be shown a Backend-titled "valid example".
	if strings.Contains(body, `"value_string": "Backend-разработчик (Go, Python, PostgreSQL)"`) {
		t.Fatal("the backend-biased few-shot is still there")
	}
}
