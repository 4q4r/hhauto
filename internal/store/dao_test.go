package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestResumesUpsertAndList(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.AccountsUpsert(ctx, Account{ID: "acc-1"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	r := Resume{AccountID: "acc-1", HhResumeID: "res-1", Title: "Go dev", Status: "published", Raw: "{}", UpdatedAt: truncSec(time.Now())}
	if err := db.ResumesUpsert(ctx, &r); err != nil {
		t.Fatalf("ResumesUpsert: %v", err)
	}
	if r.ID == 0 {
		t.Fatal("ResumesUpsert must assign ID on insert")
	}

	firstID := r.ID
	r.Title = "Senior Go dev"
	if err := db.ResumesUpsert(ctx, &r); err != nil {
		t.Fatalf("ResumesUpsert (conflict): %v", err)
	}
	if r.ID != firstID {
		t.Fatalf("upsert changed ID: %d -> %d", firstID, r.ID)
	}

	got, err := db.ResumesListByAccount(ctx, "acc-1")
	if err != nil {
		t.Fatalf("ResumesListByAccount: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Senior Go dev" {
		t.Fatalf("list = %+v, want single updated resume", got)
	}

	other, err := db.ResumesListByAccount(ctx, "acc-2")
	if err != nil || len(other) != 0 {
		t.Fatalf("list for other account = (%v, %v), want empty", other, err)
	}

	// FK cascade: removing the account removes its resumes.
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM accounts WHERE id = 'acc-1'"); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	got, _ = db.ResumesListByAccount(ctx, "acc-1")
	if len(got) != 0 {
		t.Fatalf("resumes survived account delete (cascade broken): %+v", got)
	}
}

func TestVacanciesSeenUpsertDedup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	vs := VacancySeen{AccountID: "acc-1", VacancyID: 7, Source: "search", FirstSeenAt: truncSec(time.Now()), Meta: "{}"}
	inserted, err := db.VacanciesSeenUpsert(ctx, vs)
	if err != nil {
		t.Fatalf("VacanciesSeenUpsert: %v", err)
	}
	if !inserted {
		t.Fatal("first upsert must report inserted=true")
	}

	inserted, err = db.VacanciesSeenUpsert(ctx, vs)
	if err != nil {
		t.Fatalf("VacanciesSeenUpsert (repeat): %v", err)
	}
	if inserted {
		t.Fatal("duplicate upsert must report inserted=false")
	}

	// Same vacancy, different source: still deduped by (account, vacancy).
	vs.Source = "similar"
	inserted, err = db.VacanciesSeenUpsert(ctx, vs)
	if err != nil {
		t.Fatalf("VacanciesSeenUpsert (other source): %v", err)
	}
	if inserted {
		t.Fatal("same (account,vacancy) must dedup regardless of source")
	}
}

func TestApplicationsLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")

	app := Application{
		AccountID: "acc-1", RunID: "run-1", HhResumeID: "res-1",
		VacancyID: 10, EmployerID: 20, DedupKey: "dk-1", Status: "queued",
		Letter: ptr("hello"), CorrelationID: ptr("corr-1"),
	}
	if err := db.ApplicationsCreate(ctx, &app); err != nil {
		t.Fatalf("ApplicationsCreate: %v", err)
	}
	if app.ID == 0 || app.CreatedAt.IsZero() {
		t.Fatalf("ApplicationsCreate must fill ID/CreatedAt: %+v", app)
	}

	got, err := db.ApplicationsGetByDedupKey(ctx, "dk-1")
	if err != nil {
		t.Fatalf("ApplicationsGetByDedupKey: %v", err)
	}
	if got == nil || got.ID != app.ID || got.Status != "queued" || got.Letter == nil || *got.Letter != "hello" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	missing, err := db.ApplicationsGetByDedupKey(ctx, "nope")
	if err != nil || missing != nil {
		t.Fatalf("GetByDedupKey(missing) = (%v, %v), want (nil, nil)", missing, err)
	}

	n, err := db.ApplicationsCountToday(ctx, "acc-1", "run-1", today)
	if err != nil || n != 0 {
		t.Fatalf("CountToday before applied = (%d, %v), want (0, nil)", n, err)
	}

	if err := db.ApplicationsMarkStatus(ctx, app.ID, "applied", nil); err != nil {
		t.Fatalf("MarkStatus(applied): %v", err)
	}
	n, err = db.ApplicationsCountToday(ctx, "acc-1", "run-1", today)
	if err != nil || n != 1 {
		t.Fatalf("CountToday after applied = (%d, %v), want (1, nil)", n, err)
	}
	applied, _ := db.ApplicationsGetByDedupKey(ctx, "dk-1")
	if applied.AppliedAt == nil {
		t.Fatal("applied_at must be set by MarkStatus(applied)")
	}

	n, err = db.ApplicationsCountToday(ctx, "acc-1", "run-2", today)
	if err != nil || n != 0 {
		t.Fatalf("CountToday other run = (%d, %v), want (0, nil)", n, err)
	}

	if err := db.ApplicationsMarkStatus(ctx, app.ID, "failed", ptr("boom")); err != nil {
		t.Fatalf("MarkStatus(failed): %v", err)
	}
	failed, _ := db.ApplicationsGetByDedupKey(ctx, "dk-1")
	if failed.Status != "failed" || failed.FailReason == nil || *failed.FailReason != "boom" {
		t.Fatalf("failed roundtrip mismatch: %+v", failed)
	}

	// UNIQUE dedup_key must reject a second application with the same key.
	dup := app
	dup.ID = 0
	dup.DedupKey = "dk-1"
	if err := db.ApplicationsCreate(ctx, &dup); err == nil {
		t.Fatal("duplicate dedup_key must fail")
	}
}

func TestChatsUpsertStateAndDueWarmup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := truncSec(time.Now())
	chat := Chat{
		AccountID: "acc-1", ChatikChatID: "ch-1", TopicID: ptr("t-1"),
		VacancyID: ptr(int64(10)), EmployerID: ptr(int64(20)), EmployerName: ptr("ACME"),
		State: "active", RejectionConfidence: ptr(0.5),
		LastMessageAt: &now, LastOutgoingAt: &now,
	}
	if err := db.ChatsUpsert(ctx, &chat); err != nil {
		t.Fatalf("ChatsUpsert: %v", err)
	}
	if chat.ID == 0 {
		t.Fatal("ChatsUpsert must assign ID")
	}

	// Empty state falls back to the schema default on insert.
	def := Chat{AccountID: "acc-1", ChatikChatID: "ch-def"}
	if err := db.ChatsUpsert(ctx, &def); err != nil {
		t.Fatalf("ChatsUpsert (defaults): %v", err)
	}
	gotDef, _ := db.chatsByChatikID(ctx, "acc-1", "ch-def")
	if gotDef == nil || gotDef.State != "active" || gotDef.LeaveCount != 0 {
		t.Fatalf("defaults mismatch: %+v", gotDef)
	}

	firstID := chat.ID
	chat.EmployerName = ptr("ACME LLC")
	chat.LeaveCount = 1
	if err := db.ChatsUpsert(ctx, &chat); err != nil {
		t.Fatalf("ChatsUpsert (update): %v", err)
	}
	if chat.ID != firstID {
		t.Fatalf("upsert changed ID: %d -> %d", firstID, chat.ID)
	}

	// State must not be clobbered by Upsert once set explicitly.
	if err := db.ChatsUpdateState(ctx, "acc-1", "ch-1", "rejected", ptr(0.9)); err != nil {
		t.Fatalf("ChatsUpdateState: %v", err)
	}
	if err := db.ChatsUpsert(ctx, &chat); err != nil {
		t.Fatalf("ChatsUpsert (after state change): %v", err)
	}
	got, err := db.chatsByChatikID(ctx, "acc-1", "ch-1")
	if err != nil {
		t.Fatalf("chatsByChatikID: %v", err)
	}
	if got.State != "rejected" || got.RejectionConfidence == nil || *got.RejectionConfidence != 0.9 {
		t.Fatalf("state/confidence mismatch: %+v", got)
	}
	if got.LeaveCount != 1 || got.EmployerName == nil || *got.EmployerName != "ACME LLC" {
		t.Fatalf("updatable fields mismatch: %+v", got)
	}

	// Warmup queue: only warmup_due chats, ordered by last_outgoing_at.
	older := now.Add(-2 * time.Hour)
	due1 := Chat{AccountID: "acc-1", ChatikChatID: "w-1", State: "warmup_due", LastOutgoingAt: &now}
	due2 := Chat{AccountID: "acc-1", ChatikChatID: "w-2", State: "warmup_due", LastOutgoingAt: &older}
	due3 := Chat{AccountID: "acc-1", ChatikChatID: "w-3", State: "active", LastOutgoingAt: &older}
	for _, c := range []Chat{due1, due2, due3} {
		if err := db.ChatsUpsert(ctx, &c); err != nil {
			t.Fatalf("seed chat %s: %v", c.ChatikChatID, err)
		}
	}
	due, err := db.ChatsDueWarmup(ctx, "acc-1", 10)
	if err != nil {
		t.Fatalf("ChatsDueWarmup: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("DueWarmup = %d chats, want 2", len(due))
	}
	if due[0].ChatikChatID != "w-2" || due[1].ChatikChatID != "w-1" {
		t.Fatalf("DueWarmup order wrong: %s, %s", due[0].ChatikChatID, due[1].ChatikChatID)
	}
	limited, _ := db.ChatsDueWarmup(ctx, "acc-1", 1)
	if len(limited) != 1 {
		t.Fatalf("DueWarmup limit ignored: got %d", len(limited))
	}
}

func TestMessagesAddAndHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	chat := Chat{AccountID: "acc-1", ChatikChatID: "ch-1"}
	if err := db.ChatsUpsert(ctx, &chat); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	base := truncSec(time.Now())
	m1 := Message{ChatID: chat.ID, MessageID: "m-1", Direction: "in", Author: ptr("HR"), Text: "hi", SentAt: base}
	m2 := Message{ChatID: chat.ID, MessageID: "m-2", Direction: "out", Text: "hello", SentAt: base.Add(time.Minute)}
	if err := db.MessagesAdd(ctx, &m1); err != nil {
		t.Fatalf("MessagesAdd: %v", err)
	}
	if err := db.MessagesAdd(ctx, &m2); err != nil {
		t.Fatalf("MessagesAdd: %v", err)
	}
	if m1.ID == 0 || m2.ID == 0 {
		t.Fatal("MessagesAdd must assign IDs")
	}

	hist, err := db.MessagesHistory(ctx, chat.ID, 10)
	if err != nil {
		t.Fatalf("MessagesHistory: %v", err)
	}
	if len(hist) != 2 || hist[0].MessageID != "m-1" || hist[1].MessageID != "m-2" {
		t.Fatalf("history = %+v, want chronological m-1,m-2", hist)
	}
	if hist[1].Author != nil {
		t.Fatalf("nullable author must roundtrip nil: %+v", hist[1])
	}

	limited, _ := db.MessagesHistory(ctx, chat.ID, 1)
	if len(limited) != 1 || limited[0].MessageID != "m-1" {
		t.Fatalf("history limit = %+v, want only m-1", limited)
	}

	dup := m1
	dup.ID = 0
	if err := db.MessagesAdd(ctx, &dup); err == nil {
		t.Fatal("duplicate message_id must fail (UNIQUE)")
	}

	// FK cascade: deleting the chat removes its messages.
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM chats WHERE id = ?", chat.ID); err != nil {
		t.Fatalf("delete chat: %v", err)
	}
	hist, _ = db.MessagesHistory(ctx, chat.ID, 10)
	if len(hist) != 0 {
		t.Fatalf("messages survived chat delete (cascade broken): %+v", hist)
	}
}

