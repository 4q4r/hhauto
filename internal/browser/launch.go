// launch.go carries the humanize launch profile (T32): the careful
// preset's conservative stealth flag set and the action pacing the
// consumers apply between fill operations. The flag set mirrors what the
// cloakbrowser wrappers do by default (stealth_args=True upstream):
// AutomationControlled stays off and chromedp's own automation switch is
// dropped — both are functionality-preserving (no page behavior changes).

package browser

import (
	"context"
	"fmt"
	"math/rand"
	"time"
	"unicode/utf16"
)

// StealthFlag is one Chromium launch flag; bool false removes/omits the
// switch, other values are rendered as --name=value by the consumer
// (chromedp.Flag semantics).
type StealthFlag struct {
	Name  string
	Value any
}

// StealthFlags maps a preset onto the launch flag set. The default preset
// adds nothing (plain chromedp behavior); careful adds the conservative
// stealth pair.
func StealthFlags(preset string) ([]StealthFlag, error) {
	switch preset {
	case "", PresetDefault:
		return nil, nil
	case PresetCareful:
		return []StealthFlag{
			// Hide the automation signal from Blink
			// (navigator.webdriver stays false).
			{Name: "disable-blink-features", Value: "AutomationControlled"},
			// Drop chromedp's default --enable-automation switch (it
			// re-enables the webdriver signal and the automation infobar).
			{Name: "enable-automation", Value: false},
		}, nil
	default:
		return nil, fmt.Errorf("browser: неизвестный preset %q (ожидалось default | careful)", preset)
	}
}

// PaceKind enumerates the humanize delay slots.
type PaceKind int

const (
	// PaceFocus precedes the first keystroke into a field (smooth focus).
	PaceFocus PaceKind = iota
	// PaceType separates typed chunks inside one field.
	PaceType
	// PaceAction separates discrete actions (ops, screens, saves).
	PaceAction
)

// Cadence holds the humanize timing envelope for one preset: Focus/Type/
// Action are [min,max] delay ranges, Chunk is the typed chunk size and
// MaxTyped caps how many leading characters are typed with cadence before
// the rest of the value is set in one batch (a form-filler must not spend
// minutes on a long textarea — the final value and events are identical).
type Cadence struct {
	Focus    [2]time.Duration
	Type     [2]time.Duration
	Action   [2]time.Duration
	Chunk    int
	MaxTyped int
}

// CadenceFor returns the timing envelope for a preset ("" = default).
func CadenceFor(preset string) (Cadence, error) {
	switch preset {
	case "", PresetDefault:
		return Cadence{
			Focus:    [2]time.Duration{30 * time.Millisecond, 90 * time.Millisecond},
			Type:     [2]time.Duration{10 * time.Millisecond, 40 * time.Millisecond},
			Action:   [2]time.Duration{15 * time.Millisecond, 60 * time.Millisecond},
			Chunk:    4,
			MaxTyped: 240,
		}, nil
	case PresetCareful:
		return Cadence{
			Focus:    [2]time.Duration{80 * time.Millisecond, 200 * time.Millisecond},
			Type:     [2]time.Duration{30 * time.Millisecond, 90 * time.Millisecond},
			Action:   [2]time.Duration{60 * time.Millisecond, 180 * time.Millisecond},
			Chunk:    3,
			MaxTyped: 160,
		}, nil
	default:
		return Cadence{}, fmt.Errorf("browser: неизвестный preset %q (ожидалось default | careful)", preset)
	}
}

// Pacer draws randomized delays from a Cadence (the jitter is the point:
// fixed intervals are as machine-readable as zero delay).
type Pacer struct {
	c   Cadence
	rng *rand.Rand
}

// NewPacer builds a pacer for the preset; seed drives the jitter source
// (consumers pass time-based seeds, tests pass fixed ones).
func NewPacer(preset string, seed int64) (*Pacer, error) {
	c, err := CadenceFor(preset)
	if err != nil {
		return nil, err
	}
	return &Pacer{c: c, rng: rand.New(rand.NewSource(seed))}, nil
}

// delay draws one duration from the [min,max] range.
func (p *Pacer) delay(r [2]time.Duration) time.Duration {
	span := r[1] - r[0]
	if span <= 0 {
		return r[0]
	}
	return r[0] + time.Duration(p.rng.Int63n(int64(span)))
}

// FocusDelay precedes typing into a field.
func (p *Pacer) FocusDelay() time.Duration { return p.delay(p.c.Focus) }

// TypeDelay separates typed chunks.
func (p *Pacer) TypeDelay() time.Duration { return p.delay(p.c.Type) }

// ActionDelay separates discrete actions.
func (p *Pacer) ActionDelay() time.Duration { return p.delay(p.c.Action) }

// Chunk is the typed chunk size for the preset.
func (p *Pacer) Chunk() int { return p.c.Chunk }

// MaxTyped caps the cadence-typed prefix length.
func (p *Pacer) MaxTyped() int { return p.c.MaxTyped }

// TypeChunk is one cadence step of a humanized text entry: the half-open
// UTF-16-unit range [From,To) of the value is set in one event batch
// (UTF-16 units because the consumer-side JS slices with String.slice).
type TypeChunk struct {
	From int
	To   int
}

// TypePlan splits value into typed chunks: Chunk-sized steps; once
// MaxTyped units have been typed with cadence, the remainder collapses
// into one final chunk — a form filler must not spend minutes on a long
// textarea, and the end state (value + events) is identical either way.
func TypePlan(value string, chunk, maxTyped int) []TypeChunk {
	n := len(utf16.Encode([]rune(value)))
	if n == 0 {
		return nil
	}
	if chunk <= 0 {
		chunk = n
	}
	var plan []TypeChunk
	typed := 0
	for from := 0; from < n; {
		to := from + chunk
		if typed >= maxTyped {
			to = n // cap reached: the rest lands in one batch
		}
		if to > n {
			to = n
		}
		plan = append(plan, TypeChunk{From: from, To: to})
		typed += to - from
		from = to
	}
	return plan
}

// Wait sleeps the slot's delay, honoring cancellation (the page timeout
// is the run's bound — pacing never extends it).
func (p *Pacer) Wait(ctx context.Context, kind PaceKind) error {
	var d time.Duration
	switch kind {
	case PaceFocus:
		d = p.FocusDelay()
	case PaceType:
		d = p.TypeDelay()
	default:
		d = p.ActionDelay()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
