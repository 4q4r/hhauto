package cli

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"hhauto/internal/engine"
)

// T71 wake-up: the daemon sleeps to min(apply boundary, nearest queued
// deadline). A maintenance deadline BEFORE the apply boundary wakes the
// daemon for a maintenance pass; the 24h apply cadence itself is untouched.

func t71DaemonDeps(t *testing.T) daemonDeps {
	t.Helper()
	return daemonDeps{
		DataDir:   t.TempDir(),
		TaskType:  "multiapply",
		Interval:  500 * time.Millisecond,
		Poll:      time.Millisecond,
		Kill:      engine.NewKillSwitch(""),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       time.Now,
		StatePath: filepath.Join(t.TempDir(), "daemon-multiapply.state.json"),
	}
}

// A maintenance deadline earlier than the apply boundary wakes the daemon
// for maintenance passes while the apply cycle does not run again: T76
// runs the startup pass immediately, then the wake preempts the remainder
// sleep — the 500ms boundary itself never arrives.
func TestRunDaemonWakesForMaintenanceBeforeCycle(t *testing.T) {
	d := t71DaemonDeps(t)
	start := time.Now()
	if err := saveDaemonState(d.StatePath, daemonState{LastCycleAt: start, Cycles: 1}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	var maintenance, arms, cycles atomic.Int64
	d.Wake = func(context.Context) (time.Time, bool) {
		return start.Add(40 * time.Millisecond), true
	}
	d.Maintenance = func(context.Context) error {
		if maintenance.Add(1) >= 2 {
			d.Kill.Stop()
		}
		return nil
	}
	d.Arm = func(context.Context) error {
		arms.Add(1)
		return nil
	}
	d.Cycle = func(context.Context) error {
		cycles.Add(1)
		return nil
	}

	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	if got := maintenance.Load(); got != 2 {
		t.Fatalf("maintenance passes = %d, want 2", got)
	}
	if got := cycles.Load(); got != 1 {
		t.Fatalf("apply cycles = %d, want 1 (the T76 startup pass)", got)
	}
	if got := arms.Load(); got < 2 {
		t.Fatalf("arm calls = %d, want >= 2 (the chain re-arms every iteration)", got)
	}
}

// A wake deadline AFTER the apply boundary must not preempt the cycle: the
// daemon sleeps out the interval and runs the apply pass first.
func TestRunDaemonCycleStillRunsWhenWakeIsLater(t *testing.T) {
	d := t71DaemonDeps(t)
	start := time.Now()
	var cycles atomic.Int64
	d.Wake = func(context.Context) (time.Time, bool) {
		return start.Add(10 * time.Second), true // beyond the 500ms interval
	}
	d.Maintenance = func(context.Context) error {
		t.Error("maintenance must not run when the wake follows the apply boundary")
		return nil
	}
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 1 {
			d.Kill.Stop()
		}
		return nil
	}

	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	if got := cycles.Load(); got != 1 {
		t.Fatalf("cycles = %d, want 1", got)
	}
}

// nil Wake keeps the legacy flat sleep (no wake-ups, no maintenance).
func TestRunDaemonNilWakeLegacySleep(t *testing.T) {
	d := t71DaemonDeps(t)
	var cycles atomic.Int64
	d.Maintenance = func(context.Context) error {
		t.Error("maintenance must not run without a Wake")
		return nil
	}
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 2 {
			d.Kill.Stop()
		}
		return nil
	}

	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	if got := cycles.Load(); got != 2 {
		t.Fatalf("cycles = %d, want 2", got)
	}
}