func TestContactsUpsertByEmployerExport(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	c := Contact{
		AccountID: "acc-1", EmployerID: 20, Source: "card",
		Name: ptr("Anna"), Role: ptr("HR"),
		Phones: `["+7"]`, Emails: `["a@b.c"]`, Messengers: "[]", Raw: "{}",
		CollectedAt: truncSec(time.Now()),
	}
	if err := db.ContactsUpsert(ctx, &c); err != nil {
		t.Fatalf("ContactsUpsert: %v", err)
	}
	firstID := c.ID

	c.Phones = `["+7","+7900"]`
	if err := db.ContactsUpsert(ctx, &c); err != nil {
		t.Fatalf("ContactsUpsert (conflict): %v", err)
	}
	if c.ID != firstID {
		t.Fatalf("upsert changed ID: %d -> %d", firstID, c.ID)
	}

	byEmployer, err := db.ContactsByEmployer(ctx, "acc-1", 20)
	if err != nil {
		t.Fatalf("ContactsByEmployer: %v", err)
	}
	if len(byEmployer) != 1 || byEmployer[0].ID != firstID || byEmployer[0].Phones != `["+7","+7900"]` {
		t.Fatalf("byEmployer = %+v", byEmployer)
	}

	other, _ := db.ContactsByEmployer(ctx, "acc-1", 21)
	if len(other) != 0 {
		t.Fatalf("ContactsByEmployer(21) = %+v, want empty", other)
	}

	c2 := Contact{AccountID: "acc-1", EmployerID: 21, Source: "chat", CollectedAt: truncSec(time.Now())}
	if err := db.ContactsUpsert(ctx, &c2); err != nil {
		t.Fatalf("ContactsUpsert (second): %v", err)
	}

	exported, err := db.ContactsExport(ctx, "acc-1")
	if err != nil {
		t.Fatalf("ContactsExport: %v", err)
	}
	if len(exported) != 2 || exported[0].EmployerID != 20 || exported[1].EmployerID != 21 {
		t.Fatalf("export = %+v, want ordered by employer_id", exported)
	}

	bad := Contact{AccountID: "acc-1", EmployerID: 22, Source: "web", CollectedAt: truncSec(time.Now())}
	if err := db.ContactsUpsert(ctx, &bad); err == nil {
		t.Fatal("contact source outside CHECK must fail")
	}
}

func TestAssessmentsLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	a := Assessment{
		AccountID: "acc-1", VacancyID: ptr(int64(10)), Kind: "hh_internal",
		Provider: ptr("hh"), ExternalURL: ptr("https://hh.ru/test"),
		Status: "detected", Answers: ptr("{}"),
		CreatedAt: truncSec(time.Now()),
	}
	if err := db.AssessmentsAdd(ctx, &a); err != nil {
		t.Fatalf("AssessmentsAdd: %v", err)
	}
	if a.ID == 0 {
		t.Fatal("AssessmentsAdd must assign ID")
	}

	if err := db.AssessmentsUpdateStatus(ctx, a.ID, "solved", ptr("passed")); err != nil {
		t.Fatalf("AssessmentsUpdateStatus: %v", err)
	}
	list, err := db.AssessmentsListByAccount(ctx, "acc-1")
	if err != nil {
		t.Fatalf("AssessmentsListByAccount: %v", err)
	}
	if len(list) != 1 || list[0].Status != "solved" || list[0].Result == nil || *list[0].Result != "passed" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].FinishedAt == nil {
		t.Fatal("finished_at must be set on terminal update")
	}

	bad := Assessment{AccountID: "acc-1", Kind: "quiz", CreatedAt: truncSec(time.Now())}
	if err := db.AssessmentsAdd(ctx, &bad); err == nil {
		t.Fatal("kind outside CHECK must fail")
	}
}

