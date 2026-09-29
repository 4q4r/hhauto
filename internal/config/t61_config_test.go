package config

import (
	"os"
	"path/filepath"
	"testing"
)

// T61: [features.setka] defaults — a minimal file yields the documented
// owner defaults (auto mode, 30m delay, 12:00-18:00 window, 5/2/1 limits,
// require_can_message + no_links on, "нанимаю" status filter).
func TestSetkaDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hhauto.toml")
	body := `
[ai.models.fast]
provider = "glm"
[ai.providers.glm]
dialect = "openai-chat"
base_url = "https://example.invalid/v1"
api_key = "k"
[features.setka]
enabled = true
[features.setka.classify]
model_profile = "fast"
[features.setka.personalize]
model_profile = "fast"
[features.setka.endpoints]
search_people = "https://api.setka.ru/search"
get_person = "https://api.setka.ru/person"
can_message = "https://api.setka.ru/can_message"
send_dm = "https://api.setka.ru/send"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s := cfg.Features.Setka
	if !s.Enabled {
		t.Fatal("enabled must survive")
	}
	if s.Mode != "auto" {
		t.Errorf("mode = %q, want auto", s.Mode)
	}
	if s.AfterApplyDelay != "30m" {
		t.Errorf("after_apply_delay = %q, want 30m", s.AfterApplyDelay)
	}
	if s.ActiveWindow != "12:00-18:00" {
		t.Errorf("active_window = %q, want 12:00-18:00", s.ActiveWindow)
	}
	if s.MaxDMPerDay != 5 {
		t.Errorf("max_dm_per_day = %d, want 5", s.MaxDMPerDay)
	}
	if s.MaxDMPerCompany != 2 {
		t.Errorf("max_dm_per_company = %d, want 2", s.MaxDMPerCompany)
	}
	if s.MaxDMPerPerson != 1 {
		t.Errorf("max_dm_per_person = %d, want 1", s.MaxDMPerPerson)
	}
	if s.RequireCanMessage == nil || !*s.RequireCanMessage {
		t.Error("require_can_message must default true")
	}
	if s.NoLinks == nil || !*s.NoLinks {
		t.Error("no_links must default true")
	}
	if s.Classify.Borderline != "apply" {
		t.Errorf("classify.borderline = %q, want apply", s.Classify.Borderline)
	}
	if len(s.Targets.StatusFilter) != 1 || s.Targets.StatusFilter[0] != "нанимаю" {
		t.Errorf("status_filter = %v, want [нанимаю]", s.Targets.StatusFilter)
	}
}

// T61: disabled by default — the feature never wakes without an explicit
// owner switch.
func TestSetkaDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hhauto.toml")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Features.Setka.Enabled {
		t.Fatal("setka must be disabled by default")
	}
}

// T61: enabling without the captured Setka endpoints fails fast at startup —
// loud, not a silent skip (the transport waits for live reconnaissance).
func TestSetkaValidateRequiresEndpoints(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Log.RedactSecrets = true // safety-critical default (validateGlobals)
		c.Log.Format = "text"
		c.API.Transport = TransportTLSClient
		c.General.Timezone = "Local"
		c.Features.Setka = Setka{
			Enabled:         true,
			Mode:            SetkaModeAuto,
			AfterApplyDelay: "30m",
			ActiveWindow:    "12:00-18:00",
			MaxDMPerDay:     5,
			MaxDMPerCompany: 2,
			MaxDMPerPerson:  1,
			Classify:        SetkaClassify{ModelProfile: "fast", Borderline: SetkaBorderlineApply},
			Personalize:     SetkaPersonalize{ModelProfile: "fast"},
		}
		c.AI.Models = map[string]AIModel{"fast": {Provider: "p"}}
		c.AI.Providers = map[string]AIProvider{"p": {Dialect: "openai-chat", APIKey: "k"}}
		return c
	}
	if err := Validate(base()); err == nil {
		t.Fatal("expected endpoint fail-fast, got nil")
	} else if want := "не захвачен"; !contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to mention %q", err, want)
	}

	c := base()
	c.Features.Setka.Endpoints = SetkaEndpoints{
		SearchPeople: "https://api.setka.ru/x/search",
		GetPerson:    "https://api.setka.ru/x/person",
		CanMessage:   "https://api.setka.ru/x/can_message",
		SendDM:       "https://api.setka.ru/x/send",
	}
	if err := Validate(c); err != nil {
		t.Fatalf("validate with endpoints: %v", err)
	}
}

// T64: can_message is optional — the wire has no such endpoint
// (t63-setka-recon.md §4): an enabled feature validates with it empty,
// while the other three stay mandatory fail-fast and a set can_message
// must still be an absolute http(s) URL.
func TestSetkaCanMessageEndpointOptional(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hhauto.toml")
	body := `
