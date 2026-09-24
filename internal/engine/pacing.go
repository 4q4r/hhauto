// Package engine is the runtime core: account supervisors, the scheduler
// loop, the three-channel kill switch and webhook outbox redelivery. It
// owns NO business logic — features register schedules and task handlers
// here, the engine drives them.
//
// Dependency direction: cli → engine → features → {hhapi, store, ai}.
// Features cannot import engine (cycle), so the CLI wires feature schedules
// into engine.RegisterSchedule / engine.RegisterHandler.
package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"hhauto/internal/config"
)

// Pacer spaces account mutations in time: human-like pacing, the active
// window and the nightly pause live here. Handlers call Wait right before
// a mutation; the engine only hands the Pacer out via TaskContext.
type Pacer interface {
	// Wait blocks until the next mutation slot for accountID is due.
	// Returns ctx.Err() when the context is cancelled while waiting.
	Wait(ctx context.Context, accountID string) error
}

// DefaultExpectedDaily mirrors config.Account.DailyApplyCap's default: the
// adaptive mode divides the active window by this when an account has no
// explicit cap registered via SetExpectedDaily.
const DefaultExpectedDaily = 200

// ParseDurationRange parses a "min-max" duration range used by pacing and
// feature jitter settings: "20-40m" (unit taken from the right bound),
// "30s-5m" (both bounds explicit) or a single "90s" (lo == hi).
// Bare numbers ("20-40") are an error: a unit is mandatory.
func ParseDurationRange(s string) (lo, hi time.Duration, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, errors.New("engine: empty duration range")
	}
	loS, hiS, hasDash := strings.Cut(s, "-")
	if !hasDash {
		d, perr := time.ParseDuration(s)
		if perr != nil {
			return 0, 0, fmt.Errorf("engine: bad duration %q: %w", s, perr)
		}
		if d < 0 {
			return 0, 0, fmt.Errorf("engine: negative duration %q", s)
		}
		return d, d, nil
	}
	hi, perr := time.ParseDuration(hiS)
	if perr != nil {
		return 0, 0, fmt.Errorf("engine: bad duration range %q: %w", s, perr)
	}
	if hi < 0 {
		return 0, 0, fmt.Errorf("engine: negative duration range %q", s)
	}
	// "20-40m": the left bound carries no unit — borrow the right bound's.
	lo, lerr := time.ParseDuration(loS)
	if lerr != nil {
		unit := durationUnit(hiS)
		if unit == "" {
			return 0, 0, fmt.Errorf("engine: bad duration range %q: left bound %q has no unit", s, loS)
		}
		lo, lerr = time.ParseDuration(loS + unit)
		if lerr != nil {
			return 0, 0, fmt.Errorf("engine: bad duration range %q: %w", s, lerr)
		}
	}
	if lo < 0 || lo > hi {
		return 0, 0, fmt.Errorf("engine: bad duration range %q: need 0 <= min <= max", s)
	}
	return lo, hi, nil
}

// durationUnit returns the single trailing unit of a duration string
// ("40m" -> "m", "1.5h" -> "h"). Everything else ("1h30m", "1h", "") returns
// "": the borrowed-unit grammar ("20-40m") is only defined for a plain
// number with ONE unit letter, and guessing for anything richer would
// silently misparse.
func durationUnit(s string) string {
	letters := 0
	lastLetter := -1
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9' || r == '.':
		case r == 'h' || r == 'm' || r == 's' || r == 'u' || r == 'µ' || r == 'n':
			letters++
			lastLetter = i
		default:
			return ""
		}
	}
	if letters != 1 || lastLetter != len(s)-1 {
		return ""
	}
	return s[lastLetter:]
}

// --- active window ---

// window is a daily active interval [start, end) in some day's local time.
// end <= start means the window crosses midnight (e.g. "22:00-06:00").
type window struct {
	start, end time.Duration // offsets from local midnight
	present    bool
}

// parseWindow parses "08:00-22:00". An empty string means "always active".
// Anything unparsable also degrades to always-active (the pacer is
// throughput-critical and config validation is config's job); use
// parseWindowStrict in tests to check the grammar itself.
func parseWindow(s string) window {
	if s == "" {
		return window{present: false}
	}
	w, err := parseWindowStrict(s)
	if err != nil {
		return window{present: false}
	}
	return w
}

func parseWindowStrict(s string) (window, error) {
	start, end, err := config.ParseActiveWindow(s)
	if err != nil {
		return window{}, fmt.Errorf("engine: bad active window %q: %w", s, err)
	}
	return window{start: start, end: end, present: true}, nil
}

// activeAt reports whether a moment inside day `at` falls into the window.
func (w window) activeAt(at time.Time) bool {
	if !w.present {
		return true
	}
	off := time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute +
		time.Duration(at.Second())*time.Second
	if w.start <= w.end {
		return off >= w.start && off < w.end
	}
	return off >= w.start || off < w.end // crosses midnight
}

// nextOpen returns how long until the window next opens from `at`
// (zero when already open).
func (w window) nextOpen(at time.Time) time.Duration {
	if w.activeAt(at) {
		return 0
	}
	midnight := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	if w.start <= w.end {
		open := midnight.Add(w.start)
		if open.After(at) {
			return open.Sub(at)
		}
		return open.AddDate(0, 0, 1).Sub(at) // tomorrow's opening
	}
	// Crossing midnight: before start means it opens tonight; after end
	// (i.e. off >= end, before start) can only happen when off < start.
	open := midnight.Add(w.start)
	if open.After(at) {
		return open.Sub(at)
	}
	return open.AddDate(0, 0, 1).Sub(at)
}

