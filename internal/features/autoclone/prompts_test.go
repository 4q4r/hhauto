package autoclone

// T29 per-field prompt tests: builtin defaults for every AI field, TOML
// overrides (inline | file, never both) win, date fields improve only with
// an explicit owner override prompt, and unknown prompt fields fail at
// wiring time.
import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hhauto/internal/config"
)

func improveCfg(fields ...string) config.AutoCloneImprove {
	return config.AutoCloneImprove{
		Enabled:      true,
		Fields:       fields,
		ModelProfile: "main",
	}
}

// Every builtin AI field carries a non-empty prompt that names the
// grounding contract (facts are never invented).
func TestBuiltinPromptsCoverAIAFields(t *testing.T) {
	for _, id := range []string{"title", "about", "key_skills", "salary", "experience_description"} {
		spec, ok := Spec(id)
		if !ok {
			t.Fatalf("field %q missing", id)
		}
		if !strings.Contains(spec.Prompt, "unchanged") && !strings.Contains(spec.Prompt, "Unchanged") {
			t.Fatalf("field %q prompt lacks the unchanged escape hatch", id)
		}
	}
}

// Inline override replaces the builtin; the builtin still serves the other
// fields.
func TestPromptOverrideInline(t *testing.T) {
	cfg := improveCfg("title", "about")
	cfg.Prompts = map[string]config.AITask{
		"title": {PromptInline: "Свой промпт для титула"},
	}
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	got := reg.Prompt("title")
	if got != "Свой промпт для титула" {
		t.Fatalf("title prompt = %q", got)
	}
	if got = reg.Prompt("about"); got == "" || got == "Свой промпт для титула" {
		t.Fatalf("about prompt must stay builtin, got %q", got)
	}
}

// File override reads the prompt from disk.
func TestPromptOverrideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "title.md")
	if err := os.WriteFile(path, []byte("Промпт из файла"), 0o600); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	cfg := improveCfg("title")
	cfg.Prompts = map[string]config.AITask{"title": {PromptFile: path}}
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	if reg.Prompt("title") != "Промпт из файла" {
		t.Fatalf("title prompt = %q", reg.Prompt("title"))
	}
}

// A date field without an owner override is a wiring error (dates are not
// an AI field by default — owner decision), while an override enables it.
func TestDateFieldNeedsOwnerPrompt(t *testing.T) {
	cfg := improveCfg("experience_dates")
	if _, err := NewPromptRegistry(cfg); err == nil {
		t.Fatal("experience_dates without a prompt override must fail at wiring")
	}
	if !strings.Contains(mustErrText(t, func() error { _, err := NewPromptRegistry(cfg); return err }), "experience_dates") {
		t.Fatal("error must name the offending field")
	}
	cfg.Prompts = map[string]config.AITask{"experience_dates": {PromptInline: "Сдвинь даты"}}
	if _, err := NewPromptRegistry(cfg); err != nil {
		t.Fatalf("date field with owner prompt must wire: %v", err)
	}
}

// Unknown fields and non-promptable fields are wiring errors.
func TestPromptRegistryUnknownAndStructured(t *testing.T) {
	bogus := improveCfg("title", "bogus_field")
	if err := validateImproveFields(&bogus); err == nil {
		t.Fatal("unknown improve field must fail")
	}
	structured := improveCfg("citizenship")
	if err := validateImproveFields(&structured); err == nil {
		t.Fatal("structured field must not be improvable without a prompt")
	}
	// Structured field WITH an explicit prompt is still rejected: the AI
	// cannot ground dictionary ids.
	cfg := improveCfg("citizenship")
	cfg.Prompts = map[string]config.AITask{"citizenship": {PromptInline: "x"}}
	if err := validateImproveFields(&cfg); err == nil {
		t.Fatal("structured field with a prompt override must fail")
	}
}

// inline+file exclusivity is a wiring error too (config validate also
// guards this; the feature must not depend on config running first).
func TestPromptRegistryExclusive(t *testing.T) {
	cfg := improveCfg("title")
	cfg.Prompts = map[string]config.AITask{"title": {PromptInline: "a", PromptFile: "b"}}
	if err := validateImproveFields(&cfg); err == nil {
		t.Fatal("inline+file must fail")
	}
}

func mustErrText(t *testing.T, f func() error) string {
	t.Helper()
	err := f()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}