[ai.models.fast]
provider = "glm"
[ai.providers.glm]
dialect = "openai-chat"
base_url = "https://example.invalid/v1"
api_key = "k"
[features.setka]
enabled = true
[features.setka.classify]
model_profile = "fast"
[features.setka.personalize]
model_profile = "fast"
[features.setka.endpoints]
search_people = "https://api.setka.ru/search"
get_person = "https://api.setka.ru/person"
send_dm = "https://api.setka.ru/send"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load without can_message must succeed: %v", err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("validate without can_message: %v", err)
	}
	// A set but broken can_message stays loud.
	cfg.Features.Setka.Endpoints.CanMessage = "not-a-url"
	if err := Validate(cfg); err == nil || !contains(err.Error(), "can_message") {
		t.Fatalf("broken can_message URL: %v", err)
	}
}

// T61: mode, borderline, delay, window and negative limits are validated.
func TestSetkaValidateValues(t *testing.T) {
	ok := func(c *Config) *Config {
		c.Features.Setka.Endpoints = SetkaEndpoints{
			SearchPeople: "https://api.setka.ru/s", GetPerson: "https://api.setka.ru/p",
			CanMessage: "https://api.setka.ru/c", SendDM: "https://api.setka.ru/d",
		}
		return c
	}
	base := func() *Config {
		c := &Config{}
		c.Log.RedactSecrets = true // safety-critical default (validateGlobals)
		c.Log.Format = "text"
		c.API.Transport = TransportTLSClient
		c.General.Timezone = "Local"
		c.Features.Setka = Setka{
			Enabled: true, Mode: SetkaModeAuto, AfterApplyDelay: "30m",
			ActiveWindow: "12:00-18:00", MaxDMPerDay: 5, MaxDMPerCompany: 2, MaxDMPerPerson: 1,
			Classify:    SetkaClassify{ModelProfile: "fast", Borderline: SetkaBorderlineApply},
			Personalize: SetkaPersonalize{ModelProfile: "fast"},
		}
		c.AI.Models = map[string]AIModel{"fast": {Provider: "p"}}
		c.AI.Providers = map[string]AIProvider{"p": {Dialect: "openai-chat", APIKey: "k"}}
		return c
	}

	bad := base()
	bad.Features.Setka.Mode = "draft"
	if err := Validate(ok(bad)); err == nil || !contains(err.Error(), "mode") {
		t.Fatalf("mode validation: %v", err)
	}

	bad = base()
	bad.Features.Setka.Classify.Borderline = "maybe"
	if err := Validate(ok(bad)); err == nil || !contains(err.Error(), "borderline") {
		t.Fatalf("borderline validation: %v", err)
	}

	bad = base()
	bad.Features.Setka.AfterApplyDelay = "soon"
	if err := Validate(ok(bad)); err == nil || !contains(err.Error(), "after_apply_delay") {
		t.Fatalf("delay validation: %v", err)
	}

	bad = base()
	bad.Features.Setka.ActiveWindow = "25:00-99:99"
	if err := Validate(ok(bad)); err == nil || !contains(err.Error(), "active_window") {
		t.Fatalf("window validation: %v", err)
	}

	bad = base()
	bad.Features.Setka.MaxDMPerDay = -1
	if err := Validate(ok(bad)); err == nil || !contains(err.Error(), "max_dm_per_day") {
		t.Fatalf("day limit validation: %v", err)
	}

	bad = base()
	bad.Features.Setka.Classify.ModelProfile = ""
	if err := Validate(ok(bad)); err == nil || !contains(err.Error(), "classify.model_profile") {
		t.Fatalf("classify profile validation: %v", err)
	}

	bad = base()
	bad.Features.Setka.Personalize.ModelProfile = "ghost"
	if err := Validate(ok(bad)); err == nil || !contains(err.Error(), "personalize.model_profile") {
		t.Fatalf("personalize profile validation: %v", err)
	}
}

// T61: a disabled section skips validation entirely (hand-built configs).
func TestSetkaValidateSkipsDisabled(t *testing.T) {
	c := &Config{}
	c.Log.RedactSecrets = true
	c.Log.Format = "text"
	c.API.Transport = TransportTLSClient
	c.General.Timezone = "Local"
	c.Features.Setka = Setka{Enabled: false, Mode: "nonsense"}
	if err := Validate(c); err != nil {
		t.Fatalf("disabled section must not validate: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
