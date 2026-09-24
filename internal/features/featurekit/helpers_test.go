package featurekit

import "time"

// timeAt builds a UTC time for tests.
func timeAt(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}
