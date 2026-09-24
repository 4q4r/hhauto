package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"bare tilde", "~", home},
		{"tilde slash", "~/hhauto/data.db", home + "/hhauto/data.db"},
		{"absolute untouched", "/var/lib/hhauto", "/var/lib/hhauto"},
		{"relative untouched", "data/state.db", "data/state.db"},
		{"mid-string tilde untouched", "weird~name", "weird~name"},
		{"user tilde unsupported", "~other/x", "~other/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandPath(tt.in); got != tt.want {
				t.Fatalf("ExpandPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoadFixture(t *testing.T) {
	t.Setenv("HHAUTO_TEST_WEBHOOK_TOKEN", "tok123")
	t.Setenv("HHAUTO_TEST_RESUME_ID", "res-42")

	cfg, err := Load("testdata/config.toml")
	if err != nil {
		t.Fatalf("Load(testdata/config.toml) = %v, want nil", err)
	}

	// {data_dir} substitution, then ~ expansion, in storage.path.
	wantStorage := ExpandPath("~/hhauto-data") + "/state.db"
	if cfg.Storage.Path != wantStorage {
		t.Errorf("storage.path = %q, want %q", cfg.Storage.Path, wantStorage)
	}
	if cfg.Storage.BusyTimeoutMS != 3000 {
		t.Errorf("storage.busy_timeout_ms = %d, want 3000", cfg.Storage.BusyTimeoutMS)
	}

	// {data_dir} in account cookie_file.
	wantCookie := ExpandPath("~/hhauto-data") + "/cookies/main.json"
	if got := cfg.Accounts["main"].CookieFile; got != wantCookie {
		t.Errorf("accounts.main.cookie_file = %q, want %q", got, wantCookie)
	}

	// Leading ~ directly in an account field.
	wantDirect := ExpandPath("~/cookies/direct.json")
	if got := cfg.Accounts["direct"].CookieFile; got != wantDirect {
		t.Errorf("accounts.direct.cookie_file = %q, want %q", got, wantDirect)
	}

	// ${ENV} substitution.
	if got := cfg.Webhook["tg1"].URL; !strings.Contains(got, "bot"+`tok123`+"/sendMessage") {
		t.Errorf("webhook.tg1.url = %q, want substituted token tok123", got)
	}
	if got := cfg.Runs["backend"].ResumeID; got != "res-42" {
		t.Errorf("runs.backend.resume_id = %q, want res-42", got)
	}

	// Structural spot checks.
	if got := cfg.API.Endpoints.Override["vacancy_search"]; got != "https://custom.example/vacancies" {
		t.Errorf("api.endpoints.override[vacancy_search] = %q, want https://custom.example/vacancies", got)
	}
	if got := cfg.Accounts["direct"].DailyApplyCap; got != 50 {
		t.Errorf("accounts.direct.daily_apply_cap = %d, want explicit 50", got)
	}
	if got := cfg.Accounts["main"].Headers["X-Test"]; got != "1" {
		t.Errorf("accounts.main.headers[X-Test] = %q, want 1", got)
	}
	if len(cfg.Runs["backend"].Windows) != 1 || len(cfg.Runs["backend"].Windows[0].Time) != 2 {
		t.Fatalf("runs.backend.windows not decoded: %+v", cfg.Runs["backend"].Windows)
	}
}

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.toml")
	minimal := "[general]\ndata_dir = \"" + dir + "\"\n"
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(minimal) = %v, want nil", err)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" || !cfg.Log.RedactSecrets {
		t.Errorf("log defaults not applied: %+v", cfg.Log)
	}
	if cfg.Storage.BusyTimeoutMS != 5000 {
		t.Errorf("storage.busy_timeout_ms = %d, want 5000", cfg.Storage.BusyTimeoutMS)
	}
	if cfg.General.Timezone != "Local" {
		t.Errorf("general.timezone = %q, want Local", cfg.General.Timezone)
	}
	if cfg.Pacing.ActiveWindow != "08:00-22:00" || cfg.Pacing.Mode != "adaptive" || !cfg.Pacing.NightPause {
		t.Errorf("pacing defaults not applied: %+v", cfg.Pacing)
	}
	if len(cfg.Forms.AllowedDomains) != 4 {
		t.Errorf("forms.allowed_domains = %v, want 4 default domains", cfg.Forms.AllowedDomains)
	}
	if cfg.Schedule.MissedRunPolicy != "run_once" || cfg.Schedule.MaxCatchupHours != 6 {
		t.Errorf("schedule defaults not applied: %+v", cfg.Schedule)
	}
}

func TestLoadUnsetEnvVariableIsError(t *testing.T) {
	t.Setenv("HHAUTO_TEST_PRESENT_VAR", "ok")
	dir := t.TempDir()
	path := filepath.Join(dir, "env.toml")
	body := "[general]\ndata_dir = \"${HHAUTO_TEST_PRESENT_VAR}\"\n[storage]\npath = \"${HHAUTO_TEST_MISSING_VAR_ABC}\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want unset-variable error")
	}
	if !strings.Contains(err.Error(), "HHAUTO_TEST_MISSING_VAR_ABC") {
		t.Fatalf("Load() error = %q, want it to name the unset variable", err.Error())
	}
}

func TestLoadUnknownProxyRefIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "badref.toml")
	body := `
[general]
data_dir = "` + dir + `"
[accounts.main]
cookie_file = "` + dir + `/c.json"
proxy_ref = "ghost"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want unknown-proxy error")
	}
	if !strings.Contains(err.Error(), "unknown proxy") {
		t.Fatalf("Load() error = %q, want containing %q", err.Error(), "unknown proxy")
	}
}

func TestLoadInvalidTOMLIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.toml")
	if err := os.WriteFile(path, []byte("not [ valid toml ==="), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() = nil error, want parse error")
	}
}

func TestLoadMissingFileIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("Load(missing) = nil error, want error")
	}
}
