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

type floatDefault struct {
	dst *float64
	val float64
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
		{&c.API.Transport, TransportTLSClient},
		{&c.API.Endpoints.Site, "https://hh.ru"},
		{&c.API.Endpoints.Chatik, "https://chatik.hh.ru"},
		{&c.API.Endpoints.SearchPage, "https://hh.ru/search/vacancy"},
		{&c.Pacing.ActiveWindow, "08:00-22:00"},
		{&c.Pacing.Mode, "adaptive"},
		{&c.Forms.Mode, "draft_only"},
		{&c.Forms.SubmitPolicy, "human"},
		{&c.Browser.Preset, BrowserPresetCareful},
		{&c.Browser.Channel, BrowserChannelAuto},
		{&c.Schedule.MissedRunPolicy, "run_once"},
	} {
		if *d.dst == "" {
			*d.dst = d.val
		}
	}

	for _, d := range []intDefault{
		{&c.Storage.BusyTimeoutMS, 5000},
		{&c.Features.AutoLift.PerDay, 6},
		{&c.Features.AutoClone.MaxClonesPerResume, 2},
		{&c.Features.AutoClone.MaxCloneDepth, 1},
		{&c.Features.AutoClone.SkipRejectionDays, 30},
		{&c.Features.AutoClone.FinishTimeoutMin, 10},
		{&c.Review.SLADefaultHours, 24},
		{&c.Warmup.SilenceHours, 24},
		{&c.Warmup.MaxPings, 3},
		{&c.Schedule.MaxCatchupHours, 6},
	} {
		if *d.dst == 0 {
			*d.dst = d.val
		}
	}

	for _, d := range []stringDefault{
		{&c.Features.AutoClone.ReapplyMode, ReapplyNewOnly},
		{&c.Features.AutoClone.CloneOriginalAction, CloneActionKeep},
		{&c.Features.AutoClone.Improve.Editor.BaseURL, "https://hh.ru"},
		{&c.Features.AutoClone.Improve.Editor.CookieDomain, ".hh.ru"},
		{&c.Features.MultiApply.GrayBand, GrayBandApply}, // T75: gray zone responds by default
		{&c.Console.Skips, "interesting,aggregate"},      // T69: interesting skip lines + digest breakdown
	} {
		if *d.dst == "" {
			*d.dst = d.val
		}
	}
	applySetkaDefaults(c)
	applyVerdictCacheDefaults(c) // T91: verdict caches alive (24h) for scoring/letters
	applyCaptchaDefaults(c)      // T89: captcha postpone defaults (mode=postpone, limit=3, cooldown=45m)

	for _, d := range []intDefault{
		{&c.Features.AutoClone.Improve.Editor.PageTimeoutSec, 120},
		{&c.Features.AutoClone.Improve.Editor.SaveTimeoutSec, 20},
	} {
		if *d.dst == 0 {
			*d.dst = d.val
		}
	}

	// T33: Jev-compatible prefilter defaults (endpoint per the owner's
	// live contract; threshold 0.85). T74: the systemone endpoint default
	// does not apply to wire="openai-chat" — a chat endpoint (e.g.
	// OpenRouter) must be configured explicitly and validation reports
	// the missing endpoint fail-fast.
	if c.AI.Jev.Wire != "openai-chat" && c.AI.Jev.Endpoint == "" {
		c.AI.Jev.Endpoint = "https://api.typesafe.ai/v1/systemone"
	}
	for _, d := range []floatDefault{
		{&c.AI.Jev.ConfidenceThreshold, 0.85},
	} {
		if *d.dst == 0 {
			*d.dst = d.val
		}
	}

	for _, d := range []boolPtrDefault{
		{&c.Features.MultiApply.LetterIgnoreLanguage, true}, // T86: a missing language section never blocks the letter
		{&c.Features.AutoClone.Autolift, true},
		{&c.Features.AutoClone.Improve.ExperienceReplace, true}, // T58: the owner's clone already grew duplicates from improve applies
		{&c.Pacing.NightPause, true},
		{&c.Features.ChatOps.AutoReply, true}, // owner decision #6: auto-reply is ON by default
		{&c.Email.PerEmployerOnce, true},
		{&c.Email.AttachResume, true},
		{&c.Warmup.RespectWeekend, true},
		{&c.Browser.AutoDownload, true},             // T32: cloakbrowser auto-download on
		{&c.Browser.Humanize, true},                 // T32: humanize launch profile on
		{&c.Features.Setka.RequireCanMessage, true}, // T61: skip hh-ghosts
		{&c.Features.Setka.NoLinks, true},           // T61: links are a shadow-ban trigger
		{&c.Console.Applied, true},                  // T69: console surface defaults — vacancy lines on
		{&c.Console.Errors, true},
		{&c.Console.Letters, true},
		{&c.Console.Lift, true},
	} {
		if *d.dst == nil {
			v := d.val
			*d.dst = &v
		}
	}

	for id, r := range c.Runs {
		if r.LetterMode == "" {
			r.LetterMode = LetterModeAICompany
		}
		c.Runs[id] = r
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

// applyVerdictCacheDefaults fills the T91 verdict-cache default: the two
// slow LLM tasks cache their verdicts for 24h by default (the owner pays
// to re-roll identical inputs at 60-100s/vacancy; T86 report: 601/601 live
// cache misses). An explicit cache_ttl_hours = 0 stays disabled; other
// tasks are untouched — their caching was never requested.
func applyVerdictCacheDefaults(c *Config) {
	for id, t := range c.AI.Tasks {
		if id != "vacancy_scoring" && id != "cover_letter" {
			continue
		}
		if t.CacheTTLHours == nil {
			h := 24
			t.CacheTTLHours = &h
		}
		c.AI.Tasks[id] = t
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

// applyCaptchaDefaults fills the [features.multiapply.captcha] defaults
// (T89 owner behavior): postpone mode, limit 3, 45m cooldown, 2h retry
// delay, 3 retries per vacancy.
func applyCaptchaDefaults(c *Config) {
	for _, d := range []stringDefault{
		{&c.Features.MultiApply.Captcha.Mode, CaptchaModePostpone},            // postpone instead of the captcha skip
		{&c.Features.MultiApply.Captcha.Cooldown, DefaultCaptchaCooldown},     // apply-phase pause
		{&c.Features.MultiApply.Captcha.RetryAfter, DefaultCaptchaRetryAfter}, // postponed re-attack delay
	} {
		if *d.dst == "" {
			*d.dst = d.val
		}
	}
	for _, d := range []intDefault{
		{&c.Features.MultiApply.Captcha.Limit, DefaultCaptchaLimit},           // the 3rd captcha pauses the pass
		{&c.Features.MultiApply.Captcha.MaxRetries, DefaultCaptchaMaxRetries}, // per-vacancy retry budget
	} {
		if *d.dst == 0 {
			*d.dst = d.val
		}
	}
}

// applySetkaDefaults fills the [features.setka] defaults (T61): auto mode,
// 30m delay, 12:00-18:00 window, 5/2/1 caps, safety toggles on, the
// "нанимаю" status filter.
func applySetkaDefaults(c *Config) {
	for _, d := range []stringDefault{
		{&c.Features.Setka.Mode, SetkaModeAuto},
		{&c.Features.Setka.AfterApplyDelay, "30m"},
		{&c.Features.Setka.ActiveWindow, "12:00-18:00"},
		{&c.Features.Setka.Classify.Borderline, SetkaBorderlineApply},
	} {
		if *d.dst == "" {
			*d.dst = d.val
		}
	}
	for _, d := range []intDefault{
		{&c.Features.Setka.MaxDMPerDay, 5},
		{&c.Features.Setka.MaxDMPerCompany, 2},
		{&c.Features.Setka.MaxDMPerPerson, 1},
	} {
		if *d.dst == 0 {
			*d.dst = d.val
		}
	}
	for _, d := range []boolPtrDefault{
		{&c.Features.Setka.RequireCanMessage, true}, // skip hh-ghosts
		{&c.Features.Setka.NoLinks, true},           // links are a shadow-ban trigger
	} {
		if *d.dst == nil {
			v := d.val
			*d.dst = &v
		}
	}
	if len(c.Features.Setka.Targets.StatusFilter) == 0 {
		c.Features.Setka.Targets.StatusFilter = []string{"нанимаю"}
	}
}
