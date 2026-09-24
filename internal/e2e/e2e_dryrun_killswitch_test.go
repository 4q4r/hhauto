package e2e

// E2E-5 dry-run end-to-end and E2E-6 kill-switch: the two safety switches
// observed at the fake-server boundary. Dry-run processes every task but
// leaves zero hh mutations (the mutation POST counter stays at zero while
// store writes and AI calls still happen). A STOP file present before
// start makes the engine trip before any worker executes anything: the
// fake sees not a single request.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestE2E5DryRunEndToEnd(t *testing.T) {
	env := newEnv(t, func(o *envOptions) {
		o.dryRun = true
	})

	cancel, done := env.runEngine()
	// Tail marker: apply.dry_run==3 means all cascade-passing vacancies
	// (101, 104, 105) got their survey rows — the batch is fully settled.
	waitUntil(t, 30*time.Second, "dry-run survey rows recorded and chats synced", func() bool {
		app := env.application(vacClean)
		return app != nil && app.Status == "dry_run" &&
			env.metricValue("apply.dry_run") == 3 &&
			env.hh.chatDataHits(chatQuestion) >= 1
	})
	env.stopEngine(cancel, done)

	mutations, total, leaves, negotiationAppends := env.hh.stats()
	if mutations != 0 {
		t.Fatalf("dry-run produced %d hh mutation POST(s), want 0", mutations)
	}
	if total == 0 {
		t.Fatal("fake hh saw no requests at all — the run did not touch the scenario")
	}
	if leaves != 0 {
		t.Fatalf("dry-run executed %d chat leave(s)", leaves)
	}
	if negotiationAppends != 0 {
		t.Fatalf("dry-run created %d negotiation(s) on the fake", negotiationAppends)
	}
	if sends := env.hh.sendsSnapshot(); len(sends) != 0 {
		t.Fatalf("dry-run sent %d chat message(s): %+v", len(sends), sends)
	}

	// The work still happened: survey rows (including the captcha vacancy —
	// dry-run stops before the apply, so no captcha challenge fires), the
	// AI-built letter, dry-run metrics, no applied rows, no ok-applies.
	app := env.application(vacClean)
	if app == nil || app.Status != "dry_run" {
		t.Fatalf("dry-run application = %+v, want status dry_run", app)
	}
	if app.Letter == nil || *app.Letter != e2eLetter {
		t.Fatalf("dry-run letter = %v, want the fake-AI draft (the letter path still runs)", app.Letter)
	}
	if capApp := env.application(vacCaptcha); capApp == nil || capApp.Status != "dry_run" {
		t.Fatalf("captcha vacancy in dry-run = %+v, want a dry_run survey row", capApp)
	}
	if got := env.metricValue("apply.dry_run"); got != 3 {
		t.Fatalf("apply.dry_run = %d, want 3", got)
	}
	if got := env.metricValue("apply.ok"); got != 0 {
		t.Fatalf("apply.ok = %d, want 0 in dry-run", got)
	}
	if got := env.metricValue("apply.error"); got != 0 {
		t.Fatalf("apply.error = %d, want 0 in dry-run", got)
	}
	if got := env.ai.callCount("cover_letter"); got < 1 {
		t.Fatalf("cover_letter calls = %d, want >= 1 (AI is not an hh mutation)", got)
	}
}

func TestE2E6KillSwitchStopsWorkers(t *testing.T) {
	env := newEnv(t, nil)

	// Channel 2 of the kill switch: the STOP file exists before start.
	if err := os.WriteFile(filepath.Join(env.dataDir, "STOP"), []byte("stop\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cancel, done := env.runEngine()
	// The engine must shut down ON ITS OWN via the kill-switch watcher —
	// no cancel here: a context cancellation would trip the same switch
	// through channel 1 and prove nothing about the file channel.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("engine.Run: %v", err)
		}
	case <-time.After(45 * time.Second):
		cancel()
		t.Fatal("engine did not react to the STOP file within 45s")
	}

	mutations, total, leaves, negotiationAppends := env.hh.stats()
	if total != 0 {
		t.Fatalf("kill-switch run hit the fake %d time(s), want 0 requests", total)
	}
	if mutations != 0 || leaves != 0 || negotiationAppends != 0 {
		t.Fatalf("kill-switch run mutated hh: mutations=%d leaves=%d negotiations=%d",
			mutations, leaves, negotiationAppends)
	}
	if app := env.application(vacClean); app != nil {
		t.Fatalf("kill-switch run recorded an application: %+v", app)
	}
	if got := env.metricValue("apply.ok"); got != 0 {
		t.Fatalf("apply.ok = %d, want 0", got)
	}
	// No review items, no side effects in the store at all.
	ctx, cancelCtx := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCtx()
	open, err := env.db.ReviewItemsOpen(ctx, acctID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("kill-switch run opened review items: %+v", open)
	}
}
