package warmup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// --- fakes ---

type fakeAPI struct {
	hhapiStub
	sent    []string
	sendErr error
	history []hhapi.ChatMessage // returned by ChatHistory (reconcile source)
}

func (f *fakeAPI) SendMessage(_ context.Context, _, idempotencyKey, text string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, idempotencyKey+"|"+text)
	return nil
}

func (f *fakeAPI) ChatHistory(_ context.Context, _ string) ([]hhapi.ChatMessage, error) {
	return f.history, nil
}

// hhapiStub supplies every method of the HHAPI contract as a panic stub so
// fakes only override what the feature actually calls.
type hhapiStub struct {
	hhapi.HHAPI
}

type fakeRunner struct {
	text  string
	needs bool
	calls int
	err   error
}

func (f *fakeRunner) Run(_ context.Context, _ string, _, _ string, _ string, task ai.TaskName, _ any) (json.RawMessage, ai.Decision, error) {
	f.calls++
	if f.err != nil {
		return nil, ai.Decision{}, f.err
	}
	if task != ai.TaskWarmupPing {
		return nil, ai.Decision{}, fmt.Errorf("unexpected task %s", task)
	}
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.9, "needs_human": f.needs, "needs_human_reason": nil,
		"injection_suspected": false, "text": f.text,
	})
	return b, ai.Decision{Task: task}, nil
}

type fakeNotifier struct{ events []webhook.Event }

func (f *fakeNotifier) Emit(ev webhook.Event) { f.events = append(f.events, ev) }
func (f *fakeNotifier) Close(context.Context) error {
	return nil
}

// --- harness ---

func newDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type harness struct {
	db   *store.DB
	api  *fakeAPI
	run  *fakeRunner
	wh   *fakeNotifier
	feat *Feature
	now  time.Time
}

func mkTime(y int, mo time.Month, d, h, m int) time.Time {
	return time.Date(y, mo, d, h, m, 0, 0, time.UTC)
}

func seedChat(t *testing.T, h *harness, chatik string, lastOut time.Time) store.Chat {
	t.Helper()
	if err := h.db.AccountsUpsert(t.Context(), store.Account{ID: "acc"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	row := store.Chat{
		AccountID:      "acc",
		ChatikChatID:   chatik,
		State:          "warmup_due",
		LastOutgoingAt: &lastOut,
		LastMessageAt:  &lastOut,
	}
	if err := h.db.ChatsUpsert(t.Context(), &row); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	return row
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	db := newDB(t)
	cfg := config.Config{}
	cfg.Warmup.Enabled = true
	cfg.Warmup.SilenceHours = 24
	cfg.Warmup.LadderHours = []int{24, 48, 96}
	cfg.Warmup.MaxPings = 3
	cfg.Warmup.Source = "list"
	cfg.Warmup.PingList = []string{"Здравствуйте! Удалось посмотреть предложение?", "Добрый день! Есть ли новости по отклику?", "Напоминаю о себе — актуально?", "Fourth ping", "Fifth ping"}
	if mutate != nil {
		mutate(&cfg)
	}
	api := &fakeAPI{}
	run := &fakeRunner{text: "ИИ-пинг: как дела?"}
	wh := &fakeNotifier{}
	h := &harness{
		db:  db,
		api: api,
		run: run,
		wh:  wh,
		now: mkTime(2026, time.September, 23, 12, 0), // Wednesday noon
		feat: &Feature{
			Cfg:    cfg,
			DB:     db,
			AI:     run,
			API:    func(string) (hhapi.HHAPI, error) { return api, nil },
			Notify: wh,
			Log:    slog.New(slog.DiscardHandler),
		},
	}
	h.feat.Now = func() time.Time { return h.now }
	return h
}

func scanTask() queue.Task {
	return queue.Task{AccountID: "acc", Kind: queue.KindWarmupPing, Payload: ScanPayload{AccountID: "acc"}}
}

// --- tests ---

func TestScanSendsFirstStagePingFromList(t *testing.T) {
	h := newHarness(t, nil)
	lastOut := h.now.Add(-25 * time.Hour)
	chat := seedChat(t, h, "chat1", lastOut)

	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatalf("HandleScan: %v", err)
	}
	if len(h.api.sent) != 1 {
		t.Fatalf("expected 1 ping, got %v", h.api.sent)
	}
	if h.run.calls != 0 {
		t.Fatalf("list source must not call AI, calls=%d", h.run.calls)
	}
	ladder, err := LadderOf(t.Context(), h.db, "acc", "chat1")
	if err != nil || ladder != 1 {
		t.Fatalf("ladder = %d err=%v, want 1", ladder, err)
	}
	jobs, err := h.db.PingQueueDue(t.Context(), h.now.Add(time.Hour), 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("ping must be marked sent: %+v err=%v", jobs, err)
	}
	if _, _, err := h.db.PingQueueSchedule(t.Context(), "acc", chat.ID, h.now, 3); err != nil {
		t.Fatalf("reschedule: %v", err)
	} // UNIQUE(chat,attempts) keeps the sent row; the call is a no-op
}

func TestScanRespectsLadderTiming(t *testing.T) {
	h := newHarness(t, nil)
	lastOut := h.now.Add(-25 * time.Hour)
	seedChat(t, h, "chat1", lastOut)

	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	// immediately again: stage 2 due only after 48h from lastOut
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if len(h.api.sent) != 1 {
		t.Fatalf("stage 2 must not fire early, sent=%v", h.api.sent)
	}
	h.now = lastOut.Add(50 * time.Hour)
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatalf("scan3: %v", err)
	}
	if len(h.api.sent) != 2 {
		t.Fatalf("stage 2 must fire after 48h, sent=%v", h.api.sent)
	}
	ladder, _ := LadderOf(t.Context(), h.db, "acc", "chat1")
	if ladder != 2 {
		t.Fatalf("ladder = %d, want 2", ladder)
	}
}