func TestInterviewLinks(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	l := InterviewLink{
		AccountID: "acc-1", ChatID: ptr(int64(77)), URL: "https://meet.example/1",
		Provider: "zoom", Status: "detected", DetectedAt: truncSec(time.Now()),
	}
	if err := db.InterviewLinksUpsert(ctx, &l); err != nil {
		t.Fatalf("InterviewLinksUpsert: %v", err)
	}
	firstID := l.ID

	l.ChatID = ptr(int64(78))
	if err := db.InterviewLinksUpsert(ctx, &l); err != nil {
		t.Fatalf("InterviewLinksUpsert (conflict): %v", err)
	}
	if l.ID != firstID {
		t.Fatalf("upsert changed ID: %d -> %d", firstID, l.ID)
	}

	if err := db.InterviewLinksUpdateStatus(ctx, firstID, "escalated"); err != nil {
		t.Fatalf("InterviewLinksUpdateStatus: %v", err)
	}

	list, err := db.InterviewLinksListByAccount(ctx, "acc-1")
	if err != nil {
		t.Fatalf("InterviewLinksListByAccount: %v", err)
	}
	if len(list) != 1 || list[0].Status != "escalated" || list[0].ChatID == nil || *list[0].ChatID != 78 {
		t.Fatalf("list = %+v", list)
	}
}

func TestPingQueueScheduleDueMarkSent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	chat := Chat{AccountID: "acc-1", ChatikChatID: "ch-1"}
	if err := db.ChatsUpsert(ctx, &chat); err != nil {
		t.Fatalf("seed chat: %v", err)
	}

	now := time.Now().UTC()
	id, scheduled, err := db.PingQueueSchedule(ctx, "acc-1", chat.ID, now.Add(time.Minute), 2)
	if err != nil {
		t.Fatalf("PingQueueSchedule: %v", err)
	}
	if !scheduled || id == 0 {
		t.Fatalf("first schedule = (%d, %v), want (>0, true)", id, scheduled)
	}

	// UNIQUE(chat_id, attempts): second pending ping for the same chat is a no-op.
	id2, scheduled2, err := db.PingQueueSchedule(ctx, "acc-1", chat.ID, now.Add(time.Minute), 2)
	if err != nil {
		t.Fatalf("PingQueueSchedule (dup): %v", err)
	}
	if scheduled2 || id2 != 0 {
		t.Fatalf("duplicate schedule = (%d, %v), want (0, false)", id2, scheduled2)
	}

	due, err := db.PingQueueDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("PingQueueDue(now): %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due before due_at must be empty, got %+v", due)
	}
	due, err = db.PingQueueDue(ctx, now.Add(2*time.Minute), 10)
	if err != nil {
		t.Fatalf("PingQueueDue(later): %v", err)
	}
	if len(due) != 1 || due[0].ID != id || due[0].MaxAttempts != 2 || due[0].State != "pending" {
		t.Fatalf("due = %+v", due)
	}

	if err := db.PingQueueMarkSent(ctx, id); err != nil {
		t.Fatalf("PingQueueMarkSent: %v", err)
	}
	due, _ = db.PingQueueDue(ctx, now.Add(2*time.Minute), 10)
	if len(due) != 0 {
		t.Fatalf("due after MarkSent must be empty, got %+v", due)
	}
	sent, err := db.pingByID(ctx, id)
	if err != nil {
		t.Fatalf("pingByID: %v", err)
	}
	if sent.State != "sent" || sent.SentAt == nil {
		t.Fatalf("sent ping mismatch: %+v", sent)
	}

	// FK cascade: deleting the chat removes scheduled pings.
	if _, err := db.conn.ExecContext(ctx, "DELETE FROM chats WHERE id = ?", chat.ID); err != nil {
		t.Fatalf("delete chat: %v", err)
	}
	left, err := db.pingByID(ctx, id)
	if err != nil {
		t.Fatalf("pingByID after cascade: %v", err)
	}
	if left != nil {
		t.Fatalf("ping survived chat delete (cascade broken): %+v", left)
	}
}

