package config

// T89: the captcha postpone policy — [features.multiapply.captcha] defaults
// to the owner's behavior (postpone: park on captcha, pause applies at the
// limit, re-attack after retry_after); mode="skip" restores the pre-T89
// day-breaker behavior.

import (
	"strings"
	"testing"
)

func TestCaptchaPostponeDefaults(t *testing.T) {
	var c Config
	applyDefaults(&c)
	capCfg := c.Features.MultiApply.Captcha
	if capCfg.Mode != CaptchaModePostpone {
		t.Errorf("captcha mode default = %q, want %q", capCfg.Mode, CaptchaModePostpone)
	}
	if capCfg.Limit != 3 {
		t.Errorf("captcha limit default = %d, want 3", capCfg.Limit)
	}
	if capCfg.Cooldown != "45m" {
		t.Errorf("captcha cooldown default = %q, want 45m", capCfg.Cooldown)
	}
	if capCfg.RetryAfter != "2h" {
		t.Errorf("captcha retry_after default = %q, want 2h", capCfg.RetryAfter)
	}
	if capCfg.MaxRetries != 3 {
		t.Errorf("captcha max_retries default = %d, want 3", capCfg.MaxRetries)
	}
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("default captcha section must validate: %v", err)
	}
}

func TestValidateCaptchaSection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutat func(*MultiApplyCaptcha)
		want  string // empty = must validate
	}{
		{"skip mode", func(m *MultiApplyCaptcha) { m.Mode = CaptchaModeSkip }, ""},
		{"postpone mode", func(m *MultiApplyCaptcha) { m.Mode = CaptchaModePostpone }, ""},
		{"bad mode", func(m *MultiApplyCaptcha) { m.Mode = "retry" }, "features.multiapply.captcha.mode"},
		{"bad cooldown", func(m *MultiApplyCaptcha) { m.Cooldown = "soon" }, "features.multiapply.captcha.cooldown"},
		{"zero cooldown", func(m *MultiApplyCaptcha) { m.Cooldown = "0s" }, "features.multiapply.captcha.cooldown"},
		{"bad retry_after", func(m *MultiApplyCaptcha) { m.RetryAfter = "2hours" }, "features.multiapply.captcha.retry_after"},
		{"negative limit", func(m *MultiApplyCaptcha) { m.Limit = -1 }, "features.multiapply.captcha.limit"},
		{"negative max_retries", func(m *MultiApplyCaptcha) { m.MaxRetries = -1 }, "features.multiapply.captcha.max_retries"},
	} {
		c := validConfig()
		tc.mutat(&c.Features.MultiApply.Captcha)
		err := Validate(c)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want prefix %q", tc.name, err, tc.want)
		}
	}
}
