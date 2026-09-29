package autolift

// T69: the lift success line — «↑ Поднято: «титул» · следующий подъём
// ~HH:MM» straight from the local plan; the [console].lift toggle gates it.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/console"
)

// withConsole installs a plain console surface on the harness (T69
// defaults: all line types on) and returns the buffer.
func (h *harness) withConsole(mutate func(*config.Console)) *bytes.Buffer {
	buf := &bytes.Buffer{}
	def := true
	cfg := config.Console{
		Applied: &def, Errors: &def, Letters: &def, Lift: &def,
		Skips: "interesting,aggregate",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h.console, _ = console.New(buf, false, cfg, nil)
	return buf
}

// A successful discover-pass lift renders the success line with the
// resume title and the plan moment.
func TestLiftConsoleLineAfterDiscoverPass(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Title: "Go dev", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	buf := h.withConsole(nil)
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "↑ Поднято: «Go dev»") {
		t.Fatalf("lift line missing: %q", out)
	}
	if !strings.Contains(out, "следующий подъём ~") {
		t.Fatalf("NextAt segment missing: %q", out)
	}
	// One line per lift (one banner per event).
	if got := strings.Count(out, "↑ Поднято"); got != 1 {
		t.Fatalf("lift lines = %d, want 1", got)
	}
}

// lift=false silences the line; the lift itself still happens.
func TestLiftConsoleToggleOff(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Title: "Go dev", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	buf := h.withConsole(func(c *config.Console) {
		off := false
		c.Lift = &off
	})
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}

	if buf.Len() != 0 {
		t.Fatalf("lift=false but rendered: %q", buf.String())
	}
	if h.fake.touch != 1 {
		t.Fatalf("touch calls = %d, want 1 (the toggle gates rendering only)", h.fake.touch)
	}
}

// The NextAt in the line comes from the freshly installed plan: now + 4h
// (+ jitter; the deterministic Rand keeps it 0 here).
func TestLiftConsoleNextAtFromPlan(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "hash1", Title: "Go dev", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	buf := h.withConsole(nil)
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}

	plan := f.currentPlan("acc1")
	at, ok := plan["hash1"]
	if !ok {
		t.Fatalf("plan missing hash1: %v", plan)
	}
	want := at.Format("02.01 15:04")
	if !strings.Contains(buf.String(), "~"+want) {
		t.Fatalf("lift line misses the plan moment %s: %q", want, buf.String())
	}
}

// A title-less shard falls back to the short hash on the line.
func TestLiftConsoleShortHashFallback(t *testing.T) {
	h := newHarness(t, nil,
		fakeResume{Hash: "fe31e03eff110666c30039ed1f4f5a59327851", Title: "", Status: "published", Searchable: true, Updated: now.Add(-5 * time.Hour)})
	buf := h.withConsole(nil)
	f := h.feature(t)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}

	if !strings.Contains(buf.String(), "↑ Поднято: «fe31e03e»") {
		t.Fatalf("short-hash fallback missing: %q", buf.String())
	}
}
