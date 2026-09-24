package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// KillSwitch is the three-channel emergency stop:
//
//  1. signal  — Run integrates os.Interrupt/SIGTERM via signal.NotifyContext;
//     a cancelled context trips Tripped (checked before every task).
//  2. file    — the operator touches <data_dir>/STOP; Tripped polls the
//     filesystem with a 1s cache so checks stay cheap in hot loops.
//  3. program — API Stop() (reserved for `hhauto stop` in-process control).
//
// Any single channel is sufficient: a tripped switch means "no further
// mutations", and Run shuts down gracefully.
type KillSwitch interface {
	// Tripped reports whether the switch is engaged. A cancelled ctx is
	// itself a trip condition (channel 1).
	Tripped(ctx context.Context) bool
	// Stop engages the switch programmatically (channel 3). Idempotent.
	Stop()
}

// StopFilePath returns the kill-switch flag path for a data directory.
func StopFilePath(dataDir string) string {
	return filepath.Join(dataDir, "STOP")
}

// stopNotifier is an optional extension implemented by engine-built switch
// instances so Run can react to Stop()/file-flag instantly instead of
// waiting for the next poll.
type stopNotifier interface {
	StopCh() <-chan struct{}
}

const killSwitchCacheTTL = time.Second

// fsKillSwitch implements the file-flag + programmatic channels. The fs
// check is lazy per Tripped call with a killSwitchCacheTTL result cache:
// hot loops pay at most one stat per second.
type fsKillSwitch struct {
	path string

	stop atomic.Bool // channel 3

	mu          sync.Mutex
	lastTripped bool
	checkedAt   time.Time
	now         func() time.Time // injectable clock (package tests override)

	stopOnce sync.Once
	stopCh   chan struct{} // closed on the first trip of any channel
}

var _ KillSwitch = (*fsKillSwitch)(nil)

// NewKillSwitch builds a kill switch watching path (normally
// StopFilePath(cfg.General.DataDir)). A nil-able path disables the fs
// channel: only Stop() and ctx cancellation trip the switch.
//
//nolint:revive // sealed impl: callers hold the KillSwitch interface; StopCh is an engine-internal extension
func NewKillSwitch(path string) *fsKillSwitch {
	return &fsKillSwitch{
		path:   path,
		now:    time.Now,
		stopCh: make(chan struct{}),
	}
}

// StopCh returns a channel closed when the switch trips (file found or
// Stop called). Context cancellation is NOT reflected here — Run watches
// ctx separately.
func (k *fsKillSwitch) StopCh() <-chan struct{} { return k.stopCh }

func (k *fsKillSwitch) tripOnce() {
	k.stopOnce.Do(func() { close(k.stopCh) })
}

// Stop implements KillSwitch (channel 3).
func (k *fsKillSwitch) Stop() {
	k.stop.Store(true)
	k.tripOnce()
}

// Tripped implements KillSwitch.
func (k *fsKillSwitch) Tripped(ctx context.Context) bool {
	// The caller's context is already cancelled (per-task ctx during
	// shutdown, test teardown): report tripped for THIS check so nothing
	// mutates past a dead context, but do NOT latch the switch — a
	// cancelled ctx is not a kill-switch trip.
	if err := ctx.Err(); err != nil {
		return true
	}
	// Channel 3: programmatic stop.
	if k.stop.Load() {
		return true
	}
	// Channel 2: file flag (cached).
	if k.fileTripped() {
		k.tripOnce()
		return true
	}
	return false
}

func (k *fsKillSwitch) fileTripped() bool {
	if k.path == "" {
		return false
	}
	k.mu.Lock()
	now := k.now()
	if k.checkedAtValid(now) {
		tripped := k.lastTripped
		k.mu.Unlock()
		return tripped
	}
	k.mu.Unlock()

	// Stat outside the lock: concurrent checkers may duplicate work, never
	// block each other.
	_, err := statFile(k.path)

	k.mu.Lock()
	defer k.mu.Unlock()
	k.checkedAt = now
	k.lastTripped = err == nil
	return k.lastTripped
}

func (k *fsKillSwitch) checkedAtValid(now time.Time) bool {
	return !k.checkedAt.IsZero() && now.Sub(k.checkedAt) < killSwitchCacheTTL
}

// statFile is a seam over os.Stat (kept tiny; the fs layout is fixed).
func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }
