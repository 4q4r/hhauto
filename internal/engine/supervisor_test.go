package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"hhauto/internal/audit"
	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/features"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// --- fakes ---

type fakeClock struct {
	mu     sync.Mutex
	cur    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{cur: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, &fakeTimer{at: c.cur.Add(d), ch: ch})
	return ch
}

// Advance moves the clock and fires due timers (oldest first).
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.cur = c.cur.Add(d)
	var due []*fakeTimer
	var keep []*fakeTimer
	for _, t := range c.timers {
		if !t.at.After(c.cur) {
			due = append(due, t)
		} else {
			keep = append(keep, t)
		}
	}
	c.timers = keep
	c.mu.Unlock()
	// Fire outside the lock so receivers can call Now/After reentrantly.
	for _, t := range due {
		t.ch <- c.cur // buffered cap 1: never blocks; repeated firing is a bug
	}
}

// hasPendingTimerAt reports whether a timer is registered to fire exactly
// at `at` — the deterministic "loop is parked on its tick" signal.
func (c *fakeClock) hasPendingTimerAt(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if t.at.Equal(at) {
			return true
		}
	}
	return false
}

type fakeAPI struct {
	hhapi.HHAPI // nil-embedded: unexpected calls panic loudly

	bootstrapCalls atomic.Int32
	bootstrapErr   error
}

func (f *fakeAPI) BootstrapSession(_ context.Context) error {
	f.bootstrapCalls.Add(1)
	return f.bootstrapErr
}

type fakeCookies struct {
	mu     sync.Mutex
	saved  map[string][]*http.Cookie
	loadOK bool
}

func (f *fakeCookies) Load(_ context.Context, _ string) ([]*http.Cookie, error) {
	if !f.loadOK {
		return nil, cookies.ErrNotFound
	}
	return []*http.Cookie{{Name: "hhtoken", Value: "x"}}, nil
}

func (f *fakeCookies) Save(_ context.Context, accountID string, c []*http.Cookie) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saved == nil {
		f.saved = map[string][]*http.Cookie{}
	}
	f.saved[accountID] = c
	return nil
}

func (f *fakeCookies) Delete(_ context.Context, _ string) error { return nil }
func (f *fakeCookies) Backend() string                          { return "fake" }

func (f *fakeCookies) savesFor(accountID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.saved[accountID]; ok {
		return len(c)
	}
	return -1
}

type fakeOutbox struct {
	mu        sync.Mutex
	items     []webhook.OutboxItem
	delivered []int64
	failed    map[int64]time.Time
}

func newFakeOutbox(items ...webhook.OutboxItem) *fakeOutbox {
	return &fakeOutbox{items: items, failed: map[int64]time.Time{}}
}

func (f *fakeOutbox) Add(_ string, _ []byte) error { return errors.New("not used") }

func (f *fakeOutbox) Due(_ time.Time, limit int) ([]webhook.OutboxItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]webhook.OutboxItem(nil), f.items...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeOutbox) MarkDelivered(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, id)
	var keep []webhook.OutboxItem
	for _, it := range f.items {
		if it.ID != id {
			keep = append(keep, it)
		}
	}
	f.items = keep
	return nil
}

func (f *fakeOutbox) MarkFailed(id int64, nextRetry time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = nextRetry
	return nil
}

// fakeFeature mirrors what feature packages (T10/T11) will do: add a spec to
// the scheduler registry; the factory/handler binding goes through the
// engine registration API from the wiring layer.
type fakeFeature struct{ scheduleSpec scheduler.Spec }

func (f *fakeFeature) Name() string  { return "fake" }
func (f *fakeFeature) Enabled() bool { return true }
func (f *fakeFeature) RegisterTasks(_ context.Context, _ queue.Queue, reg *scheduler.Registry) error {
	reg.Add("t12sched", f.scheduleSpec)
	return nil
}

// --- harness ---

const (
	testKind     = queue.TaskKind("t12_kind")
	testSchedule = "t12sched"
)

type harness struct {
	t        *testing.T
	dir      string
	db       *store.DB
	q        queue.Queue
	ck       *fakeCookies
	api      *fakeAPI
	notifier *recordingNotifier
	outbox   *fakeOutbox
	clock    *fakeClock
	hits     chan queue.Task
	deps     Deps
}

