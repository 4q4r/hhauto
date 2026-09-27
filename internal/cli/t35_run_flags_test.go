package cli

// T35 run-flag E2E: --task-type vocabulary and lock conflicts, the daemon
// lifecycle through the real cobra tree with the fake hh.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/engine"
	"hhauto/internal/store"
	"hhauto/internal/tasklock"
)

func TestRunTaskTypeResumeImproveRejected(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	_, err := runRoot(t, "run", "--once", "--task-type", "resume_improve", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "resume improve") {
		t.Fatalf("resume_improve must be rejected with guidance: %v", err)
	}
}

func TestRunTaskTypeUnknownRejected(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	_, err := runRoot(t, "run", "--once", "--task-type", "bogus", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "task type") {
		t.Fatalf("unknown task type must fail: %v", err)
	}
}

func TestRunDaemonOnceRejected(t *testing.T) {
	cfgPath, _ := newFakeHHConfig(t)
	_, err := runRoot(t, "run", "--daemon", "--once", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "взаимоисключающие") {
		t.Fatalf("--daemon --once must fail: %v", err)
	}
}

// A held task lock fails the run command fast, before any wiring.
func TestRunTaskTypeLockConflictE2E(t *testing.T) {
	cfgPath, dataDir := newFakeHHConfig(t)
	h, err := tasklock.Acquire(os.TempDir(), dataDir, tasklock.TypeMultiApply)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	_, err = runRoot(t, "run", "--once", "--task-type", "multiapply", "--config", cfgPath)
	if err == nil || !strings.Contains(err.Error(), "другой процесс") {
		t.Fatalf("held lock must fail the run: %v", err)
	}
}

// The daemon loop through the real command: one apply cycle runs against
// the fake hh, the state file persists, the STOP file ends it cleanly.
func TestRunDaemonCleanStopE2E(t *testing.T) {
	cfgPath, dataDir := newFakeHHConfig(t)
	statePath := filepath.Join(dataDir, "daemon-multiapply.state.json")
	stopPath := engine.StopFilePath(dataDir)

	done := make(chan error, 1)
	go func() {
		_, err := runRoot(t, "run", "--daemon", "--task-type", "multiapply", "--config", cfgPath)
		done <- err
	}()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(statePath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st := loadDaemonState(statePath)
	if st.Cycles != 1 {
		t.Fatalf("cycles = %d, want 1 (state %+v)", st.Cycles, st)
	}
	if err := os.WriteFile(stopPath, []byte("stop\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean stop must exit nil: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop on the STOP file")
	}
}

// Session loss on bootstrap: review parked, daemon exits with code 2.
func TestRunDaemonSessionLostExit2E2E(t *testing.T) {
	st := &improveHHState{Title: "x"}
	testSite = newFakeHHImprove(t, true, st) // settings = 403 (session lost)
	cfgPath := writeTestConfig(t, "")

	done := make(chan error, 1)
	go func() {
		_, err := runRoot(t, "run", "--daemon", "--task-type", "multiapply", "--config", cfgPath)
		done <- err
	}()
	select {
	case err := <-done:
		var ee *ExitError
		if !errors.As(err, &ee) || ee.Code != 2 {
			t.Fatalf("err = %v, want ExitError{2}", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not exit after session loss")
	}
	// The account is marked for review.
	db, err := store.Open(context.Background(), filepath.Join(filepath.Dir(cfgPath), "hhauto.db"), store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct, err := db.AccountsGet(context.Background(), "main")
	if err != nil || acct == nil {
		t.Fatalf("AccountsGet: %v %v", acct, err)
	}
	if acct.Status != "logged_out" {
		t.Fatalf("status = %q, want logged_out", acct.Status)
	}
	open, err := db.ReviewItemsOpen(context.Background(), "main", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range open {
		if r.Kind == "session_expired" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session_expired review item: %+v", open)
	}
}
