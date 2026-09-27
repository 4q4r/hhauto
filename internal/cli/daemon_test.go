package cli

// T35 daemon loop tests: the автоотклик cycle (apply pass → sleep exactly
// the interval from the last cycle → repeat) with an injected fake cycle,
// a scripted kill switch and a real state file in a temp dir.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"hhauto/internal/engine"
	"hhauto/internal/hhapi"
)

func daemonTestDeps(t *testing.T) daemonDeps {
	t.Helper()
	return daemonDeps{
		DataDir:   t.TempDir(),
		TaskType:  "multiapply",
		Interval:  30 * time.Millisecond,
		Poll:      time.Millisecond,
		Kill:      engine.NewKillSwitch(""),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       time.Now,
		StatePath: filepath.Join(t.TempDir(), "daemon-multiapply.state.json"),
	}
}

// The basic loop: N cycles run, then the kill switch trips and the daemon
// exits cleanly (nil).
func TestRunDaemonCyclesUntilKillSwitch(t *testing.T) {
	d := daemonTestDeps(t)
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 3 {
			d.Kill.Stop() // kill-switch after the third cycle
		}
		return nil
	}
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	if got := cycles.Load(); got != 3 {
		t.Fatalf("cycles = %d, want 3", got)
	}
	st := loadDaemonState(d.StatePath)
	if st.Cycles != 3 || st.LastCycleAt.IsZero() {
		t.Fatalf("state not persisted: %+v", st)
	}
}

// A failing cycle is logged and the daemon continues (tomorrow's cycle is
// the retry), but the state still advances.
func TestRunDaemonFailedCycleContinues(t *testing.T) {
	d := daemonTestDeps(t)
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 2 {
			d.Kill.Stop()
		}
		return errors.New("vacancy fetch failed")
	}
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("failed cycle must not stop the daemon: %v", err)
	}
	if got := cycles.Load(); got != 2 {
		t.Fatalf("cycles = %d, want 2", got)
	}
	if st := loadDaemonState(d.StatePath); st.Cycles != 2 {
		t.Fatalf("failed cycles must still persist state: %+v", st)
	}
}

// Session loss (403 on bootstrap): review + graceful exit with code 2,
// distinguishable from a clean stop.
func TestRunDaemonSessionLostExitsWithCode2(t *testing.T) {
	d := daemonTestDeps(t)
	d.Bootstrap = func(context.Context) error {
		d.Kill.Stop() // no further cycles either way
		return fmt.Errorf("bootstrap: %w", hhapi.ErrLoggedOut)
	}
	d.Cycle = func(context.Context) error { t.Fatal("cycle must not run after session loss"); return nil }
	err := runDaemon(context.Background(), d)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("err = %v, want ExitError{2}", err)
	}
	if !errors.Is(err, hhapi.ErrLoggedOut) {
		t.Fatalf("exit error must wrap the cause: %v", err)
	}
}

// Non-session bootstrap errors do not kill the daemon: the cycle's typed
// errors are the authority.
func TestRunDaemonTransientBootstrapErrorContinues(t *testing.T) {
	d := daemonTestDeps(t)
	var boots atomic.Int64
	d.Bootstrap = func(context.Context) error {
		if boots.Add(1) <= 1 {
			return errors.New("network unreachable")
		}
		return nil
	}
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 2 {
			d.Kill.Stop()
		}
		return nil
	}
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("transient bootstrap error must not stop the daemon: %v", err)
	}
}

// Restart persistence: with a fresh state file the first cycle runs
// immediately; after a restart with a RECENT last_cycle_at the daemon
// sleeps the remainder instead of starting a new cycle.
func TestRunDaemonResumesFromPersistedState(t *testing.T) {
	d := daemonTestDeps(t)
	// Simulate a cycle completed 5ms ago (interval 30ms): the remainder
	// must be slept, not re-run immediately.
	if err := saveDaemonState(d.StatePath, daemonState{LastCycleAt: time.Now().Add(-5 * time.Millisecond), Cycles: 7}); err != nil {
		t.Fatal(err)
	}
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		cycles.Add(1)
		d.Kill.Stop()
		return nil
	}
	start := time.Now()
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatalf("daemon did not sleep the persisted remainder (%v)", time.Since(start))
	}
	if got := cycles.Load(); got != 1 {
		t.Fatalf("cycles after restart = %d, want 1 (the post-sleep cycle)", got)
	}
	if st := loadDaemonState(d.StatePath); st.Cycles != 8 {
		t.Fatalf("cycles counter must continue: %+v", st)
	}
}

// A fresh start (no state file) runs the first cycle immediately.
func TestRunDaemonFreshStartRunsImmediately(t *testing.T) {
	d := daemonTestDeps(t)
	d.Now = func() time.Time { return time.Now() }
	ran := make(chan struct{})
	d.Cycle = func(context.Context) error {
		select {
		case <-ran:
		default:
			close(ran)
		}
		d.Kill.Stop()
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- runDaemon(context.Background(), d) }()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("first cycle did not run immediately on a fresh state")
	}
	if err := <-done; err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
}

// The sleep is interruptible: a stop requested mid-sleep ends the daemon
// promptly.
func TestRunDaemonKillDuringSleep(t *testing.T) {
	d := daemonTestDeps(t)
	d.Interval = 10 * time.Second // long sleep
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		cycles.Add(1)
		go func() {
			time.Sleep(20 * time.Millisecond)
			d.Kill.Stop()
		}()
		return nil
	}
	start := time.Now()
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("kill switch did not interrupt the sleep")
	}
	if got := cycles.Load(); got != 1 {
		t.Fatalf("cycles = %d, want 1", got)
	}
}

// Parent context cancellation also ends the daemon cleanly (Ctrl+C path).
func TestRunDaemonContextCancelExitsCleanly(t *testing.T) {
	d := daemonTestDeps(t)
	d.Interval = 10 * time.Second
	d.Cycle = func(context.Context) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := runDaemon(ctx, d); err != nil {
		t.Fatalf("cancelled daemon must exit nil, got %v", err)
	}
}
