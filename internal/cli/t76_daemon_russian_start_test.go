package cli

// T76: the daemon speaks Russian and starts working immediately. The
// startup banner replaces the English slog line (one banner per event),
// the first apply pass runs at startup regardless of the persisted
// cadence (the stamp re-anchors from the actual pass), and every sleep
// prints one dim line naming the wake moment and the nearest event.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/console"
	"hhauto/internal/engine"
)

func t76DaemonDeps(t *testing.T) (daemonDeps, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	surf, err := console.New(buf, false, config.Console{}, nil)
	if err != nil {
		t.Fatalf("console: %v", err)
	}
	d := daemonDeps{
		DataDir:   t.TempDir(),
		TaskType:  "multiapply",
		Interval:  30 * time.Millisecond,
		Poll:      time.Millisecond,
		Kill:      engine.NewKillSwitch(""),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       time.Now,
		StatePath: filepath.Join(t.TempDir(), "daemon-multiapply.state.json"),
		Console:   surf,
	}
	return d, buf
}

// The startup banner is Russian (the T53 mandate) and the English slog
// banner is gone — one banner per event.
func TestDaemonBannerIsRussian(t *testing.T) {
	d, buf := t76DaemonDeps(t)
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 1 {
			d.Kill.Stop()
		}
		return nil
	}
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "✓ Демон запущен: отклики раз в 24ч · первый проход сейчас · подъёмы по окнам") {
		t.Fatalf("no russian banner in output: %q", out)
	}
	if strings.Contains(out, "started task_type") {
		t.Fatalf("english banner still present: %q", out)
	}
}

// The first apply pass runs at startup even when the persisted cadence
// (last_cycle_at < 24h ago) has not elapsed: the stamp re-anchors from
// the actual pass and the counter continues.
func TestDaemonFirstPassAtStartupDespiteFreshState(t *testing.T) {
	d, _ := t76DaemonDeps(t)
	d.Interval = 24 * time.Hour // the persisted cadence must not gate the start
	if err := saveDaemonState(d.StatePath, daemonState{LastCycleAt: time.Now().Add(-time.Hour), Cycles: 1}); err != nil {
		t.Fatalf("seed state: %v", err)
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
	if time.Since(start) > 5*time.Second {
		t.Fatalf("startup pass waited for the cadence boundary (%v)", time.Since(start))
	}
	if got := cycles.Load(); got != 1 {
		t.Fatalf("startup pass = %d, want 1", got)
	}
	if st := loadDaemonState(d.StatePath); st.Cycles != 2 {
		t.Fatalf("cycles counter must continue: %+v", st)
	}
}

// Before every sleep exactly one dim line names the wake moment and the
// nearest event: the apply cadence («отклики») by default…
func TestDaemonSleepLineNamesApplyBoundary(t *testing.T) {
	d, buf := t76DaemonDeps(t)
	d.Interval = 10 * time.Second // a long remainder after the startup pass
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 1 {
			go func() {
				time.Sleep(20 * time.Millisecond)
				d.Kill.Stop()
			}()
		}
		return nil
	}
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "· Сплю до ") || !strings.Contains(out, "(ближайшее: отклики)") {
		t.Fatalf("no apply-boundary sleep line: %q", out)
	}
}

// …or the nearer maintenance wake («подъём») when one precedes the
// boundary.
func TestDaemonSleepLineNamesMaintenanceWake(t *testing.T) {
	d, buf := t76DaemonDeps(t)
	d.Interval = 500 * time.Millisecond
	base := time.Now()
	d.Wake = func(context.Context) (time.Time, bool) {
		return base.Add(10 * time.Millisecond), true
	}
	d.Maintenance = func(context.Context) error {
		d.Kill.Stop()
		return nil
	}
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		cycles.Add(1)
		return nil
	}
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "(ближайшее: подъём)") {
		t.Fatalf("no maintenance sleep line: %q", out)
	}
}

// The pass-completion lifecycle line follows every pass and sits between
// the banner and the sleep line (the T69 digest lines during the pass
// carry the counters; the daemon line never repeats them).
func TestDaemonPassCompletionLineAfterCycle(t *testing.T) {
	d, buf := t76DaemonDeps(t)
	var cycles atomic.Int64
	d.Cycle = func(context.Context) error {
		if cycles.Add(1) >= 1 {
			go func() {
				time.Sleep(20 * time.Millisecond)
				d.Kill.Stop()
			}()
		}
		return nil
	}
	if err := runDaemon(context.Background(), d); err != nil {
		t.Fatalf("runDaemon: %v", err)
	}
	out := buf.String()
	iBanner := strings.Index(out, "Демон запущен")
	iDone := strings.Index(out, "Проход завершён")
	iSleep := strings.Index(out, "Сплю до")
	if iBanner < 0 || iDone < 0 || iSleep < 0 {
		t.Fatalf("lifecycle lines missing: %q", out)
	}
	if iBanner >= iDone || iDone >= iSleep {
		t.Fatalf("lifecycle line order wrong: banner=%d done=%d sleep=%d", iBanner, iDone, iSleep)
	}
}
