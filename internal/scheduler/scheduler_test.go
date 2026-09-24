package scheduler

import (
	"errors"
	"math/rand"
	"testing"
	"time"

	// Hermetic timezone data: Next/Validate must not depend on the host
	// having /usr/share/zoneinfo installed.
	_ "time/tzdata"
)

func TestParseDayMask(t *testing.T) {
	s, err := Parse("Europe/Moscow", []string{"09:00", "22:30"}, []string{"mon", " tue"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Days != Mon|Tue {
		t.Errorf("Days = %04b, want Mon|Tue (%04b)", s.Days, Mon|Tue)
	}
	if len(s.Times) != 2 || s.Times[0] != "09:00" || s.Times[1] != "22:30" {
		t.Errorf("Times = %v, want [09:00 22:30]", s.Times)
	}
	if s.TZ != "Europe/Moscow" {
		t.Errorf("TZ = %q", s.TZ)
	}

	// Uppercase day names are accepted.
	s, err = Parse("", []string{"09:00"}, []string{"SUN"})
	if err != nil {
		t.Fatalf("Parse(SUN): %v", err)
	}
	if s.Days != Sun {
		t.Errorf("Days = %04b, want Sun (%04b)", s.Days, Sun)
	}

	// Empty day list means every day.
	s, err = Parse("", []string{"08:00"}, nil)
	if err != nil {
		t.Fatalf("Parse(no days): %v", err)
	}
	if s.Days != All {
		t.Errorf("Days = %04b, want All (%04b)", s.Days, All)
	}

	// Pinned leniency: time.Parse("15:04") accepts single-digit hours, so
	// "9:30" is a valid time here.
	if _, err = Parse("", []string{"9:30"}, nil); err != nil {
		t.Errorf("Parse(9:30) = %v, want nil (lenient hour parsing)", err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name  string
		tz    string
		times []string
		days  []string
	}{
		{"bad day", "", []string{"09:00"}, []string{"funday"}},
		{"no times", "", nil, []string{"mon"}},
		{"empty times", "", []string{}, nil},
		{"hour out of range", "", []string{"24:00"}, nil},
		{"minute out of range", "", []string{"09:60"}, nil},
		{"garbage time", "", []string{"morning"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.tz, tc.times, tc.days); !errors.Is(err, ErrBadSpec) {
				t.Fatalf("Parse(%q, %v, %v) err = %v, want ErrBadSpec", tc.tz, tc.times, tc.days, err)
			}
		})
	}
}

func TestNextRespectsTimezone(t *testing.T) {
	// 2026-09-24 is a Thursday. 06:00 UTC == 09:00 Europe/Moscow.
	base := time.Date(2026, time.September, 24, 6, 0, 0, 0, time.UTC)

	// Europe/Moscow: 09:00 local was exactly `base`, so strictly-after gives
	// tomorrow 09:00 MSK == 2026-09-25 06:00 UTC.
	msk := Spec{TZ: "Europe/Moscow", Times: []string{"09:00"}, Days: All}
	got := msk.Next(base, nil)
	want := time.Date(2026, time.September, 25, 6, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Moscow Next = %v, want %v", got, want)
	}

	// Same day when base is before the window: 05:00 UTC is 08:00 MSK.
	got = msk.Next(base.Add(-time.Hour), nil)
	want = base
	if !got.Equal(want) {
		t.Fatalf("Moscow same-day Next = %v, want %v", got, want)
	}

	// UTC spec at the same instant differs by the offset: 09:00 UTC is
	// strictly after 06:00 UTC, so the same-day window applies.
	utc := Spec{TZ: "UTC", Times: []string{"09:00"}, Days: All}
	got = utc.Next(base, nil)
	want = time.Date(2026, time.September, 24, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("UTC Next = %v, want %v", got, want)
	}
}

func TestNextWeekdayMask(t *testing.T) {
	// Only Mondays: from Thursday 2026-09-24 12:00 UTC the next candidate is
	// Monday 2026-09-28 09:00 UTC.
	s := Spec{TZ: "UTC", Times: []string{"09:00"}, Days: Mon}
	after := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	got := s.Next(after, nil)
	want := time.Date(2026, time.September, 28, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
	if got.Weekday() != time.Monday {
		t.Fatalf("weekday = %v, want Monday", got.Weekday())
	}

	// Weekends only: Friday evening must land on Saturday.
	s = Spec{TZ: "UTC", Times: []string{"10:00"}, Days: Sat | Sun}
	got = s.Next(time.Date(2026, time.September, 25, 23, 0, 0, 0, time.UTC), nil)
	want = time.Date(2026, time.September, 26, 10, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("weekend Next = %v, want %v", got, want)
	}
}

func TestNextStrictlyAfter(t *testing.T) {
	s := Spec{TZ: "UTC", Times: []string{"09:00"}, Days: All}
	at := time.Date(2026, time.September, 24, 9, 0, 0, 0, time.UTC)
	got := s.Next(at, nil)
	want := time.Date(2026, time.September, 25, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next at exact candidate = %v, want strictly after: %v", got, want)
	}
	if !got.Equal(want) || !got.After(at) {
		t.Fatalf("Next must be strictly after %v", at)
	}
}

func TestNextJitterWithFixedRand(t *testing.T) {
	const jitter = 40 * time.Minute
	s := Spec{TZ: "UTC", Times: []string{"09:00"}, Days: All, Jitter: jitter}
	after := time.Date(2026, time.September, 24, 8, 0, 0, 0, time.UTC)
	base := time.Date(2026, time.September, 24, 9, 0, 0, 0, time.UTC)

	rnd := rand.New(rand.NewSource(7))
	got := s.Next(after, rnd)

	// Same seed, same draw: jitter must be exactly the first Int63n value.
	rr := rand.New(rand.NewSource(7))
	want := base.Add(time.Duration(rr.Int63n(int64(jitter) + 1)))
	if !got.Equal(want) {
		t.Fatalf("jittered Next = %v, want %v (deterministic for fixed rnd)", got, want)
	}

	// Range sanity across several seeds: jitter in [0, Jitter], always
	// strictly after `after`.
	for seed := int64(1); seed <= 50; seed++ {
		got := s.Next(after, rand.New(rand.NewSource(seed)))
		d := got.Sub(base)
		if d < 0 || d > jitter {
			t.Fatalf("seed %d: jitter offset %v outside [0, %v]", seed, d, jitter)
		}
		if !got.After(after) {
			t.Fatalf("seed %d: Next %v not strictly after %v", seed, got, after)
		}
	}
}

func TestNextZeroSpecReturnsAfter(t *testing.T) {
	// Days=0 matches no weekday; the bounded forward search exhausts and the
	// fallback is `after` itself.
	after := time.Date(2026, time.September, 24, 8, 0, 0, 0, time.UTC)
	got := (Spec{TZ: "UTC", Times: []string{"09:00"}}).Next(after, nil)
	if !got.Equal(after) {
		t.Fatalf("zero-day spec Next = %v, want %v unchanged", got, after)
	}
}

func TestValidate(t *testing.T) {
	if err := (Spec{TZ: "Europe/Moscow", Times: []string{"09:00", "23:59"}}).Validate(); err != nil {
		t.Fatalf("Validate(valid) = %v, want nil", err)
	}
	// Empty TZ means local time and is valid.
	if err := (Spec{Times: []string{"00:00"}}).Validate(); err != nil {
		t.Fatalf("Validate(local) = %v, want nil", err)
	}
	if err := (Spec{TZ: "Mars/Olympus", Times: []string{"09:00"}}).Validate(); err == nil {
		t.Fatal("Validate(bad tz) = nil, want error")
	}
	if err := (Spec{TZ: "UTC"}).Validate(); !errors.Is(err, ErrBadSpec) {
		t.Fatalf("Validate(no times) = %v, want ErrBadSpec", err)
	}
	if err := (Spec{TZ: "UTC", Times: []string{"09:60"}}).Validate(); err == nil {
		t.Fatal("Validate(bad time) = nil, want error")
	}
}
