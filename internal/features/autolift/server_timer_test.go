package autolift

// T36 server_timer mode: lifts are planned from the hh shard's
// nextTouchAt (per resume: nextTouchAt, or updated_at + 4h when empty).
// One server computation at start / after relogin; afterwards a plain
// local 4h count from the last lift + jitter — no shard re-fetch between
// lifts.

import (
	"bytes"
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"

	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
)

// --- harness pieces ---

type armed struct {
	d  time.Duration
	fn func()
}

// captureQueue records enqueues; nothing is ever dequeued.
type captureQueue struct {
	mu    sync.Mutex
	tasks []queue.Task
}

func (q *captureQueue) Enqueue(_ context.Context, t queue.Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tasks = append(q.tasks, t)
	return nil
}
func (q *captureQueue) Dequeue(context.Context, string) (queue.Task, bool) {
	return queue.Task{}, false
}
func (q *captureQueue) Len(string) int                             { return 0 }
func (q *captureQueue) NextDeadline(string) (time.Time, bool)      { return time.Time{}, false }
func (q *captureQueue) Close(context.Context, time.Duration) error { return nil }

// stFeature builds the feature with a captured arm callback, a capture
// queue and a mutable clock (move time with the returned setter).
func stFeature(t *testing.T, h *harness, q *captureQueue, armedCalls *[]armed) (*Feature, func(time.Time)) {
	t.Helper()
	cur := now
	f := New(Options{
		Config:   h.cfg,
		DB:       h.db,
		APIFor:   func(string) (hhapi.HHAPI, error) { return h.fake.api, nil },
		ChainFor: func(string) (captcha.Chain, bool) { return h.chain, true },
		Queue:    q,
		AfterFunc: func(d time.Duration, fn func()) *time.Timer {
			*armedCalls = append(*armedCalls, armed{d: d, fn: fn})
			return nil
		},
		Now: func() time.Time { return cur },
		Log: quietLogger(),
	})
	return f, func(t time.Time) { cur = t }
}

func lastArmed(calls []armed) armed {
	if len(calls) == 0 {
		return armed{d: -1}
	}
	return calls[len(calls)-1]
}

func liftable(hash string, updated time.Time) fakeResume {
	return fakeResume{Hash: hash, Title: "Go dev", Status: "published", Searchable: true, Updated: updated}
}

// --- tests ---

// Start pass: nextTouchAt in the future plans a lift exactly at that moment
// (+ jitter, zero here) and lifts nothing.
func TestServerTimerFutureNextTouchArmsExactMoment(t *testing.T) {
	h := newHarness(t, nil, liftable("hash1", now.Add(-1*time.Hour)))
	h.fake.resumes[0].NextTouch = now.Add(2 * time.Hour)
	var calls []armed
	f, _ := stFeature(t, h, &captureQueue{}, &calls)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 0 {
		t.Fatalf("touch calls = %d, want 0 (timer in the future)", h.fake.touch)
	}
	if got := lastArmed(calls); got.d != 2*time.Hour {
		t.Fatalf("armed delay = %v, want exactly 2h (nextTouchAt)", got.d)
	}
}

// Start pass: nextTouchAt in the past lifts immediately; the follow-up
// timer is a plain local now + 4h (+ jitter).
func TestServerTimerPastNextTouchLiftsNowAndArms4h(t *testing.T) {
	h := newHarness(t, nil, liftable("hash1", now.Add(-5*time.Hour)))
	h.fake.resumes[0].NextTouch = now.Add(-30 * time.Minute)
	var calls []armed
	f, _ := stFeature(t, h, &captureQueue{}, &calls)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 1 {
		t.Fatalf("touch calls = %d, want 1 (timer elapsed)", h.fake.touch)
	}
	if got := lastArmed(calls); got.d != timer4h {
		t.Fatalf("armed delay = %v, want exactly 4h (local count after lift)", got.d)
	}
}

// Empty nextTouchAt falls back to updated_at + 4h per resume.
func TestServerTimerEmptyNextTouchUsesUpdatedPlus4h(t *testing.T) {
	// hash0 anchors the earliest plan moment (now+2h).
	h := newHarness(t, nil,
		liftable("hash0", now.Add(-1*time.Hour)),
		liftable("hash1", now.Add(-5*time.Hour)), // elapsed → due now
		liftable("hash2", now.Add(-1*time.Hour)), // plan at now+3h
	)
	h.fake.resumes[0].NextTouch = now.Add(2 * time.Hour)
	var calls []armed
	f, _ := stFeature(t, h, &captureQueue{}, &calls)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 1 {
		t.Fatalf("touch calls = %d, want 1 (updated_at + 4h elapsed)", h.fake.touch)
	}
	// earliest plan: hash0 at now+2h (hash2 is now+3h) → armed at 2h.
	if got := lastArmed(calls); got.d != 2*time.Hour {
		t.Fatalf("armed delay = %v, want 2h (earliest plan)", got.d)
	}
}

// Timer passes run from the local plan: no shard re-fetch between lifts,
// and after a lift the next timer is a local now + 4h + jitter.
func TestServerTimerTimerPassDoesNotRefetchAndCountsLocally(t *testing.T) {
	h := newHarness(t, nil, liftable("hash1", now.Add(-1*time.Hour)))
	h.fake.resumes[0].NextTouch = now.Add(2 * time.Hour)
	var calls []armed
	f, setNow := stFeature(t, h, &captureQueue{}, &calls)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("start pass: %v", err)
	}
	if h.fake.lists != 1 {
		t.Fatalf("start pass shard reads = %d, want 1", h.fake.lists)
	}

	// Mid-window pass: purely local, no reads, no lifts, re-armed to the
	// same plan moment.
	setNow(now.Add(30 * time.Minute))
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("timer pass: %v", err)
	}
	if h.fake.lists != 1 {
		t.Fatalf("timer pass re-fetched the shard (reads = %d, want 1)", h.fake.lists)
	}
	if h.fake.touch != 0 {
		t.Fatalf("timer pass lifted early (touches = %d)", h.fake.touch)
	}
	if got := lastArmed(calls); got.d != 90*time.Minute {
		t.Fatalf("re-armed delay = %v, want 90m (same plan moment)", got.d)
	}

	// Wake moment: the armed task fires → the pass lifts without a plan
	// rebuild; verify still reads once (integrity), and the next timer is
	// now + 4h from the lift.
	setNow(now.Add(2 * time.Hour))
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("wake pass: %v", err)
	}
	if h.fake.touch != 1 {
		t.Fatalf("wake pass touches = %d, want 1", h.fake.touch)
	}
	if h.fake.lists != 2 {
		t.Fatalf("shard reads after wake = %d, want 2 (verify only, no plan re-fetch)", h.fake.lists)
	}
	if got := lastArmed(calls); got.d != timer4h {
		t.Fatalf("post-lift armed delay = %v, want exactly 4h", got.d)
	}
	// The lift updated the store (verify write-through).
	rows, err := h.db.ResumesListByAccount(context.Background(), "acc1")
	if err != nil || len(rows) != 1 || rows[0].UpdatedAt.Before(now.Add(2*time.Hour).Add(-time.Minute)) {
		t.Fatalf("store updated_at not fresh: %+v err=%v", rows, err)
	}
}

// Jitter is sampled in [min,max] after the plan moment (antibot delay).
func TestServerTimerJitterSampledAfterNextTouch(t *testing.T) {
	mk := func(seed int64) time.Duration {
		h := newHarness(t, func(c *config.Config) { c.Features.AutoLift.Jitter = "20-40m" },
			liftable("hash1", now.Add(-1*time.Hour)))
		h.fake.resumes[0].NextTouch = now.Add(2 * time.Hour)
		var calls []armed
		f, _ := stFeature(t, h, &captureQueue{}, &calls)
		f.opt.Rand = rand.New(rand.NewSource(seed))
		if err := f.RunAccount(context.Background(), "acc1"); err != nil {
			t.Fatalf("RunAccount: %v", err)
		}
		return lastArmed(calls).d
	}
	for _, seed := range []int64{7, 42} {
		d := mk(seed)
		if d <= 2*time.Hour+20*time.Minute || d > 2*time.Hour+40*time.Minute {
			t.Fatalf("seed %d: armed delay = %v, want in (2h20m, 2h40m]", seed, d)
		}
	}
	a, b := mk(7), mk(7)
	if a != b {
		t.Fatalf("same seed produced different delays: %v vs %v", a, b)
	}
}

// Dry-run shows the planned lift time and mutates nothing (no touch, no arm).
func TestServerTimerDryRunPlannedNoMutation(t *testing.T) {
	var buf bytes.Buffer
	h := newHarness(t, func(c *config.Config) { c.General.DryRun = true },
		liftable("future", now.Add(-1*time.Hour)),
		liftable("due", now.Add(-5*time.Hour)),
	)
	h.fake.resumes[0].NextTouch = now.Add(2 * time.Hour)
	h.fake.resumes[1].NextTouch = now.Add(-15 * time.Minute)
	var calls []armed
	f, _ := stFeature(t, h, &captureQueue{}, &calls)
	f.log = slog.New(slog.NewTextHandler(&buf, nil))

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("RunAccount: %v", err)
	}
	if h.fake.touch != 0 {
		t.Fatalf("touch calls = %d, want 0 (dry-run)", h.fake.touch)
	}
	if len(calls) != 0 {
		t.Fatalf("dry-run armed a timer (%+v), want none", calls)
	}
	for _, want := range []string{"planned", "resume=future", "resume=due"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Fatalf("dry-run log missing %q:\n%s", want, buf.String())
		}
	}
}

// After ErrLoggedOut the plan is invalidated: the next pass recomputes from
// the server (fresh shard read) after relogin.
func TestServerTimerReloginInvalidatesPlan(t *testing.T) {
	h := newHarness(t, nil, liftable("hash1", now.Add(-1*time.Hour)))
	h.fake.resumes[0].NextTouch = now.Add(2 * time.Hour)
	var calls []armed
	f, setNow := stFeature(t, h, &captureQueue{}, &calls)

	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("start pass: %v", err)
	}
	if h.fake.lists != 1 {
		t.Fatalf("start pass shard reads = %d, want 1", h.fake.lists)
	}

	// Timer fires at the wake moment, but hh answers the touch with a
	// login redirect → account stopped, plan dropped.
	setNow(now.Add(2 * time.Hour))
	h.fake.touchErr = func(int) error {
		return &touchError{redirect: "/account/login?backurl=%2Fapplicant%2Fresumes"}
	}
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("wake pass: %v", err)
	}

	// Next pass (after relogin) must recompute from the server: a fresh
	// shard read (2) plus the verify read of the successful re-lift (3).
	h.fake.touchErr = nil
	if err := f.RunAccount(context.Background(), "acc1"); err != nil {
		t.Fatalf("post-relogin pass: %v", err)
	}
	if h.fake.lists != 3 {
		t.Fatalf("shard reads = %d, want 3 (plan invalidated by logout → recompute + verify)", h.fake.lists)
	}
}
