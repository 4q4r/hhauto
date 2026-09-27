package cli

// T35 multi-instance E2E: two hhauto instances with DIFFERENT task types
// run in parallel against one data dir — the автоотклик daemon (multiapply
// lock, sleeping between cycles) and the resume-improvement command
// (resume_improve lock). Neither blocks the other; a second instance of
// the SAME type would fail fast (covered in the tasklock package tests).

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/engine"
)

func TestMultiInstanceParallelTaskTypesE2E(t *testing.T) {
	st := &improveHHState{Title: "Базовое резюме"}
	testSite = newFakeHHImprove(t, false, st)
	cfgPath := writeTestConfig(t, t35ImproveExtra(newFakeAI(t, t35Verdict)))
	t.Setenv("T35_AI_KEY", "test-key")
	dataDir := filepath.Dir(cfgPath)
	seedBaseResumeSnapshot(t, dataDir)

	statePath := filepath.Join(dataDir, "daemon-multiapply.state.json")
	stopPath := engine.StopFilePath(dataDir)

	// Instance 1: the apply daemon claims the multiapply type and parks
	// itself on the 24h sleep after the first cycle.
	daemonDone := make(chan error, 1)
	go func() {
		_, err := runRoot(t, "run", "--daemon", "--task-type", "multiapply", "--config", cfgPath)
		daemonDone <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st := loadDaemonState(statePath); st.Cycles == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := loadDaemonState(statePath); st.Cycles != 1 {
		t.Fatalf("daemon cycle 1 did not complete: %+v", st)
	}

	// Instance 2: the resume improve command claims the resume_improve
	// type and applies WHILE the daemon holds multiapply — no conflict.
	out, err := runRoot(t, "resume", "improve", "--resume", "b4se1", "--config", cfgPath, "--confirm")
	if err != nil {
		t.Fatalf("improve must run beside the daemon: %v\n%s", err, out)
	}
	mustContain(t, out, "применено")
	if got := st.title(); got != "DevOps-инженер (автоматизация)" {
		t.Fatalf("resume title after improve = %q", got)
	}

	// Instance 1 keeps running (its lock never blocked instance 2): stop
	// it cleanly.
	if err := os.WriteFile(stopPath, []byte("stop\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-daemonDone:
		if err != nil {
			t.Fatalf("daemon must exit cleanly on STOP: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if st := loadDaemonState(statePath); st.Cycles != 1 {
		t.Fatalf("daemon cycles changed unexpectedly: %+v", st)
	}
}