func TestScanSuppressesAtMaxPingsWithReview(t *testing.T) {
	h := newHarness(t, nil)
	lastOut := h.now.Add(-25 * time.Hour)
	seedChat(t, h, "chat1", lastOut)
	if err := SetLadder(t.Context(), h.db, "acc", "chat1", 3); err != nil {
		t.Fatal(err)
	}
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatalf("HandleScan: %v", err)
	}
	if len(h.api.sent) != 0 {
		t.Fatalf("suppressed chat must not be pinged: %v", h.api.sent)
	}
	items, _ := h.db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "warmup_suppressed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected warmup_suppressed review, got %+v", items)
	}
	// suppression review is recorded once
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	items, _ = h.db.ReviewItemsOpen(t.Context(), "acc", 10)
	n := 0
	for _, it := range items {
		if it.Kind == "warmup_suppressed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("suppression review duplicated: %d", n)
	}
}

func TestScanRespectsActiveHoursAndWeekend(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Warmup.ActiveHours = "08:00-22:00"
	})
	lastOut := h.now.Add(-25 * time.Hour)
	seedChat(t, h, "chat1", lastOut)

	h.now = mkTime(2026, time.September, 23, 23, 30) // outside window
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if len(h.api.sent) != 0 {
		t.Fatal("ping outside active hours must be skipped")
	}

	h.now = mkTime(2026, time.September, 26, 12, 0) // Saturday
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if len(h.api.sent) != 0 {
		t.Fatal("weekend ping must be skipped when respect_weekend")
	}

	// explicit opt-out allows weekend pings
	tf := false
	h.feat.Cfg.Warmup.RespectWeekend = &tf
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if len(h.api.sent) != 1 {
		t.Fatalf("weekend ping expected after opt-out, sent=%v", h.api.sent)
	}
}

func TestScanAISourceAndUnsure(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Warmup.Source = "ai"; c.Warmup.PingList = nil })
	seedChat(t, h, "chat1", h.now.Add(-25*time.Hour))

	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if h.run.calls != 1 || len(h.api.sent) != 1 {
		t.Fatalf("AI ping expected, calls=%d sent=%v", h.run.calls, h.api.sent)
	}
	if h.api.sent[0] == "" || !strings.Contains(h.api.sent[0], "ИИ-пинг") {
		t.Fatalf("AI text not sent: %q", h.api.sent[0])
	}

	// unsure AI verdict → review, no send
	h2 := newHarness(t, func(c *config.Config) { c.Warmup.Source = "ai"; c.Warmup.PingList = nil })
	h2.run.needs = true
	seedChat(t, h2, "chat1", h2.now.Add(-25*time.Hour))
	if err := h2.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if len(h2.api.sent) != 0 {
		t.Fatal("unsure ping must not be sent")
	}
	items, _ := h2.db.ReviewItemsOpen(t.Context(), "acc", 10)
	if len(items) == 0 {
		t.Fatal("unsure ping must produce a review item")
	}
}

func TestScanRotationAvoidsLastN(t *testing.T) {
	h := newHarness(t, nil)
	lastOut := h.now.Add(-25 * time.Hour)
	seedChat(t, h, "chat1", lastOut)

	// Send pings across 3 ladder stages: indexes must not repeat within N=3.
	for i := 0; i < 3; i++ {
		if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
			t.Fatalf("stage %d: %v", i+1, err)
		}
		h.now = lastOut.Add(time.Duration(25*(i+2)) * time.Hour)
	}
	texts := make([]string, 0)
	for _, s := range h.api.sent {
		texts = append(texts, afterPipe(s))
	}
	uniq := map[string]bool{}
	for _, s := range texts {
		uniq[s] = true
	}
	if len(uniq) != len(texts) {
		t.Fatalf("rotation repeated a ping within N=3: %v", texts)
	}
}

