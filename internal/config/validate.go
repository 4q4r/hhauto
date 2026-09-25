package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// validDays is the lowercase weekday vocabulary for run windows.
var validDays = map[string]bool{
	"mon": true, "tue": true, "wed": true, "thu": true,
	"fri": true, "sat": true, "sun": true,
}

// Validate checks referential integrity and value sanity. It is fail-fast:
// the first problem is returned with the offending config path in the message.
func Validate(c *Config) error {
	if err := validateGlobals(c); err != nil {
		return err
	}
	if err := validateCaptcha(c); err != nil {
		return err
	}
	if err := validateProxies(c); err != nil {
		return err
	}
	if err := validateWebhook(c); err != nil {
		return err
	}
	if err := validateAccounts(c); err != nil {
		return err
	}
	if err := validateAI(c); err != nil {
		return err
	}
	if err := validateAutoClone(c); err != nil {
		return err
	}
	return validateRuns(c)
}

// validateAutoClone checks the [features.autoclone] section (T26).
func validateAutoClone(c *Config) error {
	ac := c.Features.AutoClone
	if !ac.Enabled {
		return nil
	}
	switch ac.ReapplyMode {
	case ReapplyAll, ReapplyNewOnly, ReapplySkipRecentRejections:
	default:
		return fmt.Errorf("features.autoclone.reapply_mode %q: must be one of all | new_only | skip_recent_rejections", ac.ReapplyMode)
	}
	if err := checkNegatives(map[string]float64{
		"features.autoclone.max_clones_per_resume": float64(ac.MaxClonesPerResume),
		"features.autoclone.max_clone_depth":       float64(ac.MaxCloneDepth),
		"features.autoclone.skip_rejection_days":   float64(ac.SkipRejectionDays),
		"features.autoclone.finish_timeout_min":    float64(ac.FinishTimeoutMin),
	}); err != nil {
		return err
	}
	if ac.MaxClonesPerResume == 0 {
		return fmt.Errorf("features.autoclone.max_clones_per_resume=0: autoclone could never create a clone; disable features.autoclone.enabled instead")
	}
	if ac.MaxCloneDepth == 0 {
		return fmt.Errorf("features.autoclone.max_clone_depth=0: depth is counted from the root resume; use 1 (no chains) or higher")
	}
	if ac.ReapplyMode == ReapplySkipRecentRejections && ac.SkipRejectionDays <= 0 {
		return fmt.Errorf("features.autoclone.skip_rejection_days=%d: must be positive for skip_recent_rejections", ac.SkipRejectionDays)
	}
	ov := ac.RunOverride
	switch ov.LetterMode {
	case "", LetterModeTemplate, LetterModeTemplateAI, LetterModeAICompany:
	default:
		return fmt.Errorf("features.autoclone.run_override.letter_mode %q: must be one of template | template_ai | ai_company", ov.LetterMode)
	}
	if ov.MaxPerDay < 0 {
		return fmt.Errorf("features.autoclone.run_override.max_per_day: negative values are not allowed (got %d)", ov.MaxPerDay)
	}
	return nil
}

