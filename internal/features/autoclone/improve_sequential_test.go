package autoclone

// T41: the improve plan's AI calls are strictly sequential — one field
// target at a time, in plan order, with the configured pause between
// calls. Back-to-back or overlapping provider requests trip the Z.AI
// HTTP 429 rate limit and mark every field error; the owner asked for
// strictly sequential, spaced requests.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"hhauto/internal/ai"
)

// seqRunner records every RunWithSystem call: the observed field order,
// each call's [start, end] wall span, and the peak in-call concurrency
// (must stay 1 — a second concurrent call would bump it).
type seqRunner struct {
	mu      sync.Mutex
	order   []string       // field ids in observed call order
	spans   [][2]time.Time // [start, end] per call, append order
	maxLive int
	live    int
}

func (r *seqRunner) Run(context.Context, string, string, string, string, ai.TaskName, any) (json.RawMessage, ai.Decision, error) {
	return nil, ai.Decision{}, errors.New("not used")
}

func (r *seqRunner) RunWithSystem(_ context.Context, _, _, _, _ string, _ ai.TaskName, _ string, input any) (json.RawMessage, ai.Decision, error) {
	in, ok := input.(ai.ImproveInput)
	if !ok {
		return nil, ai.Decision{}, errors.New("unexpected input type")
	}
	r.mu.Lock()
	r.live++
	if r.live > r.maxLive {
		r.maxLive = r.live
	}
	r.order = append(r.order, in.Field)
	start := time.Now()
	r.mu.Unlock()

	time.Sleep(2 * time.Millisecond) // hold the call open: an overlap would trip maxLive

	r.mu.Lock()
	r.spans = append(r.spans, [2]time.Time{start, time.Now()})
	r.live--
	r.mu.Unlock()
	// unchanged verdict: a full AI call, no apply step, no fixtures needed.
	return json.RawMessage(`{"unchanged":true}`), ai.Decision{Task: ai.TaskResumeImprove}, nil
}

// TestImproveFieldCallsAreSequential pins the T41 rate-limit contract: N
// field targets produce N strictly sequential AI calls in plan order with
// at least InterCallPause between consecutive calls.
func TestImproveFieldCallsAreSequential(t *testing.T) {
	cfg := baseImproveCfg()
	cfg.Fields = []string{"title", "about", "key_skills", "experience_description"}
	const pause = 20 * time.Millisecond

	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	db := testStore(t)
	run := &seqRunner{}
	im, err := New(Options{
		API: newFakeEditor(t), Runner: run, DB: db, Registry: reg,
		Cfg:            cfg,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:            func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
		InterCallPause: pause,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seedClone(t, db, "parent", "clone")

	artifact, err := im.ImproveClone(context.Background(), "acc1", "parent", "clone")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}

	// The fixture carries two experience items → 5 AI targets total.
	// Every call's counter grew monotonically: the observed order IS the
	// plan order.
	wantOrder := []string{
		"title", "about", "key_skills",
		"experience_description", "experience_description",
	}
	if len(run.order) != len(wantOrder) {
		t.Fatalf("AI call count = %d, want %d (order seen: %v)", len(run.order), len(wantOrder), run.order)
	}
	for i, want := range wantOrder {
		if run.order[i] != want {
			t.Fatalf("call %d = field %q, want %q (plan order broken: %v)", i, run.order[i], want, run.order)
		}
	}

	// No parallelism: the peak concurrent-call counter never left 1.
	if run.maxLive != 1 {
		t.Fatalf("max concurrent AI calls = %d, want 1 (calls overlapped)", run.maxLive)
	}

	// Spacing: every call started at least InterCallPause after the
	// previous one ended (back-to-back calls are exactly what 429s).
	for i := 1; i < len(run.spans); i++ {
		gap := run.spans[i][0].Sub(run.spans[i-1][1])
		if gap < pause {
			t.Fatalf("call %d started %v after call %d ended; want >= %v", i, gap, i-1, pause)
		}
	}

	// All unchanged verdicts park as unchanged — nothing applied.
	for i := range artifact.Fields {
		if artifact.Fields[i].Status != StatusUnchanged {
			t.Fatalf("field %s: status = %q, want %q", artifact.Fields[i].Locator, artifact.Fields[i].Status, StatusUnchanged)
		}
	}
}

// TestInterCallPauseDefault pins the production default: an Options left
// zero resolves to the 2s pause (the Z.AI rate-limit spacing).
func TestInterCallPauseDefault(t *testing.T) {
	cfg := baseImproveCfg()
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	im, err := New(Options{
		API: newFakeEditor(t), Runner: &seqRunner{}, DB: testStore(t), Registry: reg,
		Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if im.opt.InterCallPause != defaultFieldCallPause {
		t.Fatalf("default InterCallPause = %v, want %v", im.opt.InterCallPause, defaultFieldCallPause)
	}
	if defaultFieldCallPause != 2*time.Second {
		t.Fatalf("defaultFieldCallPause = %v, want 2s", defaultFieldCallPause)
	}
}
