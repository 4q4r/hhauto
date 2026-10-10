package config

// T114: [features.auto_lift] too_early_cooldown — the retry plan after a
// 409 touch (ErrTouchTooEarly). Default "30m"; a non-empty value must
// parse as a positive duration.

import "testing"

func TestValidateAutoLiftTooEarlyCooldown(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Log.RedactSecrets = true // safety-critical default (validateGlobals)
		c.Log.Format = "text"
		c.API.Transport = TransportTLSClient
		c.General.Timezone = "Local"
		c.Features.AutoLift = AutoLift{Enabled: true, TooEarlyCooldown: "30m"}
		return c
	}

	if err := Validate(base()); err != nil {
		t.Fatalf("valid cooldown rejected: %v", err)
	}

	bad := base()
	bad.Features.AutoLift.TooEarlyCooldown = "soon"
	if err := Validate(bad); err == nil || !contains(err.Error(), "too_early_cooldown") {
		t.Fatalf("cooldown validation: %v", err)
	}

	nonPositive := base()
	nonPositive.Features.AutoLift.TooEarlyCooldown = "-5m"
	if err := Validate(nonPositive); err == nil || !contains(err.Error(), "too_early_cooldown") {
		t.Fatalf("negative cooldown validation: %v", err)
	}
}