type recordingNotifier struct {
	mu     sync.Mutex
	events []webhook.Event
}

func (r *recordingNotifier) Emit(ev webhook.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingNotifier) Close(_ context.Context) error { return nil }

func (r *recordingNotifier) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func newHarness(t *testing.T, mutate func(*Deps)) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "test.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ck := &fakeCookies{}
	api := &fakeAPI{}
	n := &recordingNotifier{}
	clock := newFakeClock(time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC))

	// Seed account rows: production seeds them at `hhauto init` (T13
	// wiring); the engine only transitions status of existing rows.
	for _, id := range []string{"a1", "a2"} {
		if err := db.AccountsUpsert(context.Background(), store.Account{ID: id, DisplayName: id}); err != nil {
			t.Fatalf("seed account %s: %v", id, err)
		}
	}

	spec, err := scheduler.Parse("UTC", []string{"08:00"}, nil)
	if err != nil {
		t.Fatalf("scheduler.Parse: %v", err)
	}
	spec.Jitter = 0

	cfg := &config.Config{}
	cfg.General.DataDir = dir
	cfg.General.Timezone = "UTC"
	cfg.Pacing.Mode = "burst"
	cfg.Pacing.NightPause = boolPtr(false)
	cfg.Schedule.MissedRunPolicy = "run_once"
	cfg.Schedule.MaxCatchupHours = 6
	cfg.Accounts = map[string]config.Account{
		"a1": {
			DisplayName:   "Account One",
			UserAgent:     "test-agent/1.0",
			Concurrency:   1,
			DailyApplyCap: 200,
		},
		"a2": {
			DisplayName:   "Account Two",
			UserAgent:     "test-agent/2.0",
			Concurrency:   1,
			DailyApplyCap: 200,
		},
	}

	deps := Deps{
		Cfg:         cfg,
		DB:          db,
		Queue:       queue.New(),
		Feats:       &features.Registry{},
		Webhooks:    n,
		Log:         slog.New(discardHandler{}),
		CookieStore: ck,
		AIRunner:    nil,
		APIs: map[string]hhapi.HHAPI{
			"a1": api,
			"a2": api,
		},
		Clock: clock,
	}
	deps.Feats.Add(&fakeFeature{scheduleSpec: spec})

	h := &harness{
		t: t, dir: dir, db: db, q: deps.Queue, ck: ck, api: api,
		notifier: n, outbox: newFakeOutbox(), clock: clock,
		hits: make(chan queue.Task, 64),
		deps: deps,
	}
	if mutate != nil {
		mutate(&h.deps)
	}
	// Default binding: schedule + handler capturing fired tasks.
	if err := RegisterSchedule(testSchedule, spec, func(_ context.Context, accountID string) (queue.Task, error) {
		return queue.Task{
			AccountID: accountID,
			Kind:      testKind,
			Priority:  queue.PrioWarmup,
			DedupKey:  "fire",
			Payload:   "payload-" + accountID,
		}, nil
	}); err != nil {
		t.Fatalf("RegisterSchedule: %v", err)
	}
	t.Cleanup(resetRegistrations)
	return h
}

type discardHandler struct{}

func (discardHandler) Enabled(_ context.Context, _ slog.Level) bool  { return false }
func (discardHandler) Handle(_ context.Context, _ slog.Record) error { return nil }
func (d discardHandler) WithAttrs(_ []slog.Attr) slog.Handler        { return d }
func (d discardHandler) WithGroup(_ string) slog.Handler             { return d }