func TestAIDecisionsAddByEntitySumTokens(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	d := AIDecision{
		AccountID: "acc-1", Task: "letter", EntityType: "vacancy", EntityID: "10",
		RunID: "run-1", Model: "test-model", PromptVer: "v1", InputDigest: "abc",
		Verdict: "ok", Score: ptr(0.8), Confidence: ptr(0.9),
		LatencyMS: 120, TokensIn: 100, TokensOut: 50, CacheHit: true,
	}
	if err := db.AIDecisionsAdd(ctx, &d); err != nil {
		t.Fatalf("AIDecisionsAdd: %v", err)
	}
	if d.ID == "" || len(d.ID) != 26 {
		t.Fatalf("AIDecisionsAdd must assign ULID (26 chars), got %q", d.ID)
	}
	if d.CreatedAt.IsZero() {
		t.Fatal("AIDecisionsAdd must fill CreatedAt")
	}
	if d.Outcome != "pending" {
		t.Fatalf("default outcome = %q, want pending", d.Outcome)
	}

	d2 := d
	d2.ID = ""
	d2.TokensIn = 10
	d2.TokensOut = 20
	if err := db.AIDecisionsAdd(ctx, &d2); err != nil {
		t.Fatalf("AIDecisionsAdd (second): %v", err)
	}

	byEntity, err := db.AIDecisionsByEntity(ctx, "acc-1", "letter", "vacancy", "10", 10)
	if err != nil {
		t.Fatalf("AIDecisionsByEntity: %v", err)
	}
	if len(byEntity) != 2 || byEntity[0].ID != d2.ID {
		t.Fatalf("byEntity = %+v, want 2 with newest first", byEntity)
	}

	today := time.Now().UTC().Format("2006-01-02")
	tin, tout, err := db.AIDecisionsSumTokensDay(ctx, "acc-1", today)
	if err != nil {
		t.Fatalf("AIDecisionsSumTokensDay: %v", err)
	}
	if tin != 110 || tout != 70 {
		t.Fatalf("token sums = (%d, %d), want (110, 70)", tin, tout)
	}
}

func TestSchedulesUpsertDueMarkRun(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	s := Schedule{ID: "daily-apply", AccountID: "acc-1", Spec: "0 9 * * *", Enabled: true, NextRunAt: ptr(now.Add(time.Hour))}
	if err := db.SchedulesUpsert(ctx, &s); err != nil {
		t.Fatalf("SchedulesUpsert: %v", err)
	}

	due, err := db.SchedulesDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("SchedulesDue(now): %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due before next_run_at must be empty, got %+v", due)
	}

	due, err = db.SchedulesDue(ctx, now.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatalf("SchedulesDue(later): %v", err)
	}
	if len(due) != 1 || due[0].ID != "daily-apply" {
		t.Fatalf("due = %+v", due)
	}

	next := now.Add(26 * time.Hour)
	if err := db.SchedulesMarkRun(ctx, "daily-apply", now.Add(2*time.Hour), next); err != nil {
		t.Fatalf("SchedulesMarkRun: %v", err)
	}
	due, _ = db.SchedulesDue(ctx, now.Add(2*time.Hour), 10)
	if len(due) != 0 {
		t.Fatalf("due after MarkRun must be empty, got %+v", due)
	}

	s.Enabled = false
	if err := db.SchedulesUpsert(ctx, &s); err != nil {
		t.Fatalf("SchedulesUpsert (disable): %v", err)
	}
	due, _ = db.SchedulesDue(ctx, now.Add(48*time.Hour), 10)
	if len(due) != 0 {
		t.Fatalf("disabled schedule must never be due, got %+v", due)
	}
}

func TestFeatureStateGetSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	v, ok, err := db.FeatureStateGet(ctx, "warmup", "acc-1", "done")
	if err != nil || ok || v != "" {
		t.Fatalf("missing key = (%q, %v, %v), want (\"\", false, nil)", v, ok, err)
	}

	if err := db.FeatureStateSet(ctx, "warmup", "acc-1", "done", "1"); err != nil {
		t.Fatalf("FeatureStateSet: %v", err)
	}
	v, ok, err = db.FeatureStateGet(ctx, "warmup", "acc-1", "done")
	if err != nil || !ok || v != "1" {
		t.Fatalf("get after set = (%q, %v, %v), want (\"1\", true, nil)", v, ok, err)
	}

	if err := db.FeatureStateSet(ctx, "warmup", "acc-1", "done", "2"); err != nil {
		t.Fatalf("FeatureStateSet (overwrite): %v", err)
	}
	v, ok, _ = db.FeatureStateGet(ctx, "warmup", "acc-1", "done")
	if !ok || v != "2" {
		t.Fatalf("get after overwrite = (%q, %v), want (\"2\", true)", v, ok)
	}
}

func TestReviewItems(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.ReviewItemsAdd(ctx, "acc-1", "letter", "low-score", "{}")
	if err != nil {
		t.Fatalf("ReviewItemsAdd: %v", err)
	}

	open, err := db.ReviewItemsOpen(ctx, "acc-1", 10)
	if err != nil {
		t.Fatalf("ReviewItemsOpen: %v", err)
	}
	if len(open) != 1 || open[0].ID != id || open[0].Status != "open" || open[0].Kind != "letter" {
		t.Fatalf("open = %+v", open)
	}

	if err := db.ReviewItemsResolve(ctx, id, "fixed prompt"); err != nil {
		t.Fatalf("ReviewItemsResolve: %v", err)
	}
	open, _ = db.ReviewItemsOpen(ctx, "acc-1", 10)
	if len(open) != 0 {
		t.Fatalf("open after resolve must be empty, got %+v", open)
	}

	// T54: the by-id lookup is public (the CLI resolve flow checks the
	// item's status before applying) and sees closed items too.
	missing, err := db.ReviewItemsGet(ctx, 404)
	if err != nil {
		t.Fatalf("ReviewItemsGet miss: %v", err)
	}
	if missing != nil {
		t.Fatalf("missing item = %+v, want nil", missing)
	}

	resolved, err := db.ReviewItemsGet(ctx, id)
	if err != nil {
		t.Fatalf("ReviewItemsGet: %v", err)
	}
	if resolved == nil || resolved.Status != "resolved" || resolved.Resolution == nil || *resolved.Resolution != "fixed prompt" || resolved.ResolvedAt == nil {
		t.Fatalf("resolved item mismatch: %+v", resolved)
	}
}

func TestEmployerRatingsGetPut(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	got, err := db.EmployerRatingsGet(ctx, 20)
	if err != nil || got != nil {
		t.Fatalf("missing rating = (%v, %v), want (nil, nil)", got, err)
	}

	r := EmployerRating{EmployerID: 20, ScoreAll: 4.5, ScoreRecent: 4.7, PeriodDays: 365, ReviewsCount: 100, RecommendPct: 88, FetchedAt: truncSec(time.Now())}
	if err := db.EmployerRatingsPut(ctx, r); err != nil {
		t.Fatalf("EmployerRatingsPut: %v", err)
	}
	got, err = db.EmployerRatingsGet(ctx, 20)
	if err != nil {
		t.Fatalf("EmployerRatingsGet: %v", err)
	}
	if got == nil || got.ScoreAll != 4.5 || got.ScoreRecent != 4.7 || got.ReviewsCount != 100 || got.RecommendPct != 88 {
		t.Fatalf("rating roundtrip mismatch: %+v", got)
	}

	r.ScoreAll = 4.1
	if err := db.EmployerRatingsPut(ctx, r); err != nil {
		t.Fatalf("EmployerRatingsPut (overwrite): %v", err)
	}
	got, _ = db.EmployerRatingsGet(ctx, 20)
	if got.ScoreAll != 4.1 {
		t.Fatalf("overwrite failed: %+v", got)
	}
}

