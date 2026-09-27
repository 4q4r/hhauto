// Package tasklock is the T35 multi-instance mutual exclusion: one flock
// file per (data_dir, task type) under the OS temp dir. Two hhauto processes
// with DIFFERENT task types run in parallel against the same data dir; two
// processes of the SAME type fail fast, and the legacy untyped run ("all")
// stays mutually exclusive with everything, exactly as before T35.
//
// The lock rides flock (per open file description): it is released on Close
// AND automatically when the process dies — no stale-lock cleanup ever.
package tasklock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Task-type vocabulary for `hhauto run --task-type` (and the matching lock
// files). all = the legacy single-instance behavior: the process claims
// every type's lock and conflicts with any other instance.
const (
	TypeAll           = "all"
	TypeMultiApply    = "multiapply"
	TypeResumeImprove = "resume_improve"
)

// Types is the closed vocabulary, sorted for stable error messages.
var Types = []string{TypeAll, TypeMultiApply, TypeResumeImprove}

// ErrLocked wraps every "another instance holds this lock" failure; the
// *LockedError detail names the path and the owning task type.
var ErrLocked = errors.New("tasklock: another hhauto instance holds this task type")

// LockedError reports which lock file blocked the acquisition.
type LockedError struct {
	Path     string
	TaskType string
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("tasklock: %s already running (lock %s)", e.TaskType, e.Path)
}

func (e *LockedError) Unwrap() error { return ErrLocked }

// Validate reports whether taskType is in the vocabulary.
func Validate(taskType string) error {
	for _, t := range Types {
		if t == taskType {
			return nil
		}
	}
	return fmt.Errorf("tasklock: unknown task type %q (want one of: %v)", taskType, Types)
}

// LockPath is the lock file for one (dataDir, taskType) pair. The data dir
// is hashed into 16 hex chars: paths are long and may contain characters
// unfit for file names.
func LockPath(lockDir, dataDir, taskType string) string {
	sum := sha256.Sum256([]byte(dataDir))
	return filepath.Join(lockDir, fmt.Sprintf("hhauto-%s-%s.lock", hex.EncodeToString(sum[:8]), taskType))
}

// Handle owns the acquired lock file descriptors. The locks live until
// Release (or process death); Release is idempotent.
type Handle struct {
	files []*os.File
}

// Release drops every held lock. Idempotent; safe on a zero Handle.
func (h *Handle) Release() {
	for _, f := range h.files {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	h.files = nil
}

// Acquire exclusively locks the task type's lock file (non-blocking: a held
// lock returns *LockedError immediately — the fail-fast contract). The
// legacy "all" type locks EVERY type file, so it conflicts with any other
// instance and nothing can start under it. lockDir is normally os.TempDir();
// tests pass their own directory.
func Acquire(lockDir, dataDir, taskType string) (*Handle, error) {
	if err := Validate(taskType); err != nil {
		return nil, err
	}
	claimed := []string{taskType}
	if taskType == TypeAll {
		claimed = append([]string{}, Types...)
	}
	h := &Handle{}
	for _, t := range claimed {
		if err := h.lockOne(lockDir, dataDir, t); err != nil {
			h.Release()
			return nil, err
		}
	}
	return h, nil
}

// lockOne opens (creating if missing) one lock file and takes the exclusive
// non-blocking flock. The fd stays open on the handle: closing it would
// drop the lock.
func (h *Handle) lockOne(lockDir, dataDir, taskType string) error {
	path := LockPath(lockDir, dataDir, taskType)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return fmt.Errorf("tasklock: create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("tasklock: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return &LockedError{Path: path, TaskType: taskType}
	}
	h.files = append(h.files, f)
	return nil
}