// run starts Run in the background and returns a stop func that cancels the
// context and waits for full shutdown.
func (h *harness) run(ctx context.Context) (stop func(), done <-chan error) {
	runCtx, cancel := context.WithCancel(ctx)
	doneCh := make(chan error, 1)
	go func() { doneCh <- Run(runCtx, h.deps) }()
	var once sync.Once
	return func() {
		once.Do(cancel)
	}, doneCh
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// --- tests ---

func TestSchedulerLoopEnqueuesDueTask(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)

	var handled atomic.Int32
	if err := RegisterHandler(testKind, func(_ context.Context, tc *TaskContext, task queue.Task) error {
		if tc.AccountID != task.AccountID {
			t.Errorf("TaskContext.AccountID = %q, want %q", tc.AccountID, task.AccountID)
		}
		if tc.DB == nil || tc.API == nil || tc.Cfg == nil || tc.Pacer == nil || tc.Webhooks == nil {
			t.Errorf("TaskContext deps missing: %+v", tc)
		}
		if task.Payload != "payload-"+task.AccountID {
			t.Errorf("payload = %v", task.Payload)
		}
		handled.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, done := h.run(ctx)
	defer stop()

	// Start-up materialization must persist a jittered next_run row.
	waitFor(t, "schedule row materialized", func() bool {
		rows, err := h.db.SchedulesDue(context.Background(), h.clock.Now().Add(24*time.Hour), 10)
		return err == nil && len(rows) == 2 // one per enabled account
	})
	// Determinism: wait until the scheduler loop is parked on its first
	// tick timer (start+30s) before moving the fake clock.
	waitFor(t, "scheduler loop parked on tick timer", func() bool {
		return h.clock.hasPendingTimerAt(h.clock.Now().Add(SchedulerTick))
	})

	// Advance past the materialized next_run (tomorrow 08:00): the next
	// scheduler tick must fire the factory, enqueue, and the workers must
	// execute both accounts.
	h.clock.Advance(26 * time.Hour)
	waitFor(t, "both accounts handled", func() bool { return handled.Load() == 2 })

	// next_run advanced past the fire time: nothing due anymore.
	due, err := h.db.SchedulesDue(context.Background(), h.clock.Now(), 10)
	if err != nil {
		t.Fatalf("SchedulesDue: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("schedules still due after fire: %d", len(due))
	}
	_ = done
}

func TestSchedulerLoopMissedRunPolicies(t *testing.T) {
	t.Run("run_once catches up an expired run", func(t *testing.T) {
		resetRegistrations()
		var fired atomic.Int32
		h := newHarness(t, nil)
		mustRegisterHandler(t, testKind, func(context.Context, *TaskContext, queue.Task) error {
			fired.Add(1)
			return nil
		})

		// Pre-seed an ancient due row (well past max_catchup_hours).
		old := h.clock.Now().Add(-48 * time.Hour)
		next := old.Add(time.Hour)
		if err := h.db.SchedulesUpsert(context.Background(), &store.Schedule{
			ID: testSchedule + "@a1", AccountID: "a1", Enabled: true,
			NextRunAt: &next, UpdatedAt: old,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop, _ := h.run(ctx)
		defer stop()

		waitFor(t, "missed run executed once", func() bool { return fired.Load() == 1 })
		if fired := fired.Load(); fired != 1 {
			t.Fatalf("run_once must fire exactly once, got %d", fired)
		}
	})

	t.Run("skip drops an expired run", func(t *testing.T) {
		resetRegistrations()
		var fired atomic.Int32
		h := newHarness(t, func(d *Deps) {
			d.Cfg.Schedule.MissedRunPolicy = "skip"
		})
		mustRegisterHandler(t, testKind, func(context.Context, *TaskContext, queue.Task) error {
			fired.Add(1)
			return nil
		})

		old := h.clock.Now().Add(-48 * time.Hour)
		next := old.Add(time.Hour)
		if err := h.db.SchedulesUpsert(context.Background(), &store.Schedule{
			ID: testSchedule + "@a1", AccountID: "a1", Enabled: true,
			NextRunAt: &next, UpdatedAt: old,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop, _ := h.run(ctx)
		defer stop()

		// The loop advances the row; the missed run is dropped.
		waitFor(t, "expired row advanced", func() bool {
			due, err := h.db.SchedulesDue(context.Background(), h.clock.Now(), 10)
			return err == nil && len(due) == 0
		})
		time.Sleep(100 * time.Millisecond)
		if n := fired.Load(); n != 0 {
			t.Fatalf("skip policy executed the missed run %d times", n)
		}
	})
}

func mustRegisterHandler(t *testing.T, kind queue.TaskKind, h TaskHandler) {
	t.Helper()
	if err := RegisterHandler(kind, h); err != nil {
		t.Fatalf("RegisterHandler: %v", err)
	}
}

func TestWorkersSequentialWithinAccountParallelAcross(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, func(d *Deps) {
		d.Cfg.Accounts["a1"] = config.Account{DisplayName: "a1", UserAgent: "ua1", Concurrency: 2, DailyApplyCap: 200}
	})
	var mu sync.Mutex
	running := map[string]int{}
	maxInAccount := map[string]int{}
	bothAccountsInside := make(chan struct{}, 1)
	var seenAccounts sync.Map

	mustRegisterHandler(t, testKind, func(_ context.Context, tc *TaskContext, _ queue.Task) error {
		mu.Lock()
		running[tc.AccountID]++
		if running[tc.AccountID] > maxInAccount[tc.AccountID] {
			maxInAccount[tc.AccountID] = running[tc.AccountID]
		}
		seenAccounts.Store(tc.AccountID, true)
		if len(running) >= 2 {
			select {
			case bothAccountsInside <- struct{}{}:
			default:
			}
		}
		mu.Unlock()

		time.Sleep(120 * time.Millisecond) // widen the race window

		mu.Lock()
		running[tc.AccountID]--
		mu.Unlock()
		return nil
	})

	// Seed 4 tasks per account directly (worker-level test: bypass schedules).
	for _, acct := range []string{"a1", "a2"} {
		for i := 0; i < 4; i++ {
			if err := h.deps.Queue.Enqueue(context.Background(), queue.Task{
				AccountID: acct, Kind: testKind, Priority: queue.PrioApply, DedupKey: fmt.Sprint(i),
			}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, _ := h.run(ctx)
	defer stop()

	select {
	case <-bothAccountsInside:
	case <-time.After(5 * time.Second):
		t.Fatal("accounts never ran in parallel")
	}
	waitFor(t, "all tasks drained", func() bool { return h.deps.Queue.Len("a1") == 0 && h.deps.Queue.Len("a2") == 0 })

	mu.Lock()
	defer mu.Unlock()
	for acct, m := range maxInAccount {
		if m > 1 {
			t.Errorf("account %s ran %d tasks in parallel, want strictly sequential", acct, m)
		}
	}
}

func TestKillSwitchStopsBeforeMutation(t *testing.T) {
	resetRegistrations()
	var executed atomic.Int32
	h := newHarness(t, nil)
	mustRegisterHandler(t, testKind, func(context.Context, *TaskContext, queue.Task) error {
		executed.Add(1)
		return nil
	})

	// STOP file in place before the engine starts.
	if err := os.WriteFile(filepath.Join(h.dir, "STOP"), []byte("stop"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, done := h.run(ctx)

	// Seed tasks: the kill switch must trip before any mutation.
	for i := 0; i < 3; i++ {
		_ = h.deps.Queue.Enqueue(context.Background(), queue.Task{
			AccountID: "a1", Kind: testKind, Priority: queue.PrioApply, DedupKey: fmt.Sprint(i),
		})
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		stop()
		t.Fatal("kill switch did not shut the engine down")
	}
	if n := executed.Load(); n != 0 {
		t.Fatalf("handler executed %d times under the kill switch, want 0", n)
	}
}

func TestLoggedOutSessionFlow(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)
	h.api.bootstrapErr = hhapi.ErrLoggedOut

	var executed atomic.Int32
	mustRegisterHandler(t, testKind, func(context.Context, *TaskContext, queue.Task) error {
		executed.Add(1)
		return nil
	})

	_ = h.deps.Queue.Enqueue(context.Background(), queue.Task{
		AccountID: "a1", Kind: testKind, Priority: queue.PrioApply, DedupKey: "one",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, _ := h.run(ctx)
	defer stop()

	waitFor(t, "account marked logged_out", func() bool {
		a, err := h.db.AccountsGet(context.Background(), "a1")
		return err == nil && a != nil && a.Status == "logged_out"
	})
	waitFor(t, "review item opened", func() bool {
		items, err := h.db.ReviewItemsOpen(context.Background(), "a1", 10)
		return err == nil && len(items) == 1 && items[0].Kind == "session_expired"
	})
	waitFor(t, "needs_human notified", func() bool { return h.notifier.count() >= 1 })

	// The task was parked back, not executed.
	time.Sleep(200 * time.Millisecond)
	if n := executed.Load(); n != 0 {
		t.Fatalf("handler ran %d times while logged out, want 0", n)
	}
	// And the task is preserved in the queue for after re-login.
	if h.deps.Queue.Len("a1") != 1 {
		t.Fatalf("queued tasks = %d, want 1 (task must be preserved)", h.deps.Queue.Len("a1"))
	}
}

func TestRateLimitBackoffRequeues(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, func(d *Deps) {
		d.Cfg.Accounts["a1"] = config.Account{DisplayName: "a1", UserAgent: "ua1", Concurrency: 1, DailyApplyCap: 200}
	})

	var calls atomic.Int32
	mustRegisterHandler(t, testKind, func(_ context.Context, _ *TaskContext, _ queue.Task) error {
		calls.Add(1)
		return &hhapi.StatusError{Kind: hhapi.ErrRateLimited, StatusCode: 429}
	})

	_ = h.deps.Queue.Enqueue(context.Background(), queue.Task{
		AccountID: "a1", Kind: testKind, Priority: queue.PrioApply, DedupKey: "r1",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, _ := h.run(ctx)
	defer stop()

	waitFor(t, "task hit 429 at least once", func() bool { return calls.Load() >= 1 })
	waitFor(t, "http_429 counter", func() bool {
		m, err := h.db.MetricsSnapshot(context.Background(), audit.Day())
		return err == nil && m[audit.KeyHTTP429] >= 1
	})
	// The task is re-enqueued for a later attempt (backoff), not dropped.
	// handleRateLimit increments the 429 metric before the requeue, so the
	// requeue may lag behind the metric observation: poll with a deadline.
	waitFor(t, "task requeued after 429", func() bool {
		return h.deps.Queue.Len("a1") == 1
	})
}

func TestPanicIsolation(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)

	var second atomic.Int32
	mustRegisterHandler(t, testKind, func(_ context.Context, _ *TaskContext, task queue.Task) error {
		if strings.HasSuffix(task.DedupKey, "1") {
			panic("boom: task handler exploded")
		}
		second.Add(1)
		return nil
	})

	for _, k := range []string{"panic-1", "ok-2"} {
		_ = h.deps.Queue.Enqueue(context.Background(), queue.Task{
			AccountID: "a1", Kind: testKind, Priority: queue.PrioApply, DedupKey: k,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, _ := h.run(ctx)
	defer stop()

	waitFor(t, "task after panic executed", func() bool { return second.Load() == 1 })
	waitFor(t, "tasks_failed counter", func() bool {
		m, err := h.db.MetricsSnapshot(context.Background(), audit.Day())
		return err == nil && m[audit.KeyTasksFailed] >= 1
	})
}

func TestGracefulShutdownNoGoroutineLeak(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)
	mustRegisterHandler(t, testKind, func(context.Context, *TaskContext, queue.Task) error { return nil })

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	ctx := context.Background()
	stop, done := h.run(ctx)

	h.clock.Advance(time.Hour) // let the scheduler fire once
	time.Sleep(100 * time.Millisecond)

	stop() // SIGINT-equivalent
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}

	// goleak-style manual check with a tolerance for runtime noise
	// (HTTP transport idle conns, test framework timers).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d", base, runtime.NumGoroutine())
}

func TestOutboxRedeliveryWorker(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)
	h.deps.Outbox = h.outbox

	// Two parked items, neither with a configured endpoint: both must be
	// retried with backoff (MarkFailed), never silently dropped or marked
	// delivered.
	h.outbox.items = []webhook.OutboxItem{
		{ID: 1, EndpointID: "https://hooks.example/one", Payload: []byte(`{"type":"apply_sent"}`)},
		{ID: 2, EndpointID: "https://unknown.example/x", Payload: []byte(`{"type":"needs_human"}`)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, _ := h.run(ctx)
	defer stop()

	waitFor(t, "items marked failed with backoff", func() bool {
		h.outbox.mu.Lock()
		defer h.outbox.mu.Unlock()
		return h.outbox.failed[1] != time.Time{} && h.outbox.failed[2] != time.Time{}
	})
	h.outbox.mu.Lock()
	_, delivered1 := containsID(h.outbox.delivered, 1)
	_, delivered2 := containsID(h.outbox.delivered, 2)
	h.outbox.mu.Unlock()
	if delivered1 || delivered2 {
		t.Fatal("undeliverable items must not be marked delivered")
	}

	// Backoff must be in the future (exponential, capped).
	h.outbox.mu.Lock()
	next := h.outbox.failed[1]
	h.outbox.mu.Unlock()
	if !next.After(h.clock.Now()) {
		t.Fatalf("MarkFailed next retry %v must be after now", next)
	}
}

func containsID(ids []int64, id int64) (int, bool) {
	for i, v := range ids {
		if v == id {
			return i, true
		}
	}
	return -1, false
}

func TestRegistryAPI(t *testing.T) {
	resetRegistrations()
	spec, err := scheduler.Parse("UTC", []string{"09:00"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterSchedule("reg-test", spec, func(context.Context, string) (queue.Task, error) {
		return queue.Task{}, nil
	}); err != nil {
		t.Fatalf("RegisterSchedule: %v", err)
	}
	bindings := registeredSchedules()
	if b, ok := bindings["reg-test"]; !ok || b.factory == nil {
		t.Fatalf("binding missing: %+v", bindings)
	}
	// Re-registration replaces (idempotent wiring in tests/CLI reloads).
	if err := RegisterSchedule("reg-test", spec, func(context.Context, string) (queue.Task, error) {
		return queue.Task{}, nil
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if len(registeredSchedules()) != 1 {
		t.Fatal("re-register must replace, not duplicate")
	}

	// Invalid spec rejected.
	if err := RegisterSchedule("bad", scheduler.Spec{Times: []string{"nope"}}, nil); err == nil {
		t.Fatal("invalid spec must be rejected")
	}
	// Nil factory rejected.
	if err := RegisterSchedule("nofactory", spec, nil); err == nil {
		t.Fatal("nil factory must be rejected")
	}
	// Nil handler rejected.
	if err := RegisterHandler(testKind, nil); err == nil {
		t.Fatal("nil handler must be rejected")
	}
}

func TestGracefulDrainProcessesQueuedTask(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)

	var phase atomic.Int32 // 1 = first task, 2 = second task
	h1Started := make(chan struct{})
	release1 := make(chan struct{})
	var handled2 atomic.Int32
	mustRegisterHandler(t, testKind, func(_ context.Context, _ *TaskContext, task queue.Task) error {
		switch task.DedupKey {
		case "one":
			if !phase.CompareAndSwap(0, 1) {
				t.Error("task one executed twice")
			}
			close(h1Started)
			<-release1
		case "two":
			phase.CompareAndSwap(1, 2)
			handled2.Add(1)
		}
		return nil
	})

	_ = h.deps.Queue.Enqueue(context.Background(), queue.Task{
		AccountID: "a1", Kind: testKind, Priority: queue.PrioApply, DedupKey: "one",
	})
	// Task two waits IN THE QUEUE while the signal arrives: the drain must
	// hand it to the (still alive) worker.
	_ = h.deps.Queue.Enqueue(context.Background(), queue.Task{
		AccountID: "a1", Kind: testKind, Priority: queue.PrioApply, DedupKey: "two",
	})

	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), h.deps) }()

	<-h1Started // Run's signal handler and workers are up by now

	// SIG-имитация: a real SIGINT drops Run's NotifyContext (sigCtx) while
	// the BASE context stays alive — that is the drain window under test.
	// A plain ctx cancel would hard-cancel the workers via parent
	// propagation and test nothing.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	close(release1) // let task one finish; the drain must serve task two

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after graceful drain")
	}
	if n := handled2.Load(); n != 1 {
		t.Fatalf("queued task processed %d times during drain, want 1", n)
	}
	if got := phase.Load(); got != 2 {
		t.Fatalf("phase = %d, want 2 (task two completed)", got)
	}
}

func TestRestartPreservesScheduleRow(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)
	mustRegisterHandler(t, testKind, func(context.Context, *TaskContext, queue.Task) error { return nil })

	// Simulate a previous run: a1 has an ENABLED row with persisted
	// last_run/next_run (not due); a2's row is missing.
	now := h.clock.Now()
	last := now.Add(-2 * time.Hour)
	next := now.Add(20 * time.Hour)
	doc, _ := json.Marshal(scheduleSpecJSON{Name: testSchedule, Spec: scheduler.Spec{
		TZ: "UTC", Times: []string{"08:00"}, Days: scheduler.All,
	}})
	if err := h.db.SchedulesUpsert(context.Background(), &store.Schedule{
		ID: testSchedule + "@a1", AccountID: "a1", Spec: string(doc), Enabled: true,
		LastRunAt: &last, NextRunAt: &next, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, _ := h.run(ctx)
	defer stop()

	waitFor(t, "a2 row materialized", func() bool {
		rows, err := h.db.SchedulesListAll(context.Background())
		return err == nil && len(rows) == 2
	})

	rows, err := h.db.SchedulesListAll(context.Background())
	if err != nil {
		t.Fatalf("SchedulesListAll: %v", err)
	}
	for _, r := range rows {
		if r.ID != testSchedule+"@a1" {
			continue
		}
		if r.LastRunAt == nil || !r.LastRunAt.Equal(last) {
			t.Errorf("restart changed last_run_at: got %v, want %v", r.LastRunAt, last)
		}
		if r.NextRunAt == nil || !r.NextRunAt.Equal(next) {
			t.Errorf("restart re-synthesized next_run_at: got %v, want %v (jitter must persist)", r.NextRunAt, next)
		}
	}
}

func TestRegisterScheduleForAccountSingleRow(t *testing.T) {
	resetRegistrations()
	h := newHarness(t, nil)

	soloKind := queue.TaskKind("t12_solo")
	spec, err := scheduler.Parse("UTC", []string{"08:00"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec.Jitter = 0
	if err := RegisterScheduleForAccount("solo", "a2", spec,
		func(_ context.Context, accountID string) (queue.Task, error) {
			return queue.Task{AccountID: accountID, Kind: soloKind, Priority: queue.PrioWarmup, DedupKey: "fire"}, nil
		}); err != nil {
		t.Fatalf("RegisterScheduleForAccount: %v", err)
	}

	var mu sync.Mutex
	var firedAccounts []string
	mustRegisterHandler(t, soloKind, func(_ context.Context, tc *TaskContext, _ queue.Task) error {
		mu.Lock()
		firedAccounts = append(firedAccounts, tc.AccountID)
		mu.Unlock()
		return nil
	})
	// Keep the default fan-out schedule quiet.
	mustRegisterHandler(t, testKind, func(context.Context, *TaskContext, queue.Task) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, _ := h.run(ctx)
	defer stop()

	waitFor(t, "schedule rows materialized", func() bool {
		rows, err := h.db.SchedulesListAll(context.Background())
		return err == nil && len(rows) == 3 // t12sched@a1, t12sched@a2, solo@a2
	})

	rows, err := h.db.SchedulesListAll(context.Background())
	if err != nil {
		t.Fatalf("SchedulesListAll: %v", err)
	}
	solo := 0
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "solo@") {
			solo++
			if r.AccountID != "a2" {
				t.Errorf("solo row bound to %q, want a2", r.AccountID)
			}
		}
	}
	if solo != 1 {
		t.Fatalf("single-account binding materialized %d rows, want exactly 1", solo)
	}

	waitFor(t, "scheduler parked", func() bool {
		return h.clock.hasPendingTimerAt(h.clock.Now().Add(SchedulerTick))
	})
	h.clock.Advance(26 * time.Hour)
	waitFor(t, "solo fired", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(firedAccounts) >= 1
	})
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(firedAccounts) != 1 {
		t.Fatalf("solo fired %d times (%v), want exactly 1", len(firedAccounts), firedAccounts)
	}
	if firedAccounts[0] != "a2" {
		t.Fatalf("factory received accountID %q, want a2", firedAccounts[0])
	}
}
