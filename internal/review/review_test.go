package review

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/audit"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

type fakeNotifier struct {
	events []webhook.Event
}

func (f *fakeNotifier) Emit(ev webhook.Event) { f.events = append(f.events, ev) }
func (f *fakeNotifier) Close(_ context.Context) error {
	return nil
}

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAddCreatesAndEmits(t *testing.T) {
	db := testDB(t)
	n := &fakeNotifier{}
	s := New(n, nil)

	id, err := s.Add(context.Background(), db, "a1", "session_expired", "bootstrap redirect", `{"target":"a1:cookies"}`)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id <= 0 {
		t.Fatalf("Add: id = %d", id)
	}
	if len(n.events) != 1 {
		t.Fatalf("events = %d, want 1", len(n.events))
	}
	if n.events[0].Type != webhook.EvNeedsHuman {
		t.Errorf("event type = %q, want %q", n.events[0].Type, webhook.EvNeedsHuman)
	}
	if n.events[0].AccountID != "a1" {
		t.Errorf("event account = %q, want a1", n.events[0].AccountID)
	}
}

func TestAddIdempotentPerKindTarget(t *testing.T) {
	db := testDB(t)
	n := &fakeNotifier{}
	s := New(n, nil)
	ctx := context.Background()

	id1, err := s.Add(ctx, db, "a1", "session_expired", "bootstrap redirect", `{"target":"a1:cookies","extra":1}`)
	if err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	id2, err := s.Add(ctx, db, "a1", "session_expired", "different reason text", `{"target":"a1:cookies"}`)
	if err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("duplicate (kind,target) returned ids %d and %d, want idempotent", id1, id2)
	}
	if len(n.events) != 1 {
		t.Fatalf("events = %d, want 1 (no duplicate notify)", len(n.events))
	}

	// Different target => a new item is created and notified.
	id3, err := s.Add(ctx, db, "a1", "session_expired", "bootstrap redirect", `{"target":"a2:cookies"}`)
	if err != nil {
		t.Fatalf("Add 3: %v", err)
	}
	if id3 == id1 {
		t.Fatal("different target must create a new item")
	}
	if len(n.events) != 2 {
		t.Fatalf("events = %d, want 2", len(n.events))
	}

	// Same kind, different account: independent.
	if _, err := s.Add(ctx, db, "a2", "session_expired", "bootstrap redirect", `{"target":"a1:cookies"}`); err != nil {
		t.Fatalf("Add other account: %v", err)
	}
	if len(n.events) != 3 {
		t.Fatalf("events = %d, want 3", len(n.events))
	}
}

func TestAddNonJSONPayloadFallsBackToWholePayload(t *testing.T) {
	db := testDB(t)
	s := New(nil, nil) // nil notifier: storage still works
	ctx := context.Background()

	id1, err := s.Add(ctx, db, "a1", "captcha_exhausted", "all providers failed", "vacancy:12345")
	if err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	id2, err := s.Add(ctx, db, "a1", "captcha_exhausted", "all providers failed", "vacancy:12345")
	if err != nil {
		t.Fatalf("Add 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("plain-payload dedup failed: %d vs %d", id1, id2)
	}
}

func TestAddAfterResolveCreatesNew(t *testing.T) {
	db := testDB(t)
	s := New(nil, nil)
	ctx := context.Background()

	id1, _ := s.Add(ctx, db, "a1", "k", "r", `{"target":"t"}`)
	if err := s.Resolve(ctx, db, id1, "fixed cookies"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	id2, err := s.Add(ctx, db, "a1", "k", "r", `{"target":"t"}`)
	if err != nil {
		t.Fatalf("Add after resolve: %v", err)
	}
	if id1 == id2 {
		t.Fatal("resolved item must not absorb a new occurrence")
	}
}

func TestResolveAndIgnore(t *testing.T) {
	db := testDB(t)
	s := New(nil, nil)
	ctx := context.Background()

	id1, _ := s.Add(ctx, db, "a1", "k", "r", `{"target":"t1"}`)
	id2, _ := s.Add(ctx, db, "a1", "k", "r", `{"target":"t2"}`)

	if err := s.Resolve(ctx, db, id1, "done manually"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := s.Ignore(ctx, db, id2); err != nil {
		t.Fatalf("Ignore: %v", err)
	}

	open, err := db.ReviewItemsOpen(ctx, "a1", 10)
	if err != nil {
		t.Fatalf("ReviewItemsOpen: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("open items = %d, want 0", len(open))
	}
}

func TestScanSLA(t *testing.T) {
	db := testDB(t)
	n := &fakeNotifier{}
	s := New(n, nil)
	ctx := context.Background()

	id, err := s.Add(ctx, db, "a1", "session_expired", "r", `{"target":"t"}`)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	now := time.Now().UTC()
	// Fresh item: within the 24h SLA — nothing happens.
	if err := s.ScanSLA(ctx, db, 24, []string{"a1"}, now); err != nil {
		t.Fatalf("ScanSLA fresh: %v", err)
	}
	if len(n.events) != 1 {
		t.Fatalf("events after fresh scan = %d, want 1 (no re-notify)", len(n.events))
	}

	// Age the item past the SLA: breach counted and re-notified once.
	breachAt := now.Add(30 * time.Hour)
	if err := s.ScanSLA(ctx, db, 24, []string{"a1"}, breachAt); err != nil {
		t.Fatalf("ScanSLA breach: %v", err)
	}
	if len(n.events) != 2 {
		t.Fatalf("events after breach scan = %d, want 2", len(n.events))
	}
	m, err := db.MetricsSnapshot(ctx, audit.DayAt(breachAt))
	if err != nil {
		t.Fatalf("MetricsSnapshot: %v", err)
	}
	if m[audit.KeyReviewSLABreach] != 1 {
		t.Errorf("review_sla_breach = %d, want 1", m[audit.KeyReviewSLABreach])
	}

	// A second scan must not re-notify (feature_state latch).
	if err := s.ScanSLA(ctx, db, 24, []string{"a1"}, now.Add(31*time.Hour)); err != nil {
		t.Fatalf("ScanSLA repeat: %v", err)
	}
	if len(n.events) != 2 {
		t.Fatalf("events after repeat scan = %d, want 2 (latched)", len(n.events))
	}
	m, _ = db.MetricsSnapshot(ctx, audit.DayAt(breachAt))
	if m[audit.KeyReviewSLABreach] != 1 {
		t.Errorf("review_sla_breach after repeat = %d, want 1 (latched)", m[audit.KeyReviewSLABreach])
	}
	_ = id
}
