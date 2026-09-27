package tasklock

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func lockDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestAcquireDifferentTypesCoexist(t *testing.T) {
	dir := lockDir(t)
	h1, err := Acquire(dir, "/data/hh", TypeMultiApply)
	if err != nil {
		t.Fatalf("acquire multiapply: %v", err)
	}
	defer h1.Release()
	h2, err := Acquire(dir, "/data/hh", TypeResumeImprove)
	if err != nil {
		t.Fatalf("acquire resume_improve: %v", err)
	}
	defer h2.Release()
}

func TestAcquireSameTypeFailsFast(t *testing.T) {
	dir := lockDir(t)
	h1, err := Acquire(dir, "/data/hh", TypeMultiApply)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer h1.Release()
	_, err = Acquire(dir, "/data/hh", TypeMultiApply)
	if err == nil {
		t.Fatal("second acquire of the same type must fail fast")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if !filepath.IsAbs(lockedPath(err)) {
		t.Errorf("error must name the lock path, got %q", err)
	}
}

func TestAcquireAllConflictsWithEveryType(t *testing.T) {
	dir := lockDir(t)
	for _, typed := range []string{TypeMultiApply, TypeResumeImprove} {
		h, err := Acquire(dir, "/data/hh", typed)
		if err != nil {
			t.Fatalf("typed acquire %s: %v", typed, err)
		}
		if _, err := Acquire(dir, "/data/hh", TypeAll); !errors.Is(err, ErrLocked) {
			t.Errorf("all must conflict with held %s, got %v", typed, err)
		}
		h.Release()
	}
	// And the reverse: a held all blocks every typed acquisition.
	hAll, err := Acquire(dir, "/data/hh", TypeAll)
	if err != nil {
		t.Fatalf("acquire all: %v", err)
	}
	defer hAll.Release()
	for _, typed := range []string{TypeMultiApply, TypeResumeImprove} {
		if _, err := Acquire(dir, "/data/hh", typed); !errors.Is(err, ErrLocked) {
			t.Errorf("typed %s must conflict with held all, got %v", typed, err)
		}
	}
}

func TestAllAloneIsOneInstance(t *testing.T) {
	dir := lockDir(t)
	h, err := Acquire(dir, "/data/hh", TypeAll)
	if err != nil {
		t.Fatalf("acquire all: %v", err)
	}
	defer h.Release()
	if _, err := Acquire(dir, "/data/hh", TypeAll); !errors.Is(err, ErrLocked) {
		t.Fatalf("second all must fail fast, got %v", err)
	}
}

func TestReleaseUnlocks(t *testing.T) {
	dir := lockDir(t)
	h, err := Acquire(dir, "/data/hh", TypeResumeImprove)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.Release()
	h2, err := Acquire(dir, "/data/hh", TypeResumeImprove)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	h2.Release()
}

func TestReleaseIsIdempotent(t *testing.T) {
	dir := lockDir(t)
	h, err := Acquire(dir, "/data/hh", TypeMultiApply)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	h.Release()
	h.Release() // must not panic or error
}

func TestDataDirIsolation(t *testing.T) {
	dir := lockDir(t)
	h1, err := Acquire(dir, "/data/hh-a", TypeMultiApply)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer h1.Release()
	h2, err := Acquire(dir, "/data/hh-b", TypeMultiApply)
	if err != nil {
		t.Fatalf("different data_dir must not conflict: %v", err)
	}
	defer h2.Release()
}

func TestLockPathShape(t *testing.T) {
	got := LockPath(t.TempDir(), "/data/hh", TypeMultiApply)
	base := filepath.Base(got)
	const wantPrefix = "hhauto-"
	if filepath.Dir(got) == "" || len(base) <= len(wantPrefix)+len(TypeMultiApply)+len(".lock") {
		t.Fatalf("unexpected path %q", got)
	}
	if filepath.Ext(base) != ".lock" || base[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("base = %q, want hhauto-<hash>-<type>.lock", base)
	}
}

func TestValidateVocabulary(t *testing.T) {
	for _, ok := range []string{TypeAll, TypeMultiApply, TypeResumeImprove} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	if err := Validate("everything"); err == nil {
		t.Error("Validate(unknown) must fail")
	}
}

func TestAcquireInvalidType(t *testing.T) {
	if _, err := Acquire(t.TempDir(), "/d", "everything"); err == nil {
		t.Fatal("invalid type must fail")
	}
}

func TestFlockSurvivesFileCloseByOtherProcessSimulation(t *testing.T) {
	// Two handles on the same path within one process must conflict: the
	// lock rides the open file description, not the inode.
	dir := lockDir(t)
	path := LockPath(dir, "/d", TypeMultiApply)
	f1, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f1.Close() }()
	if err := syscall.Flock(int(f1.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock 1: %v", err)
	}
	f2, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = f2.Close() }()
	if err := syscall.Flock(int(f2.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f2.Fd()), syscall.LOCK_UN)
		t.Fatal("second flock on the same path must fail while the first is held")
	}
	_ = syscall.Flock(int(f1.Fd()), syscall.LOCK_UN)
}

func lockedPath(err error) string {
	var le *LockedError
	if errors.As(err, &le) {
		return le.Path
	}
	return ""
}