func validateGlobals(c *Config) error {
	if !c.Log.RedactSecrets {
		return fmt.Errorf("log.redact_secrets=false is not allowed: credential redaction is mandatory")
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q: must be \"text\" or \"json\"", c.Log.Format)
	}
	switch c.API.Transport {
	case TransportTLSClient, TransportHTTP:
	default:
		return fmt.Errorf("api.transport %q: must be %q or %q", c.API.Transport, TransportTLSClient, TransportHTTP)
	}
	if _, err := time.LoadLocation(c.General.Timezone); err != nil {
		return fmt.Errorf("general.timezone %q: %w", c.General.Timezone, err)
	}
	// Fail-closed windows and mode enum: an unparsable window previously
	// degraded to "always active" at runtime — reject it at startup instead.
	// An empty pacing.active_window is legal ("always active", the pacer's
	// documented default) — but applyDefaults normally fills it first.
	if c.Pacing.ActiveWindow != "" {
		if _, _, err := ParseActiveWindow(c.Pacing.ActiveWindow); err != nil {
			return fmt.Errorf("pacing.active_window %q: %w", c.Pacing.ActiveWindow, err)
		}
	}
	switch mode := strings.ToLower(strings.TrimSpace(c.Pacing.Mode)); mode {
	case "", "adaptive", "uniform", "burst": // "" = unset, defaults fill "adaptive"
	default:
		return fmt.Errorf("pacing.mode %q: must be \"adaptive\", \"uniform\" or \"burst\"", c.Pacing.Mode)
	}
	if c.Warmup.ActiveHours != "" {
		if _, _, err := ParseActiveWindow(c.Warmup.ActiveHours); err != nil {
			return fmt.Errorf("warmup.active_hours %q: %w", c.Warmup.ActiveHours, err)
		}
	}
	// R14: the qa_base toggle has no v1 implementation — reject enabling it
	// instead of silently ignoring the flag.
	if c.Features.ChatOps.QABaseEnabled {
		return fmt.Errorf("features.chatops.qa_base=true: не реализовано в v1 (зарезервировано)")
	}
	return checkNegatives(map[string]float64{
		"storage.busy_timeout_ms":    float64(c.Storage.BusyTimeoutMS),
		"ai.daily_budget_tokens":     float64(c.AI.DailyBudgetTokens),
		"ai.daily_budget_usd":        c.AI.DailyBudgetUSD,
		"review.sla_hours_default":   float64(c.Review.SLADefaultHours),
		"forms.page_timeout_sec":     float64(c.Forms.PageTimeoutSec),
		"email.limits_per_hour":      float64(c.Email.PerHourLimit),
		"email.limits_per_day":       float64(c.Email.PerDayLimit),
		"email.retries":              float64(c.Email.Retries),
		"warmup.silence_hours":       float64(c.Warmup.SilenceHours),
		"warmup.max_pings":           float64(c.Warmup.MaxPings),
		"schedule.max_catchup_hours": float64(c.Schedule.MaxCatchupHours),
		"schedule.jitter_minutes":    float64(c.Schedule.JitterMinutes),
		"pacing.min_delay_seconds":   float64(c.Pacing.MinDelaySec),
	})
}

func validateCaptcha(c *Config) error {
	for id, p := range c.Captcha {
		switch p.Protocol {
		case "2captcha", "anticaptcha":
		default:
			return fmt.Errorf("captcha[%q].protocol %q: must be \"2captcha\" or \"anticaptcha\"", id, p.Protocol)
		}
		if p.Key != "" && p.KeyEnv != "" {
			return fmt.Errorf("captcha[%q]: set either key or key_env, not both (use key_env)", id)
		}
		if err := checkNegatives(map[string]float64{
			fmt.Sprintf("captcha[%q].poll_interval_ms", id): float64(p.PollMS),
			fmt.Sprintf("captcha[%q].timeout_s", id):        float64(p.TimeoutS),
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateProxies(c *Config) error {
	for id, p := range c.Proxy {
		switch {
		case p.Pass != "" && p.PassEnv != "":
			return fmt.Errorf("proxy[%q]: set either pass or pass_env, not both (use pass_env)", id)
		case p.Pass != "":
			return fmt.Errorf("proxy[%q]: inline pass is not allowed: use pass_env", id)
		}
	}
	return nil
}

func validateAccounts(c *Config) error {
	for id, a := range c.Accounts {
		switch {
		case a.ProxyRef != "":
			if _, ok := c.Proxy[a.ProxyRef]; !ok {
				return fmt.Errorf("accounts[%q].proxy_ref %q: unknown proxy", id, a.ProxyRef)
			}
		case len(c.Proxy) > 0:
			// Direct connection while proxies exist is allowed — informational only.
			slog.Info("account uses a direct connection while proxies are configured", "account", id)
		}
		for i, ref := range a.CaptchaChain {
			if _, ok := c.Captcha[ref]; !ok {
				return fmt.Errorf("accounts[%q].captcha_chain[%d] %q: unknown captcha provider", id, i, ref)
			}
		}
		if err := checkNegatives(map[string]float64{
			fmt.Sprintf("accounts[%q].concurrency", id):     float64(a.Concurrency),
			fmt.Sprintf("accounts[%q].daily_apply_cap", id): float64(a.DailyApplyCap),
			fmt.Sprintf("accounts[%q].rps", id):             a.RPS,
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateAI(c *Config) error {
	for id, p := range c.AI.Providers {
		switch p.Dialect {
		case "openai-chat", "openai-responses", "anthropic-messages":
		default:
			return fmt.Errorf("ai.providers[%q].dialect %q: must be \"openai-chat\", \"openai-responses\" or \"anthropic-messages\"", id, p.Dialect)
		}
		if err := checkNegatives(map[string]float64{
			fmt.Sprintf("ai.providers[%q].timeout_seconds", id): float64(p.TimeoutS),
		}); err != nil {
			return err
		}
	}
	for id, m := range c.AI.Models {
		if m.Provider == "" {
			return fmt.Errorf("ai.models[%q].provider: required", id)
		}
		if _, ok := c.AI.Providers[m.Provider]; !ok {
			return fmt.Errorf("ai.models[%q].provider %q: unknown ai provider", id, m.Provider)
		}
		switch m.ReasoningEffort {
		case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return fmt.Errorf("ai.models[%q].reasoning_effort %q: must be one of none|minimal|low|medium|high|xhigh|max (empty = provider default)", id, m.ReasoningEffort)
		}
		if m.Temperature < 0 {
			return fmt.Errorf("ai.models[%q].temperature %v: negative values are not allowed", id, m.Temperature)
		}
		if m.FallbackModelRef != "" {
			if _, ok := c.AI.Models[m.FallbackModelRef]; !ok {
				return fmt.Errorf("ai.models[%q].fallback %q: unknown model", id, m.FallbackModelRef)
			}
		}
	}
	for id, t := range c.AI.Tasks {
		if t.ModelProfile == "" {
			return fmt.Errorf("ai.tasks[%q].model_profile: required", id)
		}
		if _, ok := c.AI.Models[t.ModelProfile]; !ok {
			return fmt.Errorf("ai.tasks[%q].model_profile %q: unknown model", id, t.ModelProfile)
		}
	}
	return nil
}

// validateWebhook checks webhook sections (the T5 outbox seam): every entry
// needs a url and a known preset, telegram needs a chat_id, proxy refs must
// resolve, and a url may only be used by one webhook (outbox key collision).
func validateWebhook(c *Config) error {
	owner := make(map[string]string, len(c.Webhook)) // url -> webhook id
	for id, w := range c.Webhook {
		if w.URL == "" {
			return fmt.Errorf("webhook[%q].url: required", id)
		}
		switch w.Preset {
		case "raw", "telegram", "discord":
		default:
			return fmt.Errorf("webhook[%q].preset %q: must be \"raw\", \"telegram\" or \"discord\"", id, w.Preset)
		}
		if w.Preset == "telegram" && w.ChatID == "" {
			return fmt.Errorf("webhook[%q].chat_id: required when preset is \"telegram\"", id)
		}
		if w.ProxyRef != "" {
			if _, ok := c.Proxy[w.ProxyRef]; !ok {
				return fmt.Errorf("webhook[%q].proxy_ref %q: unknown proxy", id, w.ProxyRef)
			}
		}
		if prev, ok := owner[w.URL]; ok {
			return fmt.Errorf("webhook[%q]: url %q is already used by webhook[%q]: duplicate urls are not allowed (outbox key collision)", id, w.URL, prev)
		}
		owner[w.URL] = id
	}
	return nil
}

func validateRuns(c *Config) error {
	for id, r := range c.Runs {
		// A disabled run is inert: it carries no requirements and its fields
		// (accounts, limits, windows) are not validated.
		if !r.Enabled {
			continue
		}
		if r.Account == "" {
			return fmt.Errorf("runs[%q].account: required when enabled", id)
		}
		if r.ResumeID == "" {
			return fmt.Errorf("runs[%q].resume_id: required when enabled", id)
		}
		if r.MaxPerDay == 0 {
			return fmt.Errorf("runs[%q].max_per_day=0: use enabled=false to disable the run", id)
		}
		if _, ok := c.Accounts[r.Account]; !ok {
			return fmt.Errorf("runs[%q].account %q: unknown account", id, r.Account)
		}
		switch r.LetterMode {
		case "", LetterModeTemplate, LetterModeTemplateAI, LetterModeAICompany: // "" = unset, defaults fill ai_company
		default:
			return fmt.Errorf("runs[%q].letter_mode %q: must be one of template | template_ai | ai_company", id, r.LetterMode)
		}
		if (r.LetterMode == LetterModeTemplate || r.LetterMode == LetterModeTemplateAI) &&
			strings.TrimSpace(r.LetterTemplate) == "" {
			return fmt.Errorf("runs[%q].letter_mode=%q requires letter_template", id, r.LetterMode)
		}
		if err := checkNegatives(map[string]float64{
			fmt.Sprintf("runs[%q].max_per_day", id):       float64(r.MaxPerDay),
			fmt.Sprintf("runs[%q].min_match_percent", id): float64(r.MinMatchPct),
		}); err != nil {
			return err
		}
		for _, f := range r.SearchFields {
			switch f {
			case "name", "company_name", "description":
			default:
				return fmt.Errorf("runs[%q].search_fields %q: must be one of name | company_name | description (empty = all fields)", id, f)
			}
		}
		for i, w := range r.Windows {
			if err := validateWindow(fmt.Sprintf("runs[%q].windows[%d]", id, i), w); err != nil {
				return err
			}
		}
		if err := validateRunFilters(fmt.Sprintf("runs[%q].filters", id), r.Filters); err != nil {
			return err
		}
	}
	return nil
}

// validateRunFilters rejects values hh would silently ignore. hh does not
// error on unknown enum values — it drops them (and, worse, silently drops
// the whole filter), so the config layer is the only line of defense. The
// comparisons are exact (no case-folding, no trimming): hh matches these
// values case-sensitively (live-verified 2026-09-25).
func validateRunFilters(path string, f RunFilters) error {
	for i, v := range f.WorkFormats {
		switch v {
		case "", "REMOTE", "HYBRID", "ON_SITE": // "" = skipped on the wire
		default:
			return fmt.Errorf("%s.work_formats[%d] %q: must be one of REMOTE|HYBRID|ON_SITE (case-sensitive)", path, i, v)
		}
	}
	for i, v := range f.Part {
		switch v {
		case "", "temp", "evening", "weekend":
		default:
			return fmt.Errorf("%s.part[%d] %q: must be one of temp|evening|weekend", path, i, v)
		}
	}
	switch f.OrderBy {
	case "", "relevance", "publication_time_desc", "salary_desc", "salary_asc":
	default:
		return fmt.Errorf("%s.order_by %q: must be one of relevance|publication_time_desc|salary_desc|salary_asc", path, f.OrderBy)
	}
	return nil
}

func validateWindow(path string, w Window) error {
	for _, t := range w.Time {
		parsed, err := time.Parse("15:04", t)
		// time.Parse accepts "8:00" for layout "15:04"; require the exact
		// zero-padded form by round-tripping.
		if err != nil || parsed.Format("15:04") != t {
			return fmt.Errorf("%s.time %q: must use \"15:04\" (e.g. 08:30)", path, t)
		}
	}
	for _, d := range w.Days {
		if !validDays[strings.ToLower(d)] {
			return fmt.Errorf("%s.days %q: must be one of mon..sun", path, d)
		}
	}
	return nil
}

// checkNegatives fails fast on the first negative value.
func checkNegatives(values map[string]float64) error {
	for name, v := range values {
		if v < 0 {
			return fmt.Errorf("%s: negative values are not allowed (got %v)", name, v)
		}
	}
	return nil
}