func TestMetricsIncrAndSnapshot(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")

	for i := 0; i < 3; i++ {
		if err := db.MetricsIncr(ctx, "applied", today); err != nil {
			t.Fatalf("MetricsIncr: %v", err)
		}
	}
	if err := db.MetricsIncr(ctx, "skipped", today); err != nil {
		t.Fatalf("MetricsIncr: %v", err)
	}

	snap, err := db.MetricsSnapshot(ctx, today)
	if err != nil {
		t.Fatalf("MetricsSnapshot: %v", err)
	}
	if snap["applied"] != 3 || snap["skipped"] != 1 || len(snap) != 2 {
		t.Fatalf("snapshot = %+v, want applied=3 skipped=1", snap)
	}

	empty, err := db.MetricsSnapshot(ctx, "1999-01-01")
	if err != nil || len(empty) != 0 {
		t.Fatalf("snapshot for empty day = (%v, %v), want empty", empty, err)
	}
}

func TestAIDecisionsCostRoundtrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	d := AIDecision{
		AccountID: "acc-1", Task: "letter", EntityType: "vacancy", EntityID: "10",
		TokensIn: 100, TokensOut: 50, CostUSD: 0.25,
	}
	if err := db.AIDecisionsAdd(ctx, &d); err != nil {
		t.Fatalf("AIDecisionsAdd: %v", err)
	}
	got, err := db.AIDecisionsByEntity(ctx, "acc-1", "letter", "vacancy", "10", 10)
	if err != nil {
		t.Fatalf("AIDecisionsByEntity: %v", err)
	}
	if len(got) != 1 || got[0].CostUSD != 0.25 {
		t.Fatalf("cost roundtrip = %+v, want CostUSD 0.25", got)
	}
}

func TestAIDecisionsSumTokensDayAll(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")

	for _, d := range []AIDecision{
		{AccountID: "acc-1", Task: "letter", TokensIn: 100, TokensOut: 50, CostUSD: 0.25},
		{AccountID: "acc-2", Task: "letter", TokensIn: 10, TokensOut: 20, CostUSD: 0.5},
	} {
		row := d
		if err := db.AIDecisionsAdd(ctx, &row); err != nil {
			t.Fatalf("AIDecisionsAdd (%s): %v", d.AccountID, err)
		}
	}

	tokens, cost, err := db.AIDecisionsSumTokensDayAll(ctx, today)
	if err != nil {
		t.Fatalf("AIDecisionsSumTokensDayAll: %v", err)
	}
	if tokens != 180 {
		t.Fatalf("tokens = %d, want 180 (in+out across accounts)", tokens)
	}
	if cost != 0.75 {
		t.Fatalf("cost = %v, want 0.75", cost)
	}

	tokens, cost, err = db.AIDecisionsSumTokensDayAll(ctx, "1999-01-01")
	if err != nil || tokens != 0 || cost != 0 {
		t.Fatalf("empty day = (%d, %v, %v), want (0, 0, nil)", tokens, cost, err)
	}
}

func TestIntentsPendingConflictAndLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// pending -> ErrIntentPending.
	id1, err := db.IntentsBegin(ctx, "apply:10", "pending", `{"vacancy":10}`)
	if err != nil {
		t.Fatalf("IntentsBegin: %v", err)
	}
	if id1 == "" || len(id1) != 26 {
		t.Fatalf("IntentsBegin must assign ULID, got %q", id1)
	}
	_, err = db.IntentsBegin(ctx, "apply:10", "pending", `{}`)
	if !errors.Is(err, ErrIntentPending) {
		t.Fatalf("second Begin on pending key: err = %v, want ErrIntentPending", err)
	}

	// done -> ErrIntentDone: a completed intent is a hard barrier against
	// re-running the side effect; the store must not silently re-arm it
	// (retry after done is a feature-level RECONCILE decision, not store's).
	if err := db.IntentsMarkDone(ctx, id1); err != nil {
		t.Fatalf("IntentsMarkDone: %v", err)
	}
	if _, err := db.IntentsBegin(ctx, "apply:10", "pending", `{}`); !errors.Is(err, ErrIntentDone) {
		t.Fatalf("Begin on done key: err = %v, want ErrIntentDone", err)
	}
	var state string
	if err := db.conn.QueryRow("SELECT state FROM intents WHERE key = 'apply:10'").Scan(&state); err != nil {
		t.Fatalf("read intent after ErrIntentDone: %v", err)
	}
	if state != "done" {
		t.Fatalf("done intent was clobbered: state = %q, want done", state)
	}

	// failed -> replaced in place with a fresh intent.
	id2, err := db.IntentsBegin(ctx, "apply:11", "pending", `{}`)
	if err != nil {
		t.Fatalf("IntentsBegin (apply:11): %v", err)
	}
	if err := db.IntentsMarkFailed(ctx, id2); err != nil {
		t.Fatalf("IntentsMarkFailed: %v", err)
	}
	id3, err := db.IntentsBegin(ctx, "apply:11", "pending", `{}`)
	if err != nil {
		t.Fatalf("Begin after failed: %v", err)
	}
	if id3 == id2 {
		t.Fatal("Begin after failed must create a new intent")
	}

	// abandoned -> replaced in place with a fresh intent.
	if err := db.IntentsMarkAbandoned(ctx, id3); err != nil {
		t.Fatalf("IntentsMarkAbandoned: %v", err)
	}
	id4, err := db.IntentsBegin(ctx, "apply:11", "pending", `{}`)
	if err != nil {
		t.Fatalf("Begin after abandoned: %v", err)
	}
	if id4 == id3 {
		t.Fatal("Begin after abandoned must create a new intent")
	}

	// State CHECK: unknown states must fail.
	if _, err := db.IntentsBegin(ctx, "apply:12", "running", `{}`); err == nil {
		t.Fatal("intent state outside CHECK must fail")
	}
}

