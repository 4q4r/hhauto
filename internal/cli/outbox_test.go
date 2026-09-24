package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/store"
)

func newTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestStoreOutboxBridge(t *testing.T) {
	db := newTestStore(t)
	o := newStoreOutbox(db)

	if err := o.Add("ep-1", []byte(`{"event":"x"}`)); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	due, err := o.Due(now, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %v %v", due, err)
	}
	if due[0].EndpointID != "ep-1" || string(due[0].Payload) != `{"event":"x"}` {
		t.Fatalf("item round-trip: %+v", due[0])
	}

	// MarkFailed delays the item in memory.
	if err := o.MarkFailed(due[0].ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	due, err = o.Due(now, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("delayed item still due: %v %v", due, err)
	}
	due, err = o.Due(now.Add(2*time.Hour), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("delayed item not due after the window: %v %v", due, err)
	}

	// MarkDelivered clears it for good.
	if err := o.MarkDelivered(due[0].ID); err != nil {
		t.Fatal(err)
	}
	due, err = o.Due(now.Add(3*time.Hour), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("delivered item still due: %v %v", due, err)
	}
}

// TestAILedgerBridgeRoundTrip: the bridge maps ai.DecisionRecord onto the
// store DAO; the budget guard reads the same numbers back.
func TestAILedgerBridgeRoundTrip(t *testing.T) {
	db := newTestStore(t)
	l := aiLedgerBridge{db: db}

	in := &ai.DecisionRecord{
		Decision: ai.Decision{
			ID: "01TEST", CreatedAt: time.Now().UTC(), AccountID: "main",
			Task: ai.TaskVacancyScoring, EntityType: "vacancy", EntityID: "1",
			Model: "m1", PromptVer: "v1", InputDigest: "d1",
			Verdict: json.RawMessage(`{"score":80}`),
			Score:   80, Confidence: 0.9,
			TokensIn: 100, TokensOut: 20,
			Outcome: "auto",
		},
		CostUSD: 0.0021,
	}
	if err := l.Add(in); err != nil {
		t.Fatal(err)
	}
	day := in.CreatedAt.UTC().Format("2006-01-02")
	tokens, usd := l.SumTokensDay(day)
	if tokens != 120 {
		t.Fatalf("tokens = %d, want 120", tokens)
	}
	if usd <= 0 {
		t.Fatalf("usd = %v, want > 0", usd)
	}
	// A day without records reads as zero (budget guard contract).
	if got, _ := l.SumTokensDay("1999-01-01"); got != 0 {
		t.Fatalf("stale day tokens = %d", got)
	}
}
