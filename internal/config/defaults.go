package config

// Field→default declaration tables used by applyDefaults. Each rule fills dst
// only while dst still holds its zero value, exactly like the previous chain
// of `if x == zero` guards.

type stringDefault struct {
	dst *string
	val string
}

type intDefault struct {
	dst *int
	val int
}

type boolPtrDefault struct {
	dst **bool
	val bool
}

// applyDefaults fills every unset (zero) value with the documented default
// from docs/PLAN.md §5 so that a minimal TOML file yields a working config.
//
// Default-true toggles are *bool (orchestrator ruling R1): nil means "not
// set" and becomes true, while an explicit false in TOML is respected.
// Log.RedactSecrets and Forms.AbortOnPasswordField stay plain bool and are
// forced to true: they are safety-critical and have no opt-out.
func applyDefaults(c *Config) {
	for _, d := range []stringDefault{
		{&c.General.Timezone, "Local"},
		{&c.Log.Level, "info"},
		{&c.Log.Format, "text"},
		{&c.API.Kind, "web"},
		{&c.API.Endpoints.Site, "https://hh.ru"},
		{&c.API.Endpoints.Chatik, "https://chatik.hh.ru"},
		{&c.API.Endpoints.SearchPage, "https://hh.ru/search/vacancy"},
		{&c.Pacing.ActiveWindow, "08:00-22:00"},
		{&c.Pacing.Mode, "adaptive"},
		{&c.Forms.Mode, "draft_only"},
		{&c.Forms.SubmitPolicy, "human"},
		{&c.Schedule.MissedRunPolicy, "run_once"},
	} {
		if *d.dst == "" {
			*d.dst = d.val
		}
	}

	for _, d := range []intDefault{
		{&c.Storage.BusyTimeoutMS, 5000},
		{&c.Features.AutoLift.PerDay, 6},
		{&c.Review.SLADefaultHours, 24},
		{&c.Warmup.SilenceHours, 24},
		{&c.Warmup.MaxPings, 3},
		{&c.Schedule.MaxCatchupHours, 6},
	} {
		if *d.dst == 0 {
			*d.dst = d.val
		}
	}

	for _, d := range []boolPtrDefault{
		{&c.Features.AutoLift.Respect4hTimer, true},
		{&c.Pacing.NightPause, true},
		{&c.Features.ChatOps.AutoReply, true}, // owner decision #6: auto-reply is ON by default
		{&c.Email.PerEmployerOnce, true},
		{&c.Email.AttachResume, true},
		{&c.Warmup.RespectWeekend, true},
	} {
		if *d.dst == nil {
			v := d.val
			*d.dst = &v
		}
	}

	for id, a := range c.Accounts {
		applyAccountDefaults(&a)
		c.Accounts[id] = a
	}

	// Mandatory; Validate also rejects explicit false.
	c.Log.RedactSecrets = true
	c.Forms.AbortOnPasswordField = true

	if len(c.Forms.AllowedDomains) == 0 {
		c.Forms.AllowedDomains = []string{"hh.ru", "forms.gle", "docs.google.com", "forms.yandex.ru"}
	}
	if len(c.Warmup.LadderHours) == 0 {
		c.Warmup.LadderHours = []int{24, 48, 96}
	}
}

// applyAccountDefaults fills unset per-account values in place.
func applyAccountDefaults(a *Account) {
	for _, d := range []intDefault{
		{&a.DailyApplyCap, 200},
		{&a.Concurrency, 2},
	} {
		if *d.dst == 0 {
			*d.dst = d.val
		}
	}
	if a.RPS == 0 {
		a.RPS = 0.5
	}
}
