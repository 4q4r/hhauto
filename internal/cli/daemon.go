package cli

// T35: the автоотклик daemon mode (`hhauto run --daemon`). The loop is
// deliberately NOT the scheduler: one bootstrap check → one apply pass →
// persist state → sleep EXACTLY the interval from the cycle completion →
// repeat, until the kill switch (STOP file / signal / Stop()) or a lost
// session. T76: the first pass runs immediately at startup — the
// persisted cadence never gates it, the state file only carries the
// counters and the stamp re-anchors from each actual pass (the
// applied-dedup protects against repeats).
//
// Exit contract: nil = clean stop (exit 0); *ExitError{2} = the session
// died (403 on bootstrap): the account is marked logged_out, a review item
// is parked and the daemon exits so the owner can rotate the cookies and
// restart.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"hhauto/internal/console"
	"hhauto/internal/engine"
	"hhauto/internal/hhapi"
)

// daemonInterval is the documented cycle cadence: exactly 24h from the
// last apply pass.
const daemonInterval = 24 * time.Hour

// daemonKillPoll bounds how long a STOP file / signal can go unnoticed
// during the sleep.
const daemonKillPoll = time.Second

// daemonState is the persisted cycle bookkeeping
// (<data_dir>/daemon-<task_type>.state.json).
type daemonState struct {
	LastCycleAt time.Time `json:"last_cycle_at"`
	Cycles      int       `json:"cycles"`
}

func daemonStatePath(dataDir, taskType string) string {
	return filepath.Join(dataDir, "daemon-"+taskType+".state.json")
}

func loadDaemonState(path string) daemonState {
	b, err := os.ReadFile(path)
	if err != nil {
		return daemonState{} // fresh start
	}
	var st daemonState
	if err := json.Unmarshal(b, &st); err != nil {
		return daemonState{} // a corrupt file must not brick the daemon
	}
	return st
}

func saveDaemonState(path string, st daemonState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o640)
}

// daemonDeps wires the loop. Cycle and Bootstrap are seams: the command
// binds them to the runtime; tests inject fakes.
type daemonDeps struct {
	DataDir  string
	TaskType string
	// Interval is the sleep between cycles (production: 24h exactly from
	// the last cycle completion).
	Interval time.Duration
	// Poll bounds kill-switch latency during the sleep (tests shrink it).
	Poll time.Duration
	// Cycle is one apply pass. A returned error is logged and the loop
	// continues — tomorrow's cycle is the retry.
	Cycle func(ctx context.Context) error
	// Bootstrap is the pre-cycle session check (nil = skip). An
	// hhapi.ErrLoggedOut failure exits with code 2; other errors are
	// logged and the cycle proceeds (its typed errors are the authority).
	Bootstrap func(ctx context.Context) error
	// T71 wake-up: Wake reports the nearest queued maintenance deadline
	// (ok=false when none). A deadline preceding the apply boundary wakes
	// the daemon for a Maintenance pass; the apply cadence itself is
	// untouched. nil Wake = the legacy flat interval sleep.
	Wake func(ctx context.Context) (time.Time, bool)
	// Maintenance is the work done at a wake-up (the due-task drain). A
	// returned error is logged and the loop continues.
	Maintenance func(ctx context.Context) error
	// Arm re-seeds the maintenance task chain; called every iteration,
	// dedup-idempotent. Errors are logged, never fatal. nil = skip.
	Arm  func(ctx context.Context) error
	Kill engine.KillSwitch
	Log  *slog.Logger
	// Console renders the lifecycle lines (T76: the russian start banner,
	// the pass-completion line, the pre-sleep line). nil = silent (unit
	// tests without a surface).
	Console *console.Surface
	// StatePath overrides the default daemonStatePath (tests).
	StatePath string
	Now       func() time.Time
}

