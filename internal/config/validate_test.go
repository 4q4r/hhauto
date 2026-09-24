package config

import (
	"fmt"
	"strings"
	"testing"
)

// validConfig returns a minimal config that passes validation.
func validConfig() *Config {
	return &Config{
		General: General{Timezone: "Local"},
		Log:     Log{Level: "info", Format: "text", RedactSecrets: true},
		Captcha: map[string]CaptchaProvider{
			"cap1": {Protocol: "2captcha"},
		},
		Proxy: map[string]Proxy{
			"p1": {Type: "socks5", Host: "10.0.0.1", Port: 1080, PassEnv: "P1_PASS"},
		},
		Accounts: map[string]Account{
			"main": {ProxyRef: "p1", CaptchaChain: []string{"cap1"}, DailyApplyCap: 200, Concurrency: 2, RPS: 0.5},
		},
		Runs: map[string]Run{
			"daily": {Enabled: true, Account: "main", ResumeID: "res-1", MaxPerDay: 10},
		},
		Email: Email{PerHourLimit: 5, PerDayLimit: 20},
	}
}

// mutateAccount copies the account out of the map, applies f, and writes it
// back (Go forbids assigning to fields of a map value directly).
func mutateAccount(c *Config, id string, f func(*Account)) {
	a := c.Accounts[id]
	f(&a)
	c.Accounts[id] = a
}

// mutateRun is mutateAccount for run profiles.
func mutateRun(c *Config, id string, f func(*Run)) {
	r := c.Runs[id]
	f(&r)
	c.Runs[id] = r
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}
}

func TestValidateAcceptsDirectConnectionAndEmptyProxyMap(t *testing.T) {
	c := validConfig()
	c.Proxy = nil // no proxies configured
	mutateAccount(c, "main", func(a *Account) {
		a.ProxyRef = ""
		a.CaptchaChain = nil
	})
	if err := Validate(c); err != nil {
		t.Fatalf("Validate(direct account, no proxies) = %v, want nil", err)
	}
}

func TestValidateAcceptsValidWindowsAndTimezone(t *testing.T) {
	c := validConfig()
	c.General.Timezone = "Europe/Moscow"
	mutateRun(c, "daily", func(r *Run) {
		r.Windows = []Window{
			{TZ: "Europe/Moscow", Time: []string{"08:00", "22:30"}, Days: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}},
		}
	})
	if err := Validate(c); err != nil {
		t.Fatalf("Validate(valid windows) = %v, want nil", err)
	}
}

func TestValidateAcceptsDisabledRunWithNoFields(t *testing.T) {
	c := validConfig()
	// A disabled run is inert: no requirements, no referential checks.
	c.Runs["off"] = Run{Enabled: false, Account: "ghost", MaxPerDay: -5, MinMatchPct: -1}
	if err := Validate(c); err != nil {
		t.Fatalf("Validate(disabled empty run) = %v, want nil", err)
	}
}

func TestValidateAcceptsValidAIChain(t *testing.T) {
	c := validConfig()
	c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat"}}
	c.AI.Models = map[string]AIModel{
		"m":  {Provider: "gpt"},
		"m2": {Provider: "gpt", FallbackModelRef: "m"},
	}
	c.AI.Tasks = map[string]AITask{"cover": {ModelProfile: "m2"}}
	if err := Validate(c); err != nil {
		t.Fatalf("Validate(valid ai chain) = %v, want nil", err)
	}
}

func TestValidateAcceptsValidWebhooks(t *testing.T) {
	c := validConfig()
	c.Webhook = map[string]Webhook{
		"raw": {URL: "https://hooks.example/raw", Preset: "raw"},
		"tg":  {URL: "https://api.telegram.org/botX/sendMessage", Preset: "telegram", ChatID: "42"},
	}
	if err := Validate(c); err != nil {
		t.Fatalf("Validate(valid webhooks) = %v, want nil", err)
	}
}

