package config

import (
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"
)

// t69Console parses a [console] snippet on a valid config and runs the
// default+validate chain (the load path the daemon uses).
func t69Console(t *testing.T, snippet string) *Config {
	t.Helper()
	c := validConfig()
	if err := toml.Unmarshal([]byte(snippet), &c.Console); err != nil {
		t.Fatalf("decode: %v", err)
	}
	applyDefaults(c)
	return c
}

// T69: [console] defaults — line types on, verdicts off, skips
// interesting+aggregate.
func TestConsoleDefaultsT69(t *testing.T) {
	var c Config
	applyDefaults(&c)
	for name, b := range map[string]*bool{
		"applied": c.Console.Applied, "errors": c.Console.Errors,
		"letters": c.Console.Letters, "lift": c.Console.Lift,
	} {
		if b == nil || !*b {
			t.Fatalf("console.%s default = %v, want true", name, b)
		}
	}
	if c.Console.Verdicts {
		t.Fatal("console.verdicts default = true, want false")
	}
	if c.Console.Skips != "interesting,aggregate" {
		t.Fatalf("console.skips default = %q", c.Console.Skips)
	}
}

// T69: explicit values survive the defaults.
func TestConsoleExplicitValuesT69(t *testing.T) {
	c := t69Console(t, `
applied = false
verdicts = true
skips = "all"
`)
	if err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Console.Applied == nil || *c.Console.Applied {
		t.Fatal("console.applied = true, want the explicit false")
	}
	if !c.Console.Verdicts {
		t.Fatal("console.verdicts = false, want true")
	}
	if c.Console.Skips != "all" {
		t.Fatalf("console.skips = %q", c.Console.Skips)
	}
}

// T69: skips mode vocabulary; none stands alone.
func TestConsoleSkipsValidationT69(t *testing.T) {
	for _, modes := range []string{"none", "all", "interesting", "aggregate", "interesting,aggregate", "aggregate,interesting"} {
		if err := Validate(t69Console(t, "skips = \""+modes+"\"\n")); err != nil {
			t.Fatalf("skips %q rejected: %v", modes, err)
		}
	}
	for _, modes := range []string{"sometimes", "none,all", "none,interesting"} {
		err := Validate(t69Console(t, "skips = \""+modes+"\"\n"))
		if err == nil {
			t.Fatalf("skips %q accepted", modes)
		}
		if !strings.Contains(err.Error(), "console.skips") {
			t.Fatalf("skips %q error misses the config path: %v", modes, err)
		}
	}
}

// T69: template overrides must parse as Go text/template.
func TestConsoleTemplateValidationT69(t *testing.T) {
	c := t69Console(t, "template_applied = \"✓ {{.VacancyTitle}}\"\n")
	if err := Validate(c); err != nil {
		t.Fatalf("valid template rejected: %v", err)
	}
	err := Validate(t69Console(t, "template_skip = \"{{if .X}\"\n"))
	if err == nil {
		t.Fatal("malformed template accepted")
	}
	if !strings.Contains(err.Error(), "console.template_skip") {
		t.Fatalf("template error misses the config path: %v", err)
	}
}

// SkipModes splits the validated field into the mode set.
func TestConsoleSkipModesT69(t *testing.T) {
	modes := (Console{Skips: "interesting,aggregate"}).SkipModes()
	if !modes["interesting"] || !modes["aggregate"] || modes["none"] {
		t.Fatalf("SkipModes = %v", modes)
	}
}
