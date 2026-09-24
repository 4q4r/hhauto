package config

import (
	"reflect"
	"testing"
)

func TestApplyDefaultsEmptyConfig(t *testing.T) {
	var c Config
	applyDefaults(&c)

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"general.timezone", c.General.Timezone, "Local"},
		{"storage.busy_timeout_ms", c.Storage.BusyTimeoutMS, 5000},
		{"log.level", c.Log.Level, "info"},
		{"log.format", c.Log.Format, "text"},
		{"log.redact_secrets", c.Log.RedactSecrets, true},
		{"api.kind", c.API.Kind, "web"},
		{"api.endpoints.site", c.API.Endpoints.Site, "https://hh.ru"},
		{"api.endpoints.chatik", c.API.Endpoints.Chatik, "https://chatik.hh.ru"},
		{"api.endpoints.search_page", c.API.Endpoints.SearchPage, "https://hh.ru/search/vacancy"},
		{"features.auto_lift.per_day", c.Features.AutoLift.PerDay, 6},
		{"features.auto_lift.respect_4h_timer", *c.Features.AutoLift.Respect4hTimer, true},
		{"pacing.active_window", c.Pacing.ActiveWindow, "08:00-22:00"},
		{"pacing.mode", c.Pacing.Mode, "adaptive"},
		{"pacing.night_pause", *c.Pacing.NightPause, true},
		{"review.sla_hours_default", c.Review.SLADefaultHours, 24},
		{"forms.mode", c.Forms.Mode, "draft_only"},
		{"forms.submit_policy", c.Forms.SubmitPolicy, "human"},
		{"forms.abort_on_password_field", c.Forms.AbortOnPasswordField, true},
		{"email.per_employer_once", *c.Email.PerEmployerOnce, true},
		{"warmup.silence_hours", c.Warmup.SilenceHours, 24},
		{"warmup.max_pings", c.Warmup.MaxPings, 3},
		{"schedule.missed_run_policy", c.Schedule.MissedRunPolicy, "run_once"},
		{"schedule.max_catchup_hours", c.Schedule.MaxCatchupHours, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("applyDefaults: got %v, want %v", tt.got, tt.want)
			}
		})
	}

	wantDomains := []string{"hh.ru", "forms.gle", "docs.google.com", "forms.yandex.ru"}
	if got := c.Forms.AllowedDomains; !reflect.DeepEqual(got, wantDomains) {
		t.Fatalf("forms.allowed_domains: got %v, want %v", got, wantDomains)
	}
	wantLadder := []int{24, 48, 96}
	if got := c.Warmup.LadderHours; !reflect.DeepEqual(got, wantLadder) {
		t.Fatalf("warmup.ladder_hours: got %v, want %v", got, wantLadder)
	}
}

func TestApplyDefaultsPreservesExplicitAccountValues(t *testing.T) {
	c := &Config{Accounts: map[string]Account{
		"sparse": {},
		"custom": {DailyApplyCap: 50, Concurrency: 4, RPS: 1.5},
	}}
	applyDefaults(c)

	a := c.Accounts["sparse"]
	if a.DailyApplyCap != 200 {
		t.Errorf("sparse.daily_apply_cap: got %d, want 200", a.DailyApplyCap)
	}
	if a.Concurrency != 2 {
		t.Errorf("sparse.concurrency: got %d, want 2", a.Concurrency)
	}
	if a.RPS != 0.5 {
		t.Errorf("sparse.rps: got %v, want 0.5", a.RPS)
	}

	b := c.Accounts["custom"]
	if b.DailyApplyCap != 50 {
		t.Errorf("custom.daily_apply_cap: got %d, want explicit 50", b.DailyApplyCap)
	}
	if b.Concurrency != 4 {
		t.Errorf("custom.concurrency: got %d, want explicit 4", b.Concurrency)
	}
	if b.RPS != 1.5 {
		t.Errorf("custom.rps: got %v, want explicit 1.5", b.RPS)
	}
}

// Owner decision #6: auto-reply defaults to ON ("everything is sent
// immediately"); applyDefaults materializes the default for a minimal TOML.
func TestApplyDefaultsChatOpsAutoReplyOn(t *testing.T) {
	c := &Config{}
	applyDefaults(c)
	if c.Features.ChatOps.AutoReply == nil || !*c.Features.ChatOps.AutoReply {
		t.Fatal("features.chatops.auto_reply_enabled: got false, want default true (owner decision #6)")
	}
}

// R1 pattern: an explicit false must survive applyDefaults (the old plain
// bool was overwritten with true unconditionally — a dead toggle).
func TestApplyDefaultsRespectsExplicitAutoReplyFalse(t *testing.T) {
	v := false
	c := &Config{Features: Features{ChatOps: ChatOps{AutoReply: &v}}}
	applyDefaults(c)
	if c.Features.ChatOps.AutoReply == nil || *c.Features.ChatOps.AutoReply {
		t.Fatal("explicit auto_reply_enabled=false was overwritten with the default true")
	}
}