// runDaemon drives the loop until the kill switch trips, the context is
// cancelled, or the session is lost. See the file head for the contract.
func runDaemon(ctx context.Context, d daemonDeps) error {
	if d.Interval <= 0 {
		d.Interval = daemonInterval
	}
	if d.Poll <= 0 {
		d.Poll = daemonKillPoll
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Kill == nil {
		d.Kill = engine.NewKillSwitch("")
	}
	statePath := d.StatePath
	if statePath == "" {
		statePath = daemonStatePath(d.DataDir, d.TaskType)
	}
	state := loadDaemonState(statePath)
	d.Console.DaemonStart()

	// T76: the first pass runs immediately — the persisted cadence never
	// gates the startup pass; the cycle stamp re-anchors from it (the
	// applied-dedup protects against repeats).
	first := true
	for {
		if d.Kill.Tripped(ctx) {
			d.Log.Info("daemon: запрошена остановка, выхожу", "cycles", state.Cycles)
			return nil
		}
		// T71: keep the maintenance chain armed (dedup-idempotent — a no-op
		// while the window task is still queued, a fresh ~4h deadline after
		// the drain consumed it).
		if d.Arm != nil {
			if err := d.Arm(ctx); err != nil {
				d.Log.Warn("daemon: maintenance arm failed", "err", err)
			}
		}
		next := state.LastCycleAt.Add(d.Interval)
		if !first && d.Now().Before(next) {
			// T71: between passes the daemon sleeps out the remainder,
			// waking early for a nearer maintenance deadline.
			if !d.sleepRemainder(ctx, next) {
				d.Log.Info("daemon: запрошена остановка во время сна", "cycles", state.Cycles)
				return nil
			}
			continue
		}
		first = false
		if err := d.applyPass(ctx, &state, statePath); err != nil {
			return err
		}
	}
}

// applyPass runs one bootstrap-gated apply cycle and persists the cycle
// stamp; a failed cycle is logged (tomorrow's pass is the retry). A lost
// session returns the exit error for cookie rotation.
func (d daemonDeps) applyPass(ctx context.Context, state *daemonState, statePath string) error {
	if d.Bootstrap != nil {
		if err := d.Bootstrap(ctx); err != nil {
			if errors.Is(err, hhapi.ErrLoggedOut) {
				d.Log.Error("daemon: сессия слетела; выхожу для ротации кук", "err", err)
				return &ExitError{Code: 2, Err: fmt.Errorf("daemon: сессия слетела (403): %w", err)}
			}
			d.Log.Warn("daemon: bootstrap не удался; продолжаю цикл", "err", err)
		}
	}
	if err := d.Cycle(ctx); err != nil {
		d.Log.Warn("daemon: цикл завершился с ошибками; повтор на следующем проходе", "err", err)
	}
	state.LastCycleAt = d.Now()
	state.Cycles++
	if err := saveDaemonState(statePath, *state); err != nil {
		d.Log.Error("daemon: persist state", "path", statePath, "err", err)
	}
	d.Console.CycleDone()
	return nil
}

// sleepRemainder covers the in-interval wait: the daemon sleeps to the
// apply boundary, waking early for a nearer maintenance deadline (T71 —
// past deadlines included, the wake is immediate) to run the maintenance
// pass. Reports whether the loop continues (false = stop requested).
func (d daemonDeps) sleepRemainder(ctx context.Context, next time.Time) bool {
	wake := next
	maintenance := false
	if d.Wake != nil {
		if w, ok := d.Wake(ctx); ok && w.Before(next) {
			wake = w
			maintenance = true
		}
	}
	d.Console.Sleeping(wake, maintenance)
	if !daemonSleep(ctx, d, wake) {
		return false
	}
	if wake.Before(next) && d.Maintenance != nil {
		if err := d.Maintenance(ctx); err != nil {
			d.Log.Warn("daemon: maintenance pass failed; the next wake retries", "err", err)
		}
	}
	return true
}

// daemonSleep waits until the deadline, waking early on stop. It reports
// whether the deadline was reached (false = stop requested).
func daemonSleep(ctx context.Context, d daemonDeps, until time.Time) bool {
	for {
		if d.Kill.Tripped(ctx) || ctx.Err() != nil {
			return false
		}
		now := d.Now()
		if !now.Before(until) {
			return true
		}
		wait := until.Sub(now)
		if wait > d.Poll {
			wait = d.Poll
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}