func TestValidateAcceptsReasoningEffortEnumAndZeroTemperature(t *testing.T) {
	c := validConfig()
	c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat"}}
	for i, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		c.AI.Models = map[string]AIModel{
			fmt.Sprintf("m%d", i): {Provider: "gpt", ReasoningEffort: effort, Temperature: 0},
		}
		if err := Validate(c); err != nil {
			t.Fatalf("Validate(reasoning_effort %q, temperature 0) = %v, want nil", effort, err)
		}
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{
			name:    "redact_secrets false",
			mutate:  func(c *Config) { c.Log.RedactSecrets = false },
			wantErr: "redact_secrets",
		},
		{
			name:    "log format invalid",
			mutate:  func(c *Config) { c.Log.Format = "xml" },
			wantErr: "log.format",
		},
		{
			name:    "timezone unparsable",
			mutate:  func(c *Config) { c.General.Timezone = "Mars/Olympus" },
			wantErr: "general.timezone",
		},
		{
			name: "unknown proxy_ref",
			mutate: func(c *Config) {
				mutateAccount(c, "main", func(a *Account) { a.ProxyRef = "ghost" })
			},
			wantErr: "unknown proxy",
		},
		{
			name: "unknown captcha_chain ref",
			mutate: func(c *Config) {
				mutateAccount(c, "main", func(a *Account) { a.CaptchaChain = []string{"cap1", "ghost"} })
			},
			wantErr: "captcha_chain",
		},
		{
			name: "enabled run without resume_id",
			mutate: func(c *Config) {
				mutateRun(c, "daily", func(r *Run) { r.ResumeID = "" })
			},
			wantErr: "resume_id",
		},
		{
			name: "enabled run without account",
			mutate: func(c *Config) {
				mutateRun(c, "daily", func(r *Run) { r.Account = "" })
			},
			wantErr: "account",
		},
		{
			name: "run with unknown account",
			mutate: func(c *Config) {
				mutateRun(c, "daily", func(r *Run) { r.Account = "ghost" })
			},
			wantErr: "unknown account",
		},
		{
			name: "enabled run with max_per_day zero",
			mutate: func(c *Config) {
				mutateRun(c, "daily", func(r *Run) { r.MaxPerDay = 0 })
			},
			wantErr: "enabled=false",
		},
		{
			name: "negative daily_apply_cap",
			mutate: func(c *Config) {
				mutateAccount(c, "main", func(a *Account) { a.DailyApplyCap = -1 })
			},
			wantErr: "negative",
		},
		{
			name: "negative concurrency",
			mutate: func(c *Config) {
				mutateAccount(c, "main", func(a *Account) { a.Concurrency = -2 })
			},
			wantErr: "negative",
		},
		{
			name: "negative rps",
			mutate: func(c *Config) {
				mutateAccount(c, "main", func(a *Account) { a.RPS = -0.5 })
			},
			wantErr: "negative",
		},
		{
			name:    "negative email per_hour limit",
			mutate:  func(c *Config) { c.Email.PerHourLimit = -1 },
			wantErr: "negative",
		},
		{
			name:    "negative email per_day limit",
			mutate:  func(c *Config) { c.Email.PerDayLimit = -1 },
			wantErr: "negative",
		},
		{
			name:    "negative storage busy_timeout_ms",
			mutate:  func(c *Config) { c.Storage.BusyTimeoutMS = -5 },
			wantErr: "negative",
		},
		{
			name:    "negative pacing min_delay_seconds",
			mutate:  func(c *Config) { c.Pacing.MinDelaySec = -1 },
			wantErr: "negative",
		},
		{
			name:    "negative schedule jitter_minutes",
			mutate:  func(c *Config) { c.Schedule.JitterMinutes = -3 },
			wantErr: "negative",
		},
		{
			name: "negative ai provider timeout_seconds",
			mutate: func(c *Config) {
				c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat", TimeoutS: -10}}
			},
			wantErr: "negative",
		},
		{
			name: "ai task references unknown model",
			mutate: func(c *Config) {
				c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat"}}
				c.AI.Models = map[string]AIModel{"m": {Provider: "gpt"}}
				c.AI.Tasks = map[string]AITask{"t": {ModelProfile: "ghost"}}
			},
			wantErr: "model_profile",
		},
		{
			name: "ai task without model_profile",
			mutate: func(c *Config) {
				c.AI.Tasks = map[string]AITask{"t": {}}
			},
			wantErr: "model_profile",
		},
		{
			name: "ai model references unknown provider",
			mutate: func(c *Config) {
				c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat"}}
				c.AI.Models = map[string]AIModel{"m": {Provider: "ghost"}}
			},
			wantErr: "provider",
		},
		{
			name: "ai model without provider",
			mutate: func(c *Config) {
				c.AI.Models = map[string]AIModel{"m": {}}
			},
			wantErr: "provider",
		},
		{
			name: "ai model fallback references unknown model",
			mutate: func(c *Config) {
				c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat"}}
				c.AI.Models = map[string]AIModel{"m": {Provider: "gpt", FallbackModelRef: "ghost"}}
			},
			wantErr: "fallback",
		},
		{
			name: "ai model reasoning_effort invalid",
			mutate: func(c *Config) {
				c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat"}}
				c.AI.Models = map[string]AIModel{"m": {Provider: "gpt", ReasoningEffort: "extreme"}}
			},
			wantErr: "reasoning_effort",
		},
		{
			name: "negative ai model temperature",
			mutate: func(c *Config) {
				c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "openai-chat"}}
				c.AI.Models = map[string]AIModel{"m": {Provider: "gpt", Temperature: -0.1}}
			},
			wantErr: "temperature",
		},
		{
			name: "webhook without url",
			mutate: func(c *Config) {
				c.Webhook = map[string]Webhook{"tg": {Preset: "raw"}}
			},
			wantErr: "url",
		},
		{
			name: "webhook preset invalid",
			mutate: func(c *Config) {
				c.Webhook = map[string]Webhook{"tg": {URL: "https://x", Preset: "slack"}}
			},
			wantErr: "preset",
		},
		{
			name:    "pacing.active_window typo",
			mutate:  func(c *Config) { c.Pacing.ActiveWindow = "08:00-2200" },
			wantErr: "pacing.active_window",
		},
		{
			name:    "pacing.active_window en-dash",
			mutate:  func(c *Config) { c.Pacing.ActiveWindow = "08:00–22:00" },
			wantErr: "pacing.active_window",
		},
		{
			name:    "warmup.active_hours en-dash",
			mutate:  func(c *Config) { c.Warmup.ActiveHours = "10:00–19:00" },
			wantErr: "warmup.active_hours",
		},
		{
			name:    "pacing.mode unknown",
			mutate:  func(c *Config) { c.Pacing.Mode = "turbo" },
			wantErr: "pacing.mode",
		},
		{
			name:    "chatops qa_base enabled (reserved in v1)",
			mutate:  func(c *Config) { c.Features.ChatOps.QABaseEnabled = true },
			wantErr: "qa_base",
		},
		{
			name: "telegram webhook without chat_id",
			mutate: func(c *Config) {
				c.Webhook = map[string]Webhook{"tg": {URL: "https://x", Preset: "telegram"}}
			},
			wantErr: "chat_id",
		},
		{
			name: "webhook unknown proxy_ref",
			mutate: func(c *Config) {
				c.Webhook = map[string]Webhook{"tg": {URL: "https://x", Preset: "raw", ProxyRef: "ghost"}}
			},
			wantErr: "unknown proxy",
		},
		{
			name: "duplicate webhook url",
			mutate: func(c *Config) {
				c.Webhook = map[string]Webhook{
					"a": {URL: "https://same", Preset: "raw"},
					"b": {URL: "https://same", Preset: "raw"},
				}
			},
			wantErr: "duplicate",
		},
		{
			name: "captcha key and key_env together",
			mutate: func(c *Config) {
				cp := c.Captcha["cap1"]
				cp.Key = "plain"
				cp.KeyEnv = "CAP_KEY"
				c.Captcha["cap1"] = cp
			},
			wantErr: "key",
		},
		{
			name: "proxy inline pass without pass_env",
			mutate: func(c *Config) {
				p := c.Proxy["p1"]
				p.Pass = "plain"
				p.PassEnv = ""
				c.Proxy["p1"] = p
			},
			wantErr: "pass_env",
		},
		{
			name:    "negative review sla_hours",
			mutate:  func(c *Config) { c.Review.SLADefaultHours = -24 },
			wantErr: "negative",
		},
		{
			name:    "negative ai token budget",
			mutate:  func(c *Config) { c.AI.DailyBudgetTokens = -100 },
			wantErr: "negative",
		},
		{
			name:    "negative ai usd budget",
			mutate:  func(c *Config) { c.AI.DailyBudgetUSD = -1.5 },
			wantErr: "negative",
		},
		{
			name: "negative run max_per_day",
			mutate: func(c *Config) {
				mutateRun(c, "daily", func(r *Run) { r.MaxPerDay = -3 })
			},
			wantErr: "negative",
		},
		{
			name: "proxy pass and pass_env together",
			mutate: func(c *Config) {
				p := c.Proxy["p1"]
				p.Pass = "plain"
				c.Proxy["p1"] = p
			},
			wantErr: "pass",
		},
		{
			name: "ai dialect invalid",
			mutate: func(c *Config) {
				c.AI.Providers = map[string]AIProvider{"gpt": {Dialect: "gemini"}}
			},
			wantErr: "dialect",
		},
		{
			name: "captcha protocol invalid",
			mutate: func(c *Config) {
				cp := c.Captcha["cap1"]
				cp.Protocol = "hcaptcha"
				c.Captcha["cap1"] = cp
			},
			wantErr: "protocol",
		},
		{
			name: "window time format invalid",
			mutate: func(c *Config) {
				mutateRun(c, "daily", func(r *Run) {
					r.Windows = []Window{{Time: []string{"8:00"}}}
				})
			},
			wantErr: "time",
		},
		{
			name: "window day invalid",
			mutate: func(c *Config) {
				mutateRun(c, "daily", func(r *Run) {
					r.Windows = []Window{{Time: []string{"08:00"}, Days: []string{"funday"}}}
				})
			},
			wantErr: "days",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := Validate(c)
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}