func TestCaptchaEventsAdd(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	e := CaptchaEvent{AccountID: "acc-1", ProviderID: "2captcha", Kind: "recaptcha_v2", Ok: true, LatencyMS: 1500, CreatedAt: truncSec(time.Now())}
	if err := db.CaptchaEventsAdd(ctx, &e); err != nil {
		t.Fatalf("CaptchaEventsAdd: %v", err)
	}
	if e.ID == 0 {
		t.Fatal("CaptchaEventsAdd must assign ID")
	}

	var ok int
	var latency int
	if err := db.conn.QueryRow("SELECT ok, latency_ms FROM captcha_events WHERE id = ?", e.ID).Scan(&ok, &latency); err != nil {
		t.Fatalf("read captcha event: %v", err)
	}
	if ok != 1 || latency != 1500 {
		t.Fatalf("captcha roundtrip = (ok=%d, latency=%d), want (1, 1500)", ok, latency)
	}
}

func TestWebhookOutboxDueMarkDelivered(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	item := OutboxItem{EndpointID: "telegram", Payload: `{"text":"hi"}`, NextRetryAt: &past}
	if err := db.WebhookOutboxAdd(ctx, &item); err != nil {
		t.Fatalf("WebhookOutboxAdd: %v", err)
	}
	if item.ID == 0 || item.Attempts != 0 || item.Delivered {
		t.Fatalf("add must default attempts=0 delivered=false: %+v", item)
	}

	future := OutboxItem{EndpointID: "discord", Payload: "{}", NextRetryAt: ptr(now.Add(time.Hour))}
	if err := db.WebhookOutboxAdd(ctx, &future); err != nil {
		t.Fatalf("WebhookOutboxAdd (future): %v", err)
	}

	due, err := db.WebhookOutboxDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("WebhookOutboxDue: %v", err)
	}
	if len(due) != 1 || due[0].ID != item.ID {
		t.Fatalf("due = %+v, want only overdue item", due)
	}

	if err := db.WebhookOutboxMarkDelivered(ctx, item.ID); err != nil {
		t.Fatalf("WebhookOutboxMarkDelivered: %v", err)
	}
	due, _ = db.WebhookOutboxDue(ctx, now, 10)
	if len(due) != 0 {
		t.Fatalf("due after delivery must be empty, got %+v", due)
	}
	// The future item becomes due only after its next_retry_at passes.
	due, _ = db.WebhookOutboxDue(ctx, now.Add(2*time.Hour), 10)
	if len(due) != 1 || due[0].ID != future.ID {
		t.Fatalf("future item must be due after its retry time, got %+v", due)
	}
}

func TestProfileFactsSetAll(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.ProfileFactsSet(ctx, "acc-1", "grade", "middle"); err != nil {
		t.Fatalf("ProfileFactsSet: %v", err)
	}
	if err := db.ProfileFactsSet(ctx, "acc-1", "city", "Moscow"); err != nil {
		t.Fatalf("ProfileFactsSet: %v", err)
	}
	if err := db.ProfileFactsSet(ctx, "acc-1", "grade", "senior"); err != nil {
		t.Fatalf("ProfileFactsSet (overwrite): %v", err)
	}

	all, err := db.ProfileFactsAll(ctx, "acc-1")
	if err != nil {
		t.Fatalf("ProfileFactsAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all = %d facts, want 2", len(all))
	}
	if all[0].Question != "city" || all[0].Answer != "Moscow" {
		t.Fatalf("first fact = %+v, want city=Moscow (ordered by question)", all[0])
	}
	if all[1].Question != "grade" || all[1].Answer != "senior" {
		t.Fatalf("second fact = %+v, want grade=senior", all[1])
	}
}