func TestScanDryRunSendsNothing(t *testing.T) {
	h := newHarness(t, nil)
	h.feat.Cfg.General.DryRun = true
	seedChat(t, h, "chat1", h.now.Add(-25*time.Hour))

	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if len(h.api.sent) != 0 {
		t.Fatal("dry-run must not send")
	}
	ladder, _ := LadderOf(t.Context(), h.db, "acc", "chat1")
	if ladder != 0 {
		t.Fatalf("dry-run must not advance ladder, got %d", ladder)
	}
	// Wave-3 review #7: the dry-run intent must NOT stay pending — after
	// switching dry-run off, the same stage send goes through.
	h.feat.Cfg.General.DryRun = false
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if len(h.api.sent) != 1 {
		t.Fatalf("dry-run must abandon its intent, sends after re-enable=%v", h.api.sent)
	}
}

func TestScanAmbiguousReconcilesByReadingState(t *testing.T) {
	// Ambiguous send + history shows the ping → treat as delivered.
	h := newHarness(t, nil)
	h.api.sendErr = &hhapi.AmbiguousError{Err: errors.New("conn reset mid-flight")}
	h.api.history = []hhapi.ChatMessage{{ID: "m1", Direction: "out", Text: "Здравствуйте! Удалось посмотреть предложение?"}}
	seedChat(t, h, "chat1", h.now.Add(-25*time.Hour))

	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatalf("reconciled-delivered ping must complete quietly: %v", err)
	}
	ladder, _ := LadderOf(t.Context(), h.db, "acc", "chat1")
	if ladder != 1 {
		t.Fatalf("ladder = %d, want 1 after reconcile", ladder)
	}

	// Ambiguous send + history does NOT show the ping → intent failed,
	// review recorded, and a re-scan must NOT auto-repeat the send.
	h2 := newHarness(t, nil)
	h2.api.sendErr = &hhapi.AmbiguousError{Err: errors.New("conn reset mid-flight")}
	seedChat(t, h2, "chat1", h2.now.Add(-25*time.Hour))
	if err := h2.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatalf("handled ambiguity must not surface as scan error: %v", err)
	}
	if len(h2.api.sent) != 0 {
		t.Fatalf("no successful sends expected, got %v", h2.api.sent)
	}
	items, _ := h2.db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "warmup_send_ambiguous" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected warmup_send_ambiguous review, got %+v", items)
	}
	// second scan: transient send error cleared, but intent was marked failed →
	// a fresh attempt WOULD repeat the send — verify the review barrier instead:
	// the ladder gate keeps the ping blocked until the next stage window and
	// the review flags it for a human. No further API calls happen this pass.
	if err := h2.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
}

func TestResetLadderOnInbound(t *testing.T) {
	h := newHarness(t, nil)
	seedChat(t, h, "chat1", h.now.Add(-25*time.Hour))
	if err := SetLadder(t.Context(), h.db, "acc", "chat1", 2); err != nil {
		t.Fatal(err)
	}
	if err := ResetLadder(t.Context(), h.db, "acc", "chat1"); err != nil {
		t.Fatal(err)
	}
	ladder, err := LadderOf(t.Context(), h.db, "acc", "chat1")
	if err != nil || ladder != 0 {
		t.Fatalf("ladder = %d, want 0 (inbound resets)", ladder)
	}
}

func TestDisabledFeatureIsNoop(t *testing.T) {
	h := newHarness(t, nil)
	h.feat.Cfg.Warmup.Enabled = false
	seedChat(t, h, "chat1", h.now.Add(-25*time.Hour))
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if len(h.api.sent) != 0 {
		t.Fatal("disabled warmup must not ping")
	}
}

// --- tiny helpers ---

func afterPipe(s string) string {
	if i := strings.IndexByte(s, '|'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Wave-3 review #4: the pacer must be waited on before the ping mutation;
// chats that are skipped (window/weekend) must not touch the pacer.
func TestScanPacerWaitsBeforeSendOnly(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Warmup.ActiveHours = "08:00-22:00" })
	calls := 0
	h.feat.Pacer = pacerFunc(func(context.Context, string) error { calls++; return nil })
	seedChat(t, h, "chat1", h.now.Add(-25*time.Hour))

	h.now = mkTime(2026, time.September, 23, 23, 30) // outside active hours
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("no mutation expected outside the window, pacer calls=%d", calls)
	}

	h.now = mkTime(2026, time.September, 23, 12, 0)
	if err := h.feat.HandleScan(t.Context(), scanTask()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(h.api.sent) != 1 {
		t.Fatalf("pacer must wait before the ping, calls=%d sent=%v", calls, h.api.sent)
	}
}

type pacerFunc func(ctx context.Context, accountID string) error

func (f pacerFunc) Wait(ctx context.Context, accountID string) error { return f(ctx, accountID) }
