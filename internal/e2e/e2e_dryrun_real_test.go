package e2e

// E2E-7 dry-run → real transition and E2E-8 kill switch mid-apply:
//
//   - E2E-7 runs the whole engine dry (a full-fidelity survey: nothing is
//     sent), then flips the SAME deployment to a real run on the same store
//     and requires the sends to happen (the dry pass must not consume any
//     send barrier — the §6 intents contract).
//   - E2E-8 trips the STOP file in the middle of the apply batch (the fake
//     hh's first mutation POST writes it): the vacancy in flight settles,
//     the rest of the batch is not started.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/features/featurekit"
)

func TestE2E7DryRunThenRealRunSends(t *testing.T) {
	env := newEnv(t, func(o *envOptions) { o.dryRun = true })

	// --- pass 1: dry-run settles the batch without a single send ---
	cancel, done := env.runEngine()
	waitUntil(t, 30*time.Second, "dry-run batch settled (survey row + chats synced)", func() bool {
		app := env.application(vacClean)
		return app != nil && app.Status == "dry_run" &&
			env.hh.chatDataHits(chatQuestion) >= 1
	})
	env.stopEngine(cancel, done)
	if sends := env.hh.sendsSnapshot(); len(sends) != 0 {
		t.Fatalf("dry-run sent %d message(s): %+v", len(sends), sends)
	}

	// --- pass 2: flip the same deployment to a real run ---
	raw, err := os.ReadFile(env.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	realTOML := strings.Replace(string(raw), "dry_run = true", "dry_run = false", 1)
	if realTOML == string(raw) {
		t.Fatal("config template changed: dry_run line not found")
	}
	if err := os.WriteFile(env.cfgPath, []byte(realTOML), 0o600); err != nil {
		t.Fatal(err)
	}

	// The dry pass legitimately stored the first inbound messages (a dry-run
	// is a full-fidelity survey); the real pass needs fresh inbound work.
	env.hh.appendInbound(chatQuestion, "m-q-2", questionText)
	env.hh.appendInbound(chatTest, "m-t-2", testText)

	cancel, done = env.runEngine()
	// End-state includes the settled intents: a send that lands before the
	// final MarkDone would leave a pending intent behind the stop.
	waitUntil(t, 30*time.Second, "real run: reply and submit delivered, intents done", func() bool {
		return env.hh.sendCount(chatQuestion) == 1 && env.hh.sendCount(chatTest) == 1 &&
			env.intentStep("msg:"+acctID+":"+chatQuestion+":m-q-2") == featurekit.IntentReconcileFirst &&
			env.intentStep("test:"+acctID+":hh:"+chatTest) == featurekit.IntentReconcileFirst
	})
	env.stopEngine(cancel, done)

	// The dry pass consumed nothing: exactly one reply, exactly one submit.
	if got := env.metricValue("chatops.reply_sent"); got != 1 {
		t.Fatalf("chatops.reply_sent = %d, want 1", got)
	}
	if got := env.metricValue("chatops.test_submitted"); got != 1 {
		t.Fatalf("chatops.test_submitted = %d, want 1", got)
	}
}

func TestE2E8KillSwitchMidApply(t *testing.T) {
	// chatops is off so the only hh mutations are the applies: the first
	// one is the apply itself and the trip lands mid-apply-batch.
	env := newEnv(t, func(o *envOptions) { o.chatsOff = true })

	// The first mutation POST on the fake trips the STOP file (channel 2).
	// Every LATER mutation holds the wire for >1s so the batch spans the
	// kill-switch fs-cache TTL: the vacancy in flight settles (real hh
	// mutations take hundreds of ms too), and the between-vacancies check
	// reads a fresh (tripped) state before the next vacancy starts.
	stopPath := filepath.Join(env.dataDir, "STOP")
	var tripped bool
	env.hh.onMutation = func() {
		if !tripped {
			tripped = true
			if err := os.WriteFile(stopPath, []byte("stop\n"), 0o600); err != nil {
				t.Errorf("write STOP: %v", err)
			}
			return
		}
		time.Sleep(1100 * time.Millisecond)
	}

	cancel, done := env.runEngine()
	// The first vacancy applies; the switch trips during its mutation POST.
	waitUntil(t, 30*time.Second, "first apply landed", func() bool {
		return env.metricValue("apply.ok") >= 1
	})
	// Grace window: past the fs-cache TTL every subsequent check must be
	// fresh, so the batch cannot advance any further.
	time.Sleep(2 * time.Second)
	env.stopEngine(cancel, done)

	if !tripped {
		t.Fatal("the scenario never mutated hh — no mid-apply trip happened")
	}
	if got := env.metricValue("apply.ok"); got < 1 || got > 2 {
		t.Fatalf("apply.ok = %d, want 1..2: the batch must apply, then stop", got)
	}
	// The LAST vacancy (captcha, 105) must never be attempted: the batch
	// stopped before it, leaving neither a challenge nor an application row.
	if env.hh.applyTryCount(vacCaptcha) != 0 {
		t.Fatalf("vacancy %d was attempted after the trip", vacCaptcha)
	}
	if got := env.metricValue("apply.error"); got != 0 {
		t.Fatalf("apply.error = %d, want 0 (the captcha vacancy was never attempted)", got)
	}
	if app := env.application(vacCaptcha); app != nil {
		t.Fatalf("vacancy %d recorded an application after the trip: %+v", vacCaptcha, app)
	}
}
