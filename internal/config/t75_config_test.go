package config

// T75: the gray-band policy — [features.multiapply].gray_band routes a
// gray-band needs_human verdict of the AI scorer. Default apply (owner
// decision: a live run must not stall on borderline vacancies); injection
// and red-flag verdicts always park regardless of the setting.

import (
	"strings"
	"testing"
)

func TestGrayBandDefaultIsApply(t *testing.T) {
	var c Config
	applyDefaults(&c)
	if c.Features.MultiApply.GrayBand != GrayBandApply {
		t.Errorf("gray_band default = %q, want %q", c.Features.MultiApply.GrayBand, GrayBandApply)
	}
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("default gray_band must validate: %v", err)
	}
}

func TestValidateGrayBandVocabulary(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string // empty = must validate
	}{
		{GrayBandApply, ""},
		{GrayBandNeedsHuman, ""},
		{GrayBandSkip, ""},
		{"respond", "features.multiapply.gray_band"},
		{"APPLY", "features.multiapply.gray_band"},
	} {
		c := validConfig()
		c.Features.MultiApply.GrayBand = tc.value
		err := Validate(c)
		if tc.want == "" {
			if err != nil {
				t.Errorf("gray_band=%q: unexpected error %v", tc.value, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("gray_band=%q: err = %v, want prefix %q", tc.value, err, tc.want)
		}
	}
}
