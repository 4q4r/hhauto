package e2e

// E2E-3 chat-flow: one inbox sync through the real wiring — an inbound
// question is classified and auto-replied (send recorded by the fake, reply
// intent done), an internal test assignment is answered and submitted, and
// the structurally refused negotiation is routed to the rejection state
// with zero sends and zero leaves (auto-leave disabled).

import (
	"testing"
	"time"

	"hhauto/internal/features/featurekit"
)

const testAnswer = "Отвечу подробно на интервью: тема мне хорошо знакома."

func TestE2E3ChatFlow(t *testing.T) {
	env := newEnv(t, nil)

	cancel, done := env.runEngine()
	waitUntil(t, 30*time.Second, "question reply, test answers and full sync settled", func() bool {
		// chatops.other==1 is the warmup chat's inbound message — the last
		// chat of the list — so the whole sync pass has been routed.
		return env.hh.sendCount(chatQuestion) == 1 && env.hh.sendCount(chatTest) == 1 &&
			env.hh.chatDataHits(chatWarmup) >= 1 && env.metricValue("chatops.other") == 1
	})
	env.stopEngine(cancel, done)

	// --- question → chat_reply → auto-send ---
	sends := env.hh.sendsSnapshot()
	var reply *sendPost
	var submit *sendPost
	for i := range sends {
		switch sends[i].ChatID {
		case chatQuestion:
			reply = &sends[i]
		case chatTest:
			submit = &sends[i]
		}
	}
	if reply == nil || reply.Text != e2eReply {
		t.Fatalf("question reply = %+v, want the fixed fake-AI draft", reply)
	}
	if reply.IdempotencyKey == "" {
		t.Fatal("reply send carries no idempotency key")
	}
	if step := env.intentStep("msg:" + acctID + ":" + chatQuestion + ":" + questionMessageID); step != featurekit.IntentReconcileFirst {
		t.Fatalf("reply intent step = %v, want done", step)
	}
	if got := env.metricValue("chatops.reply_sent"); got != 1 {
		t.Fatalf("chatops.reply_sent = %d, want 1", got)
	}

	// --- test assignment → answers → one-shot submit ---
	wantSubmit := "1. " + testAnswer + "\n2. " + testAnswer
	if submit == nil || submit.Text != wantSubmit {
		t.Fatalf("test submit = %+v, want %q", submit, wantSubmit)
	}
	if step := env.intentStep("test:" + acctID + ":hh:" + chatTest); step != featurekit.IntentReconcileFirst {
		t.Fatalf("test submit intent step = %v, want done", step)
	}
	if got := env.metricValue("chatops.test_submitted"); got != 1 {
		t.Fatalf("chatops.test_submitted = %d, want 1", got)
	}

	// --- structural rejection (R9): refused negotiation → rejected state,
	// no classification call, no reply, no auto-leave ---
	if state := env.chatMirrorState(chatRejection); state != "rejected" {
		t.Fatalf("rejection chat mirror state = %q, want rejected", state)
	}
	if got := env.metricValue("chatops.rejection_structural"); got != 1 {
		t.Fatalf("chatops.rejection_structural = %d, want 1", got)
	}
	if got := env.metricValue("chatops.rejection_detected"); got != 1 {
		t.Fatalf("chatops.rejection_detected = %d, want 1", got)
	}
	if got := env.hh.sendCount(chatRejection); got != 0 {
		t.Fatalf("rejection chat sends = %d, want 0", got)
	}
	_, _, leaves, _ := env.hh.stats()
	if leaves != 0 {
		t.Fatalf("auto-leave is disabled but %d leave(s) were executed", leaves)
	}

	// The AI saw the question, the test assignment and the warmup chat's
	// inbound message — never the rejection (structural, R9).
	if got := env.ai.callCount("message_classification"); got != 3 {
		t.Fatalf("classification calls = %d, want 3 (structural rejection skips the LLM)", got)
	}
	if got := env.ai.callCount("chat_reply"); got != 1 {
		t.Fatalf("chat_reply calls = %d, want 1", got)
	}
	if got := env.ai.callCount("test_answer"); got != 1 {
		t.Fatalf("test_answer calls = %d, want 1", got)
	}
}