// length returns the window's daily span; an always-active window spans 24h.
func (w window) length() time.Duration {
	if !w.present {
		return 24 * time.Hour
	}
	if w.start <= w.end {
		return w.end - w.start
	}
	return 24*time.Hour - (w.start - w.end)
}

// --- pacer ---

type accountPace struct {
	mu        sync.Mutex // serializes Wait within the account: mutations never overlap
	next      time.Time  // earliest allowed next mutation instant (adaptive)
	permits   int        // expected daily actions (adaptive)
	markEmpty bool       // permits never set: using DefaultExpectedDaily
}

type pacer struct {
	cfg   config.Pacing
	rnd   *rand.Rand
	rndMu sync.Mutex // math/rand.Rand is not goroutine-safe; serialize all draws
	win   window
	jlo   time.Duration
	jhi   time.Duration
	haveJ bool
	mu    sync.Mutex
	accts map[string]*accountPace
	now   func() time.Time // injectable clock (package tests override)
}

// NewPacer builds the account mutation pacer from [pacing] config.
// rnd may be nil (a private non-deterministic source is used); a shared
// *rand.Rand is safe here: every draw is mutex-guarded (math/rand.Rand is
// documented as not goroutine-safe).
//
// Tolerance rules (config validation is config's job): an unknown mode
// behaves as adaptive; an unparsable ActiveWindow means always-active;
// an unparsable Jitter is ignored. NightPause is a *bool (R1): nil means
// enabled, an explicit false is respected.
//
//nolint:revive // sealed impl: Wait (Pacer) is the public surface; the concrete type carries internal/test knobs
func NewPacer(cfg config.Pacing, rnd *rand.Rand) *pacer {
	if rnd == nil {
		rnd = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	p := &pacer{
		cfg:   cfg,
		rnd:   rnd,
		win:   parseWindow(cfg.ActiveWindow),
		accts: map[string]*accountPace{},
		now:   time.Now,
	}
	if lo, hi, err := ParseDurationRange(cfg.Jitter); err == nil {
		p.jlo, p.jhi, p.haveJ = lo, hi, true
	}
	return p
}

// SetExpectedDaily registers an account's expected daily action count
// (config.Account.DailyApplyCap): the adaptive interval is the active
// window length divided by it. Non-positive n falls back to
// DefaultExpectedDaily.
func (p *pacer) SetExpectedDaily(accountID string, n int) {
	if n <= 0 {
		n = DefaultExpectedDaily
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.acctLocked(accountID)
	a.permits = n
	a.markEmpty = false
}

func (p *pacer) acctLocked(accountID string) *accountPace {
	a, ok := p.accts[accountID]
	if !ok {
		a = &accountPace{permits: DefaultExpectedDaily, markEmpty: true}
		p.accts[accountID] = a
	}
	return a
}

// Wait implements Pacer.
func (p *pacer) Wait(ctx context.Context, accountID string) error {
	a := p.acct(accountID)
	a.mu.Lock() // held through the sleep: in-account mutations are sequential
	defer a.mu.Unlock()

	d := p.nextDelayLocked(a, accountID)
	if d <= 0 {
		return ctx.Err() // nothing to wait for
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *pacer) acct(accountID string) *accountPace {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.acctLocked(accountID)
}

// nextDelay computes the wait before the next mutation for testing and for
// Wait. Adaptive spacing state is advanced as a side effect.
func (p *pacer) nextDelay(accountID string) time.Duration {
	a := p.acct(accountID)
	a.mu.Lock()
	defer a.mu.Unlock()
	return p.nextDelayLocked(a, accountID)
}

func (p *pacer) nextDelayLocked(a *accountPace, _ string) time.Duration {
	now := p.now()

	// Night pause dominates every mode: outside the active window the
	// account sleeps until the window reopens.
	if nightPaused(p.cfg.NightPause) {
		if d := p.win.nextOpen(now); d > 0 {
			return d + p.jitterLocked()
		}
	}

	var base time.Duration
	switch strings.ToLower(strings.TrimSpace(p.cfg.Mode)) {
	case "burst":
		base = 0
	case "uniform":
		base = time.Duration(p.cfg.MinDelaySec) * time.Second
	default: // "adaptive" and anything unrecognized
		base = p.adaptiveDelayLocked(a, now)
	}
	return base + p.jitterLocked()
}

// adaptiveDelayLocked implements fixed-rate spacing at window/permits with
// no idle catch-up bursts: after a long pause the next mutation goes
// immediately and spacing restarts from it.
func (p *pacer) adaptiveDelayLocked(a *accountPace, now time.Time) time.Duration {
	interval := p.win.length() / time.Duration(a.permits)
	if interval <= 0 {
		return 0
	}
	if a.next.Before(now) {
		a.next = now
	}
	d := a.next.Sub(now)
	a.next = a.next.Add(interval)
	return d
}

func (p *pacer) jitterLocked() time.Duration {
	if !p.haveJ {
		return 0
	}
	p.rndMu.Lock()
	defer p.rndMu.Unlock()
	span := p.jhi - p.jlo
	if span <= 0 {
		return p.jlo
	}
	return p.jlo + time.Duration(p.rnd.Int63n(int64(span)+1))
}

func nightPaused(night *bool) bool {
	return night == nil || *night
}
