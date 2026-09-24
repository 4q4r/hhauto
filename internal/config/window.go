package config

import (
	"fmt"
	"strings"
	"time"
)

// ParseActiveWindow strictly parses a daily activity window "HH:MM-HH:MM"
// (the pacing.active_window / warmup.active_hours grammar). end <= start
// means the window crosses midnight (e.g. "22:00-06:00"); start == end is
// grammatically valid. Validation is fail-open nowhere: an unparsable
// window is a config error, not an "always active" fallback.
func ParseActiveWindow(s string) (start, end time.Duration, err error) {
	startS, endS, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("missing \"-\" separator: want \"HH:MM-HH:MM\"")
	}
	start, err = parseClockStrict(startS)
	if err != nil {
		return 0, 0, err
	}
	end, err = parseClockStrict(endS)
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

func parseClockStrict(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("bad clock time %q", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}
