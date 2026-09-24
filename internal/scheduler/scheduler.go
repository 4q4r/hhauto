// Package scheduler computes run times from TZ + times-of-day + weekday masks,
// with one-shot jitter persisted by the caller. Pure time logic, no I/O:
// fully testable with an injected rand source.
package scheduler

import (
	"errors"
	"math/rand"
	"strings"
	"time"
)

// Weekdays is a bitmask: bit0=Mon ... bit6=Sun.
type Weekdays uint8

const (
	Mon Weekdays = 1 << iota
	Tue
	Wed
	Thu
	Fri
	Sat
	Sun
	All = Mon | Tue | Wed | Thu | Fri | Sat | Sun
)

var ErrBadSpec = errors.New("scheduler: bad spec")

// Spec is the declarative schedule used by [runs.*.windows] and features.
type Spec struct {
	TZ     string        // IANA name; "" = local
	Times  []string      // "HH:MM", one or more per day
	Days   Weekdays      // weekday mask
	Jitter time.Duration // added ONCE when Next computes, then persisted by caller
}

var dayNames = map[string]Weekdays{
	"mon": Mon, "tue": Tue, "wed": Wed, "thu": Thu,
	"fri": Fri, "sat": Sat, "sun": Sun,
}

// Parse builds a Spec from TOML-friendly values ("Europe/Moscow", ["09:30"], ["mon","tue"], "20-40m" jitter is parsed by caller).
func Parse(tz string, times, days []string) (Spec, error) {
	s := Spec{TZ: tz, Times: times}
	for _, d := range days {
		bit, ok := dayNames[strings.ToLower(strings.TrimSpace(d))]
		if !ok {
			return s, ErrBadSpec
		}
		s.Days |= bit
	}
	if len(s.Times) == 0 {
		return s, ErrBadSpec
	}
	for _, t := range s.Times {
		if _, err := time.Parse("15:04", t); err != nil {
			return s, ErrBadSpec
		}
	}
	if s.Days == 0 {
		s.Days = All
	}
	return s, nil
}

func (s Spec) Validate() error {
	if _, err := time.LoadLocation(s.tzOrLocal()); err != nil {
		return err
	}
	if len(s.Times) == 0 {
		return ErrBadSpec
	}
	for _, t := range s.Times {
		if _, err := time.Parse("15:04", t); err != nil {
			return err
		}
	}
	return nil
}

func (s Spec) tzOrLocal() string {
	if s.TZ == "" {
		return "Local"
	}
	return s.TZ
}

// Registry holds named specs; the engine's scheduler loop computes due times from it.
type Registry struct {
	items map[string]Spec
	order []string
}

func NewRegistry() *Registry { return &Registry{items: map[string]Spec{}} }

func (r *Registry) Add(name string, s Spec) {
	if _, exists := r.items[name]; !exists {
		r.order = append(r.order, name)
	}
	r.items[name] = s
}

func (r *Registry) Get(name string) (Spec, bool) {
	s, ok := r.items[name]
	return s, ok
}

func (r *Registry) Names() []string { return append([]string(nil), r.order...) }

// Next returns the next run instant strictly after `after`, with jitter applied once.
func (s Spec) Next(after time.Time, rnd *rand.Rand) time.Time {
	loc, err := time.LoadLocation(s.tzOrLocal())
	if err != nil {
		loc = time.Local
	}
	base := after.In(loc)
	for d := 0; d < 400; d++ { // bounded forward search
		day := time.Date(base.Year(), base.Month(), base.Day()+d, 0, 0, 0, 0, loc)
		if s.Days&(Weekdays(1<<uint((int(day.Weekday())+6)%7))) == 0 {
			continue
		}
		for _, t := range s.Times {
			hm, err := time.Parse("15:04", t)
			if err != nil {
				continue
			}
			cand := time.Date(day.Year(), day.Month(), day.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
			if !cand.After(base) {
				continue
			}
			if s.Jitter > 0 && rnd != nil {
				cand = cand.Add(time.Duration(rnd.Int63n(int64(s.Jitter) + 1)))
			}
			return cand
		}
	}
	return after
}
