package featurekit

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// ParseJitterRange parses a schedule jitter expression: "" (no jitter),
// "<dur>" (fixed) or "<dur>-<dur>" (uniform range), where each part is a Go
// duration ("40m", "90s", "2h", "1h30m"). The range form requires max >= min.
func ParseJitterRange(s string) (time.Duration, time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, nil
	}
	minPart, maxPart, isRange := strings.Cut(s, "-")
	mn, err := time.ParseDuration(strings.TrimSpace(minPart))
	if err != nil && isRange {
		// "20-40m" style: the min part inherits the max part's unit.
		if unit, ok := durationUnit(maxPart); ok && allDigits(strings.TrimSpace(minPart)) {
			if mn2, err2 := time.ParseDuration(strings.TrimSpace(minPart) + unit); err2 == nil {
				mn, err = mn2, nil
			}
		}
	}
	if err != nil {
		return 0, 0, fmt.Errorf("featurekit: bad jitter %q: %w", s, err)
	}
	mx := mn
	if isRange {
		mx, err = time.ParseDuration(strings.TrimSpace(maxPart))
		if err != nil {
			return 0, 0, fmt.Errorf("featurekit: bad jitter %q: %w", s, err)
		}
	}
	if mn < 0 || mx < mn {
		return 0, 0, fmt.Errorf("featurekit: bad jitter %q: need 0 <= min <= max", s)
	}
	return mn, mx, nil
}

// JitterDuration draws uniformly from [min, max]. A nil rnd or a degenerate
// range returns min (0 when both are zero) — never panics.
func JitterDuration(min, max time.Duration, rnd *rand.Rand) time.Duration {
	if max <= min {
		return min
	}
	if rnd == nil {
		return min
	}
	return min + time.Duration(rnd.Int63n(int64(max-min+1)))
}

// durationUnit returns the trailing unit suffix of a duration string
// ("40m" → "m", "2h30m" → "m"). ok is false when there is no unit.
func durationUnit(s string) (string, bool) {
	k := strings.LastIndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' })
	if k < 0 || k+1 >= len(s) {
		return "", false
	}
	return s[k+1:], true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
