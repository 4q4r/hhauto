package config

// applyDefaults fills every unset (zero) value with the documented default
// from docs/PLAN.md §5 so that a minimal TOML file yields a working config.
//
// Note: because the config contract uses plain value types (no pointers),
// boolean defaults are applied unconditionally — an explicit `false` in TOML
// for the safety flags below is normalized back to the safe default.
func applyDefaults(c *Config) {
	if c.General.Timezone == "" {
		c.General.Timezone = "Local"
	}
	if c.Storage.BusyTimeoutMS == 0 {
		c.Storage.BusyTimeoutMS = 5000
	}

	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	c.Log.RedactSecrets = true // mandatory; Validate also rejects explicit false

	if c.API.Kind == "" {
		c.API.Kind = "web"
	}
	if c.API.Endpoints.Site == "" {
		c.API.Endpoints.Site = "https://hh.ru"
	}
	if c.API.Endpoints.Chatik == "" {
		c.API.Endpoints.Chatik = "https://chatik.hh.ru"
	}
	if c.API.Endpoints.SearchPage == "" {
		c.API.Endpoints.SearchPage = "https://hh.ru/search/vacancy"
	}

	for id, a := range c.Accounts {
		if a.DailyApplyCap == 0 {
			a.DailyApplyCap = 200
		}
		if a.Concurrency == 0 {
			a.Concurrency = 2
		}
		if a.RPS == 0 {
			a.RPS = 0.5
		}
		c.Accounts[id] = a
	}

	if c.Features.AutoLift.PerDay == 0 {
		c.Features.AutoLift.PerDay = 6
	}
	c.Features.AutoLift.Respect4hTimer = true

	if c.Pacing.ActiveWindow == "" {
		c.Pacing.ActiveWindow = "08:00-22:00"
	}
	if c.Pacing.Mode == "" {
		c.Pacing.Mode = "adaptive"
	}
	c.Pacing.NightPause = true

	if c.Review.SLADefaultHours == 0 {
		c.Review.SLADefaultHours = 24
	}

	if c.Forms.Mode == "" {
		c.Forms.Mode = "draft_only"
	}
	if c.Forms.SubmitPolicy == "" {
		c.Forms.SubmitPolicy = "human"
	}
	c.Forms.AbortOnPasswordField = true
	if len(c.Forms.AllowedDomains) == 0 {
		c.Forms.AllowedDomains = []string{"hh.ru", "forms.gle", "docs.google.com", "forms.yandex.ru"}
	}

	c.Email.PerEmployerOnce = true

	if c.Warmup.SilenceHours == 0 {
		c.Warmup.SilenceHours = 24
	}
	if len(c.Warmup.LadderHours) == 0 {
		c.Warmup.LadderHours = []int{24, 48, 96}
	}
	if c.Warmup.MaxPings == 0 {
		c.Warmup.MaxPings = 3
	}

	if c.Schedule.MissedRunPolicy == "" {
		c.Schedule.MissedRunPolicy = "run_once"
	}
	if c.Schedule.MaxCatchupHours == 0 {
		c.Schedule.MaxCatchupHours = 6
	}
}
