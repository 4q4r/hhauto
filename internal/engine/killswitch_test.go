package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStopFilePath(t *testing.T) {
	if got := StopFilePath("/var/lib/hhauto"); got != filepath.Join("/var/lib/hhauto", "STOP") {
		t.Fatalf("StopFilePath = %q", got)
	}
}

func TestKillSwitchFileFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "STOP")
	ks := NewKillSwitch(path)

	cur := time.Now()
	ks.now = func() time.Time { return cur }
	ctx := context.Background()

	if ks.Tripped(ctx) {
		t.Fatal("fresh switch must not be tripped")
	}
	// Write the flag: cached state (fresh for 1s) still says false.
	if err := os.WriteFile(path, []byte("stop"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ks.Tripped(ctx) {
		t.Fatal("cached check must not see the flag before the cache expires")
	}
	// Cache expired: the flag is seen.
	cur = cur.Add(1100 * time.Millisecond)
	if !ks.Tripped(ctx) {
		t.Fatal("flag file must trip the switch after cache expiry")
	}
	// And it latches via cache too.
	if !ks.Tripped(ctx) {
		t.Fatal("tripped state must be sticky while cached")
	}
}

func TestKillSwitchRemovedFlagClears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "STOP")
	if err := os.WriteFile(path, []byte("stop"), 0o644); err != nil {
		t.Fatal(err)
	}
	ks := NewKillSwitch(path)
	cur := time.Now()
	ks.now = func() time.Time { return cur }
	if !ks.Tripped(context.Background()) {
		t.Fatal("flag must trip the switch")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cur = cur.Add(1100 * time.Millisecond)
	if ks.Tripped(context.Background()) {
		t.Fatal("removing the flag must clear the trip (fs state is the source)")
	}
}

func TestKillSwitchProgrammaticStop(t *testing.T) {
	ks := NewKillSwitch(filepath.Join(t.TempDir(), "STOP"))
	ks.now = func() time.Time { return time.Now() }
	if ks.Tripped(context.Background()) {
		t.Fatal("must not be tripped before Stop")
	}
	ks.Stop()
	if !ks.Tripped(context.Background()) {
		t.Fatal("Stop() must trip the switch immediately (no fs round-trip)")
	}
	// StopCh fires exactly once and does not block repeated Stops.
	select {
	case <-ks.StopCh():
	default:
		t.Fatal("StopCh must be closed by Stop()")
	}
	ks.Stop() // idempotent: no panic, channel already closed
}

func TestKillSwitchCancelledContext(t *testing.T) {
	ks := NewKillSwitch(filepath.Join(t.TempDir(), "STOP"))
	ks.now = func() time.Time { return time.Now() }
	ctx, cancel := context.WithCancel(context.Background())
	if ks.Tripped(ctx) {
		t.Fatal("live ctx must not trip")
	}
	cancel()
	if !ks.Tripped(ctx) {
		t.Fatal("cancelled ctx (signal channel via NotifyContext) must trip")
	}
}

// compile-time check: *fsKillSwitch satisfies KillSwitch.
var _ KillSwitch = (*fsKillSwitch)(nil)
