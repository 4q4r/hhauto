package e2e

// E2E-4 warmup+pings: the chat whose last outgoing message went silent
// beyond [warmup].silence_hours gets marked warmup_due by the chatops sync
// pass; the warmup scan consumes it, sends the first ping from the
// configured list and advances the ladder. Run #1 establishes the mirror
// (inbound message resets the ladder, no ping); run #2 is the second full
// engine start on the same store where the silence branch fires.

import (
	"testing"
	"time"

	"hhauto/internal/features/featurekit"
)

func TestE2E4WarmupPing(t *testing.T) {
	env := newEnv(t, func(o *envOptions) {
		o.warmupSilence = 1 // hours: the 10h-old outgoing message is silent
		o.warmupLadder = []int{8}
	})

	// --- run #1: the sync stores the history; inbound resets the ladder,
	// the chat stays active and the warmup scan finds nothing due ---
	cancel, done := env.runEngine()
	waitUntil(t, 30*time.Second, "first sync processed the warmup chat", func() bool {
		// chatops.other==1: the warmup chat's inbound message was routed —
		// the last chat of the list, so the pass is complete.
		return env.hh.chatDataHits(chatWarmup) >= 1 && env.metricValue("chatops.other") == 1
	})
	env.stopEngine(cancel, done)

	if got := env.featureState("warmup", "ladder:"+chatWarmup); got != "0" {
		t.Fatalf("ladder after first sync = %q, want 0", got)
	}
	if got := env.hh.sendCount(chatWarmup); got != 0 {
		t.Fatalf("first run pinged the chat %d time(s), want 0", got)
	}

	// --- run #2: no new inbound → chatops marks warmup_due → the warmup
	// scan sends ping stage 1 from the list ---
	cancel2, done2 := env.runEngine()
	// warmup.ping is the LAST write of completePing (after intent-done,
	// ping ledger and ladder): seeing it means the stage is fully settled.
	waitUntil(t, 30*time.Second, "warmup ping sent from the list", func() bool {
		return env.hh.sendCount(chatWarmup) == 1 && env.metricValue("warmup.ping") == 1
	})
	env.stopEngine(cancel2, done2)

	sends := env.hh.sendsSnapshot()
	var ping *sendPost
	for i := range sends {
		if sends[i].ChatID == chatWarmup {
			ping = &sends[i]
		}
	}
	if ping == nil || ping.Text != e2ePing {
		t.Fatalf("warmup ping = %+v, want the first list entry %q", ping, e2ePing)
	}
	if ping.IdempotencyKey == "" {
		t.Fatal("warmup ping carries no idempotency key")
	}
	// ladder++ persisted by completePing.
	if got := env.featureState("warmup", "ladder:"+chatWarmup); got != "1" {
		t.Fatalf("ladder after ping = %q, want 1", got)
	}
	if got := env.metricValue("warmup.ping"); got != 1 {
		t.Fatalf("warmup.ping metric = %d, want 1", got)
	}
	if step := env.intentStep("ping:" + chatWarmup + ":1"); step != featurekit.IntentReconcileFirst {
		t.Fatalf("warmup ping intent step = %v, want done", step)
	}
	_, _, leaves, _ := env.hh.stats()
	if leaves != 0 {
		t.Fatalf("warmup must not leave chats, got %d leave(s)", leaves)
	}
}
