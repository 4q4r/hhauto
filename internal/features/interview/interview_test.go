package interview

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// --- fakes ---

type fakeRunner struct {
	verdict func(task ai.TaskName) (json.RawMessage, error)
	calls   []ai.TaskName
}

func (f *fakeRunner) Run(_ context.Context, _ string, _, _ string, _ string, task ai.TaskName, _ any) (json.RawMessage, ai.Decision, error) {
	f.calls = append(f.calls, task)
	v, err := f.verdict(task)
	return v, ai.Decision{Task: task}, err
}

type fakeNotifier struct {
	events []webhook.Event
}

func (f *fakeNotifier) Emit(ev webhook.Event) { f.events = append(f.events, ev) }
func (f *fakeNotifier) Close(context.Context) error {
	return nil
}

func newDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func linkVerdict(links ...map[string]any) func(ai.TaskName) (json.RawMessage, error) {
	return func(task ai.TaskName) (json.RawMessage, error) {
		if task != ai.TaskLinkDetection {
			return nil, errors.New("unexpected task " + string(task))
		}
		return json.Marshal(map[string]any{
			"confidence": 0.9, "needs_human": false, "needs_human_reason": nil,
			"injection_suspected": false, "links": links,
		})
	}
}

func detector(t *testing.T, db *store.DB, runner ai.Runner, n *fakeNotifier) *Detector {
	t.Helper()
	return &Detector{
		DB:     db,
		AI:     runner,
		Notify: n,
		Cfg:    config.InterviewDetect{Enabled: true},
		Log:    slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
	}
}

// --- tests ---

func TestDetectDisabledIsNoop(t *testing.T) {
	db := newDB(t)
	n := &fakeNotifier{}
	d := detector(t, db, &fakeRunner{verdict: linkVerdict()}, n)
	d.Cfg.Enabled = false

	res, err := d.Detect(t.Context(), "acc", 1, "chat1", "Завтра в 15:00 созвон https://zoom.us/j/123")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(res.Links) != 0 || res.When != "" || len(n.events) != 0 {
		t.Fatalf("disabled detector must be a no-op, got %+v", res)
	}
}

func TestDetectStoresLinkAndDateAndNotifies(t *testing.T) {
	db := newDB(t)
	n := &fakeNotifier{}
	runner := &fakeRunner{verdict: linkVerdict(map[string]any{"url": "https://zoom.us/j/123", "kind": "calendar", "risk": "low"})}
	d := detector(t, db, runner, n)

	res, err := d.Detect(t.Context(), "acc", 7, "chat42", "Приглашаем на созвон! Завтра в 15:00, вот ссылка https://zoom.us/j/123")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != ai.TaskLinkDetection {
		t.Fatalf("expected one link_detection call, got %v", runner.calls)
	}
	if len(res.Links) != 1 {
		t.Fatalf("expected 1 stored link, got %d", len(res.Links))
	}
	l := res.Links[0]
	if l.AccountID != "acc" || l.URL != "https://zoom.us/j/123" || l.Provider != "calendar" {
		t.Fatalf("bad link row: %+v", l)
	}
	if l.ChatID == nil || *l.ChatID != 7 {
		t.Fatalf("chat id not attached: %+v", l)
	}
	if l.Status != "detected" {
		t.Fatalf("status = %q", l.Status)
	}
	if res.When == "" {
		t.Fatal("date not extracted from «Завтра в 15:00»")
	}
	stored, ok, err := db.FeatureStateGet(t.Context(), "interview", "acc", "date:chat42")
	if err != nil || !ok || stored != res.When {
		t.Fatalf("date not persisted: %q ok=%v err=%v", stored, ok, err)
	}
	if len(n.events) != 1 || n.events[0].Type != webhook.EvInterviewFound {
		t.Fatalf("expected one EvInterviewFound, got %+v", n.events)
	}
}

func TestDetectHighRiskLinkGoesToReviewNotStore(t *testing.T) {
	db := newDB(t)
	n := &fakeNotifier{}
	runner := &fakeRunner{verdict: linkVerdict(map[string]any{"url": "https://sketchy.example/inv", "kind": "other", "risk": "high"})}
	d := detector(t, db, runner, n)

	res, err := d.Detect(t.Context(), "acc", 1, "chat1", "подтвердите https://sketchy.example/inv")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(res.Links) != 0 {
		t.Fatalf("high-risk link must not be stored: %+v", res.Links)
	}
	items, err := db.ReviewItemsOpen(t.Context(), "acc", 10)
	if err != nil || len(items) != 1 || items[0].Kind != "phishing_suspect" {
		t.Fatalf("expected phishing_suspect review, got %+v err=%v", items, err)
	}
}

func TestDetectDuplicateLinkUpsertsOnce(t *testing.T) {
	db := newDB(t)
	n := &fakeNotifier{}
	runner := &fakeRunner{verdict: linkVerdict(map[string]any{"url": "https://meet.google.com/abc", "kind": "calendar", "risk": "low"})}
	d := detector(t, db, runner, n)

	if _, err := d.Detect(t.Context(), "acc", 1, "chat1", "созвон https://meet.google.com/abc в 15:00"); err != nil {
		t.Fatalf("first Detect: %v", err)
	}
	res, err := d.Detect(t.Context(), "acc", 1, "chat1", "повтор: https://meet.google.com/abc")
	if err != nil {
		t.Fatalf("second Detect: %v", err)
	}
	if len(res.Links) != 1 {
		t.Fatalf("duplicate URL must upsert to one row, got %d", len(res.Links))
	}
}

func TestDetectWithoutLinksOrDateIsQuiet(t *testing.T) {
	db := newDB(t)
	n := &fakeNotifier{}
	d := detector(t, db, &fakeRunner{verdict: linkVerdict()}, n)

	res, err := d.Detect(t.Context(), "acc", 1, "chat1", "Давайте созвонимся на следующей неделе.")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(res.Links) != 0 || res.When != "" || len(n.events) != 0 {
		t.Fatalf("quiet detect expected, got %+v events=%d", res, len(n.events))
	}
}

func TestDetectAIFailureReturnsError(t *testing.T) {
	db := newDB(t)
	d := detector(t, db, &fakeRunner{verdict: func(ai.TaskName) (json.RawMessage, error) {
		return nil, errors.New("llm down")
	}}, &fakeNotifier{})
	if _, err := d.Detect(t.Context(), "acc", 1, "chat1", "see https://x.example"); err == nil {
		t.Fatal("AI failure must be returned")
	}
}
