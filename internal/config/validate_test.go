package config

import (
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
