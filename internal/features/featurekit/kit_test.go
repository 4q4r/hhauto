package featurekit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"hhauto/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "kit.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- BeginIntent: the four store outcomes map onto steps ---

func TestBeginIntentProceedThenSkipPending(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	c1, err := BeginIntent(ctx, db, "apply:acc:777", "{}")
	if err != nil {
		t.Fatalf("first BeginIntent: %v", err)
	}
	if c1.Step != IntentProceed || c1.ID == "" {
		t.Fatalf("first claim = %+v, want proceed with id", c1)
	}

	c2, err := BeginIntent(ctx, db, "apply:acc:777", "{}")
	if err != nil {
		t.Fatalf("second BeginIntent: %v", err)
	}
	if c2.Step != IntentSkipPending {
		t.Fatalf("second claim step = %v, want IntentSkipPending", c2.Step)
	}
}

func TestBeginIntentDoneMeansReconcileFirst(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	c1, _ := BeginIntent(ctx, db, "lift:h:win", "{}")
	if err := db.IntentsMarkDone(ctx, c1.ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	c2, err := BeginIntent(ctx, db, "lift:h:win", "{}")
	if err != nil {
		t.Fatalf("second BeginIntent: %v", err)
	}
	if c2.Step != IntentReconcileFirst {
		t.Fatalf("step = %v, want IntentReconcileFirst", c2.Step)
	}
}

func TestBeginIntentAbandonedIsReplaced(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	c1, _ := BeginIntent(ctx, db, "apply:acc:778", "{}")
	if err := db.IntentsMarkAbandoned(ctx, c1.ID); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	c2, err := BeginIntent(ctx, db, "apply:acc:778", "{}")
	if err != nil {
		t.Fatalf("re-begin: %v", err)
	}
	if c2.Step != IntentProceed {
		t.Fatalf("step = %v, want proceed after abandon", c2.Step)
	}
	if c2.ID == c1.ID {
		t.Fatalf("intent id reused after abandon: %s", c2.ID)
	}
}

func TestFinishIntentDoneAndFailed(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	c, _ := BeginIntent(ctx, db, "k1", "{}")
	if err := FinishIntent(ctx, db, c, nil); err != nil {
		t.Fatalf("finish nil: %v", err)
	}
	// done is a hard barrier: the next begin must ask for reconcile.
	c2, _ := BeginIntent(ctx, db, "k1", "{}")
	if c2.Step != IntentReconcileFirst {
		t.Fatalf("after finish(nil) step = %v, want reconcile-first", c2.Step)
	}

	c3, _ := BeginIntent(ctx, db, "k2", "{}")
	if err := FinishIntent(ctx, db, c3, errors.New("boom")); err != nil {
		t.Fatalf("finish err: %v", err)
	}
	// failed intents are re-armed by Begin.
	c4, _ := BeginIntent(ctx, db, "k2", "{}")
	if c4.Step != IntentProceed {
		t.Fatalf("after finish(err) step = %v, want proceed", c4.Step)
	}
}

func TestAbandonIntentAllowsRetry(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	c, _ := BeginIntent(ctx, db, "k3", "{}")
	if err := AbandonIntent(ctx, db, c); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	c2, _ := BeginIntent(ctx, db, "k3", "{}")
	if c2.Step != IntentProceed {
		t.Fatalf("step = %v, want proceed", c2.Step)
	}
}

// --- NoLinksRule ---

func TestNoLinksRule(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"plain text", "Здравствуйте! Готов к собеседованию.", false},
		{"empty", "", false},
		{"http url", "смотрите http://example.com/cv", true},
		{"https url", "портфолио: https://example.com", true},
		{"www", "сайт www.example.com хороший", true},
		{"t.me", "пишите в t.me/myhandle", true},
		{"telegram.me", "telegram.me/myhandle", true},
		{"at-handle", "мой ник @myhandle", true},
		{"email", "почта me@example.com", true},
		{"no false positive on at-word inside word", "задача@дом — нет, это тоже контакт", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NoLinksRule(tc.text)
			if tc.wantErr && err == nil {
				t.Fatalf("NoLinksRule(%q) = nil, want error", tc.text)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("NoLinksRule(%q) = %v, want nil", tc.text, err)
			}
		})
	}
}

func TestNoLinksRuleMentionsMatch(t *testing.T) {
	err := NoLinksRule("look at https://evil.example")
	if err == nil || !strings.Contains(err.Error(), "https://evil.example") {
		t.Fatalf("err = %v, want message containing the offending fragment", err)
	}
}

// --- DedupKey / windows / day ---

func TestDedupKeyStableAndDistinct(t *testing.T) {
	a := DedupKey("acc1", "hash1", 777)
	b := DedupKey("acc1", "hash1", 777)
	c := DedupKey("acc1", "hash1", 778)
	d := DedupKey("acc2", "hash1", 777)
	if a != b || a == c || a == d {
		t.Fatalf("dedup keys wrong: %q %q %q %q", a, b, c, d)
	}
	if len(a) != 32 { // sha256 truncated to 16 bytes hex
		t.Fatalf("dedup key length = %d, want 32", len(a))
	}
}

func TestLiftWindowBuckets4h(t *testing.T) {
	w1 := LiftWindow(timeAt(2026, 9, 24, 7, 59))
	w2 := LiftWindow(timeAt(2026, 9, 24, 4, 1))
	w3 := LiftWindow(timeAt(2026, 9, 24, 8, 0))
	w4 := LiftWindow(timeAt(2026, 9, 24, 11, 59))
	if w1 != w2 {
		t.Fatalf("07:59 and 04:01 must share the 04-08 window: %q vs %q", w1, w2)
	}
	if w1 == w3 {
		t.Fatalf("07:59 and 08:00 must be different windows (boundary between), both %q", w1)
	}
	if w3 != w4 {
		t.Fatalf("08:00 and 11:59 must share the 08-12 window: %q vs %q", w3, w4)
	}
}

func TestDayUTC(t *testing.T) {
	got := DayUTC(timeAt(2026, 9, 24, 23, 5))
	if got != "2026-09-24" {
		t.Fatalf("DayUTC = %q", got)
	}
}
