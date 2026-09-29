package setka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/setka"
	"hhauto/internal/store"
)

// --- fakes ---

type fakeClient struct {
	people     []setka.Person
	canMessage map[string]bool
	sent       []string // personIDs passed to SendDM
	texts      map[string]string
	err        error
	sendErr    error // SendDM-only failure (T64)
}

func (c *fakeClient) SearchPeople(_ context.Context, _ string) ([]setka.Person, error) {
	if c.err != nil {
		return nil, c.err
	}
	return c.people, nil
}

func (c *fakeClient) GetPerson(_ context.Context, id string) (*setka.Person, error) {
	for i := range c.people {
		if c.people[i].ID == id {
			p := c.people[i]
			if p.ProfileText == "" {
				p.ProfileText = "профиль " + id
			}
			return &p, nil
		}
	}
	return nil, errors.New("нет такого человека")
}

func (c *fakeClient) CanMessage(_ context.Context, id string) (bool, error) {
	if v, ok := c.canMessage[id]; ok {
		return v, nil
	}
	return true, nil
}

func (c *fakeClient) SendDM(_ context.Context, id, text string) error {
	if c.sendErr != nil {
		return c.sendErr
	}
	if c.err != nil {
		return c.err
	}
	c.sent = append(c.sent, id)
	if c.texts == nil {
		c.texts = map[string]string{}
	}
	c.texts[id] = text
	return nil
}

type fakeAI struct {
	verdicts map[ai.TaskName]string
	err      error
	calls    []ai.TaskName
	inputs   []string
}

func (r *fakeAI) Run(_ context.Context, _, _, _, _ string, task ai.TaskName, input any) (json.RawMessage, ai.Decision, error) {
	r.calls = append(r.calls, task)
	b, _ := json.Marshal(input)
	r.inputs = append(r.inputs, string(b))
	if r.err != nil {
		return nil, ai.Decision{}, r.err
	}
	return json.RawMessage(r.verdicts[task]), ai.Decision{}, nil
}

// --- harness ---

var testNow = time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC) // внутри окна 12:00-18:00

func baseCfg() config.Config {
	c := config.Config{}
	c.General.Timezone = "UTC"
	c.Features.Setka = config.Setka{
		Enabled:         true,
		Mode:            config.SetkaModeAuto,
		AfterApplyDelay: "30m",
		ActiveWindow:    "12:00-18:00",
		MaxDMPerDay:     5,
		MaxDMPerCompany: 2,
		MaxDMPerPerson:  1,
		Classify:        config.SetkaClassify{ModelProfile: "fast", Borderline: config.SetkaBorderlineApply},
		Personalize:     config.SetkaPersonalize{ModelProfile: "fast"},
	}
	c.Features.Setka.Targets.StatusFilter = []string{"нанимаю"} // the applyDefaults default
	return c
}

func classifyVerdict(decision string, conf float64) string {
	b, _ := json.Marshal(map[string]any{
		"decision": decision, "reason_category": "hiring_role",
		"confidence": conf, "needs_human": false, "needs_human_reason": nil,
		"injection_suspected": false,
	})
	return string(b)
}

func personalizeVerdict(msg string) string {
	b, _ := json.Marshal(map[string]any{
		"message": msg, "used_facts": []any{},
		"confidence": 0.9, "needs_human": false, "needs_human_reason": nil,
		"injection_suspected": false,
	})
	return string(b)
}

func threePeople() []setka.Person {
	return []setka.Person{
		{ID: "hr-1", Name: "Мария", Headline: "HR @ Acme", Company: "Acme", Status: "нанимаю"},
		{ID: "eng-1", Name: "Пётр", Headline: "Senior Go @ Acme", Company: "Acme", Status: "нетворкаю"},
		{ID: "hr-2", Name: "Ольга", Headline: "рекрутер Acme", Company: "Acme", Status: "нанимаю"},
	}
}

type harness struct {
	cfg    config.Config
	db     *store.DB
	ai     *fakeAI
	client *fakeClient
	q      queue.Queue
	review *captureReviewer
	reg    *scheduler.Registry
	f      *Feature
}

type captureReviewer struct{ items []store.ReviewItem }

func (r *captureReviewer) Add(_ context.Context, accountID, kind, reason, payload string) (int64, error) {
	r.items = append(r.items, store.ReviewItem{ID: int64(len(r.items) + 1), AccountID: accountID, Kind: kind, Reason: reason, Payload: payload})
	return int64(len(r.items)), nil
}

// newHarness builds an enabled feature with the DB and queue; seedVacancy
// seeds one applied application (vacancy 7, employer 42) that is due.
func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	h := &harness{
		cfg:    baseCfg(),
		db:     openDB(t),
		ai:     &fakeAI{verdicts: map[ai.TaskName]string{ai.TaskSetkaClassify: classifyVerdict("apply", 0.9), ai.TaskSetkaPersonalize: personalizeVerdict("Здравствуйте! Откликнулся на вашу вакансию — готов рассказать подробнее.")}},
		client: &fakeClient{people: threePeople()},
		q:      queue.New(),
		review: &captureReviewer{},
		reg:    scheduler.NewRegistry(),
	}
	if mutate != nil {
		mutate(&h.cfg)
	}
	h.f = &Feature{
		Cfg: h.cfg, DB: h.db, AI: h.ai,
		ClientFor: func(context.Context, string) (setka.SetkaClient, error) { return h.client, nil },
		VacancyFor: func(_ context.Context, _ string, vacancyID int64) (*hhapi.Vacancy, error) {
			return &hhapi.Vacancy{
				ID: vacancyID, Name: "Middle DevOps-инженер", EmployerID: 42,
				EmployerName: "Acme", Description: "Kubernetes, CI/CD, Docker. Удалённо.",
			}, nil
		},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return testNow },
		Review: h.review,
	}
	if err := h.f.RegisterTasks(context.Background(), h.q, h.reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	return h
}

// seedApplied inserts an applied application due for processing
// (vacancy 7 / employer 42, applied 2h ago).
func (h *harness) seedApplied(t *testing.T, dedup string) *store.Application {
	t.Helper()
	return h.seedAppliedAt(t, dedup, 7, testNow.Add(-2*time.Hour))
}

// seedAppliedAt is the explicit variant (custom vacancy id / applied time).
func (h *harness) seedAppliedAt(t *testing.T, dedup string, vacancyID int64, appliedAt time.Time) *store.Application {
	t.Helper()
	app := &store.Application{
		AccountID: "acc1", RunID: "r1", HhResumeID: "hash1",
		VacancyID: vacancyID, EmployerID: 42, DedupKey: dedup, Status: "applied", AppliedAt: &appliedAt,
	}
	if err := h.db.ApplicationsCreate(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	return app
}

// runScan executes the scan and returns the enqueued setka tasks.
func (h *harness) runScan(t *testing.T, accountID string) []queue.Task {
	t.Helper()
	if err := h.f.HandleScan(context.Background(), queue.Task{Payload: ScanPayload{AccountID: accountID}}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var out []queue.Task
	for h.q.Len(accountID) > 0 {
		task, ok := h.q.Dequeue(context.Background(), accountID)
		if !ok {
			break
		}
		out = append(out, task)
	}
	return out
}

// runDM enqueues the seeded application, runs the scan and processes the
// setka task.
func (h *harness) runDM(t *testing.T) {
	t.Helper()
	tasks := h.runScan(t, "acc1")
	if len(tasks) != 1 {
		t.Fatalf("enqueued %d tasks, want 1", len(tasks))
	}
	if err := h.f.HandleDM(context.Background(), tasks[0]); err != nil {
		t.Fatalf("dm: %v", err)
	}
}

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir()+"/test.db", store.Options{BusyTimeoutMS: 500})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- tests ---

// Scan: a due application enqueues exactly one setka job with the right
// payload; a second scan does not re-enqueue (queue dedup) and a processed
// application (done marker) never comes back.
func TestScanEnqueuesDueApplicationsOnce(t *testing.T) {
	h := newHarness(t, nil)
	h.seedApplied(t, "acc1:r1:hash1:7")

	tasks := h.runScan(t, "acc1")
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].Kind != queue.KindSetkaDM {
		t.Fatalf("kind = %s", tasks[0].Kind)
	}
	p, ok := tasks[0].Payload.(DMPayload)
	if !ok {
		t.Fatalf("payload type %T", tasks[0].Payload)
	}
	if p.VacancyID != 7 || p.EmployerID != 42 || p.DedupKey != "acc1:r1:hash1:7" {
		t.Fatalf("payload = %+v", p)
	}

	// Process it: the done marker must keep it out of the next scan.
	if err := h.f.HandleDM(context.Background(), tasks[0]); err != nil {
		t.Fatalf("dm: %v", err)
	}
	h.seedAppliedAt(t, "acc1:r1:hash1:9", 9, testNow.Add(-2*time.Hour))
	tasks = h.runScan(t, "acc1")
	if len(tasks) != 1 || tasks[0].Payload.(DMPayload).VacancyID != 9 {
		t.Fatalf("second scan tasks = %+v, want only vacancy 9", tasks)
	}
}

// Scan: a too-fresh application (delay not elapsed) is not enqueued.
func TestScanRespectsAfterApplyDelay(t *testing.T) {
	h := newHarness(t, nil)
	h.seedAppliedAt(t, "acc1:r1:fresh", 7, testNow.Add(-5*time.Minute))
	tasks := h.runScan(t, "acc1")
	if len(tasks) != 0 {
		t.Fatalf("fresh application must not be due, got %d tasks", len(tasks))
	}
}

// Limits: max_dm_per_company selects the top-N classified candidates;
// max_dm_per_day stops the flow; max_dm_per_person blocks a second DM.
func TestLimitsDayCompanyPerson(t *testing.T) {
	// company cap 1 → only the best candidate is messaged.
	h := newHarness(t, func(c *config.Config) { c.Features.Setka.MaxDMPerCompany = 1 })
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	if len(h.client.sent) != 1 || h.client.sent[0] != "hr-1" {
		t.Fatalf("sent = %v, want [hr-1] (highest confidence first)", h.client.sent)
	}

	// day cap 1: the first application sends exactly one DM (hr-1), the
	// second application of the same day sends nothing new.
	h2 := newHarness(t, func(c *config.Config) { c.Features.Setka.MaxDMPerDay = 1 })
	h2.seedApplied(t, "acc1:r1:7")
	h2.runDM(t)
	if len(h2.client.sent) != 1 {
		t.Fatalf("day cap 1 must allow exactly one send: %v", h2.client.sent)
	}
	h2.seedAppliedAt(t, "acc1:r1:9", 9, testNow.Add(-2*time.Hour))
	h2.runDM(t)
	if len(h2.client.sent) != 1 {
		t.Fatalf("day cap must stop the second application: %v", h2.client.sent)
	}

	// person cap: a sent row today blocks another DM to the same person.
	h3 := newHarness(t, func(c *config.Config) { c.Features.Setka.MaxDMPerCompany = 2 })
	h3.seedApplied(t, "acc1:r1:7")
	day := testNow.UTC().Format("2006-01-02")
	id, err := h3.db.SetkaDMCreate(context.Background(), &store.SetkaDM{
		AccountID: "acc1", ApplicationDedupKey: "x", VacancyID: 5, EmployerID: 42,
		PersonID: "hr-1", Mode: "auto", Status: "queued_review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h3.db.SetkaDMMarkSent(ctxOf(), id); err != nil {
		t.Fatal(err)
	}
	_ = day
	h3.runDM(t)
	for _, sent := range h3.client.sent {
		if sent == "hr-1" {
			t.Fatalf("hr-1 already at person cap, sent = %v", h3.client.sent)
		}
	}
}

// Active window: outside the window nothing is sent and the application is
// left for the next scan.
func TestActiveWindowGatesSending(t *testing.T) {
	h := newHarness(t, nil)
	h.seedApplied(t, "acc1:r1:7")
	h.f.Now = func() time.Time { return testNow.Add(6 * time.Hour) } // 19:00 UTC — вне 12:00-18:00
	tasks := h.runScan(t, "acc1")
	if len(tasks) != 1 {
		t.Fatal("task must be enqueued")
	}
	if err := h.f.HandleDM(context.Background(), tasks[0]); err != nil {
		t.Fatalf("dm outside window: %v", err)
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("nothing may be sent outside the window: %v", h.client.sent)
	}
	// And the application comes back on the next scan (no done marker).
	if got := h.markerSet("acc1:r1:7"); got {
		t.Fatal("done marker must not be set outside the window")
	}
}

// Target filters: status_filter, headline_include/exclude.
func TestTargetFilters(t *testing.T) {
	// status filter (default "нанимаю") drops eng-1.
	h := newHarness(t, nil)
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	for _, s := range h.client.sent {
		if s == "eng-1" {
			t.Fatalf("eng-1 has status нетворкаю, must be filtered: %v", h.client.sent)
		}
	}

	// headline_include requires a substring (no HR here → nobody).
	h2 := newHarness(t, func(c *config.Config) {
		c.Features.Setka.Targets.HeadlineInclude = []string{"техлид"}
	})
	h2.seedApplied(t, "acc1:r1:7")
	h2.runDM(t)
	if len(h2.client.sent) != 0 {
		t.Fatalf("headline_include must drop everyone: %v", h2.client.sent)
	}

	// headline_exclude drops matching headlines.
	h3 := newHarness(t, func(c *config.Config) {
		c.Features.Setka.Targets.HeadlineExclude = []string{"рекрутер"}
	})
	h3.seedApplied(t, "acc1:r1:7")
	h3.runDM(t)
	for _, s := range h3.client.sent {
		if s == "hr-2" {
			t.Fatalf("hr-2 must be excluded by headline: %v", h3.client.sent)
		}
	}
}

// no_links: a link in the AI text is a validation error — nothing is sent,
// the ledger row is skipped, the human sees a review item.
func TestNoLinksValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.ai.verdicts[ai.TaskSetkaPersonalize] = personalizeVerdict("Смотрите моё резюме: https://example.com/cv")
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	if len(h.client.sent) != 0 {
		t.Fatalf("linked text must not be sent: %v", h.client.sent)
	}
	if len(h.review.items) == 0 {
		t.Fatal("review item expected for the linked draft")
	}
	for _, it := range h.review.items {
		if it.Kind == "setka_dm_links" && strings.Contains(it.Payload, "https://example.com/cv") {
			return
		}
	}
	t.Fatalf("no setka_dm_links review with the text: %+v", h.review.items)
}

// Borderline policy routes unsure verdicts.
func TestBorderlinePolicy(t *testing.T) {
	// borderline → apply (default): the borderline person is messaged.
	h := newHarness(t, func(c *config.Config) {
		c.Features.Setka.MaxDMPerCompany = 1
	})
	h.ai.verdicts[ai.TaskSetkaClassify] = classifyVerdict("borderline", 0.5)
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	if len(h.client.sent) == 0 {
		t.Fatal("borderline=apply must send")
	}

	// borderline → skip: nothing is sent.
	h2 := newHarness(t, func(c *config.Config) {
		c.Features.Setka.Classify.Borderline = config.SetkaBorderlineSkip
	})
	h2.ai.verdicts[ai.TaskSetkaClassify] = classifyVerdict("borderline", 0.5)
	h2.seedApplied(t, "acc1:r1:7")
	h2.runDM(t)
	if len(h2.client.sent) != 0 {
		t.Fatalf("borderline=skip must not send: %v", h2.client.sent)
	}
}

// Skip verdicts and classify needs_human park for review without sending.
func TestClassifySkipAndNeedsHuman(t *testing.T) {
	h := newHarness(t, nil)
	h.ai.verdicts[ai.TaskSetkaClassify] = classifyVerdict("skip", 0.9)
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	if len(h.client.sent) != 0 {
		t.Fatalf("skip must not send: %v", h.client.sent)
	}

	h2 := newHarness(t, nil)
	h2.ai.verdicts[ai.TaskSetkaClassify] = classifyVerdict("apply", 0.9)
	// needs_human verdict
	h2.ai.verdicts[ai.TaskSetkaClassify] = `{"decision":"apply","reason_category":"other","confidence":0.9,"needs_human":true,"needs_human_reason":"непонятный профиль","injection_suspected":false}`
	h2.seedApplied(t, "acc1:r1:7")
	h2.runDM(t)
	if len(h2.client.sent) != 0 {
		t.Fatalf("needs_human must not auto-send: %v", h2.client.sent)
	}
	if len(h2.review.items) == 0 {
		t.Fatal("needs_human classify must park a review item")
	}
}

// Approve mode: no direct send; the DM lands in the review queue and the
// ledger row is queued_review; review resolve (send.go) sends it.
func TestApproveModeParksReviewAndResolveSends(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Features.Setka.Mode = config.SetkaModeApprove })
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	if len(h.client.sent) != 0 {
		t.Fatalf("approve mode must not send: %v", h.client.sent)
	}
	var dmItem *store.ReviewItem
	for i := range h.review.items {
		if h.review.items[i].Kind == "setka_dm" && dmItem == nil {
			dmItem = &h.review.items[i]
		}
	}
	if dmItem == nil {
		t.Fatalf("setka_dm review item expected, items: %+v", h.review.items)
	}
	p := ParseDMReviewPayload(dmItem.Payload)
	if p == nil || p.PersonID != "hr-1" || p.Text == "" || p.DMID == 0 {
		t.Fatalf("payload = %+v", p)
	}
	// The ledger row is bound to the review item.
	row, err := h.db.SetkaDMGet(context.Background(), p.DMID)
	if err != nil || row == nil || row.Status != "queued_review" {
		t.Fatalf("ledger row = %+v, %v", row, err)
	}

	// Resolve apply → send.
	if _, err := SendApproved(context.Background(), h.client, h.db, false, p); err != nil {
		t.Fatalf("send approved: %v", err)
	}
	if len(h.client.sent) != 1 {
		t.Fatalf("resolve must send once: %v", h.client.sent)
	}
	row, _ = h.db.SetkaDMGet(context.Background(), p.DMID)
	if row.Status != "sent" {
		t.Fatalf("row status = %s, want sent", row.Status)
	}

	// Resolve discard on a second DM → skipped, nothing sent.
	h2 := newHarness(t, func(c *config.Config) { c.Features.Setka.Mode = config.SetkaModeApprove })
	h2.seedApplied(t, "acc1:r1:7")
	h2.runDM(t)
	var dm2 *store.ReviewItem
	for i := range h2.review.items {
		if h2.review.items[i].Kind == "setka_dm" && dm2 == nil {
			dm2 = &h2.review.items[i]
		}
	}
	p2 := ParseDMReviewPayload(dm2.Payload)
	if err := DiscardApproved(context.Background(), h2.db, p2); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if len(h2.client.sent) != 0 {
		t.Fatalf("discard must not send: %v", h2.client.sent)
	}
	row2, _ := h2.db.SetkaDMGet(context.Background(), p2.DMID)
	if row2.Status != "skipped" {
		t.Fatalf("discard row status = %s, want skipped", row2.Status)
	}
}

// Idempotency: a processed application never reprocesses; an already
// messaged person is skipped on a second vacancy of the same company run.
func TestIdempotency(t *testing.T) {
	h := newHarness(t, nil)
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	first := len(h.client.sent)
	if first == 0 {
		t.Fatal("expected sends")
	}
	// The done marker blocks reprocessing.
	tasks := h.runScan(t, "acc1")
	if len(tasks) != 0 {
		t.Fatalf("done application must not re-enqueue: %+v", tasks)
	}

	// A person already messaged for this vacancy is not messaged twice
	// (direct pipeline re-entry without the marker).
	h2 := newHarness(t, nil)
	h2.seedApplied(t, "acc1:r1:7")
	id, err := h2.db.SetkaDMCreate(context.Background(), &store.SetkaDM{
		AccountID: "acc1", ApplicationDedupKey: "prev", VacancyID: 7, EmployerID: 42,
		PersonID: "hr-1", Mode: "auto", Status: "queued_review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h2.db.SetkaDMMarkSent(ctxOf(), id); err != nil {
		t.Fatal(err)
	}
	h2.runDM(t)
	for _, s := range h2.client.sent {
		if s == "hr-1" {
			t.Fatalf("hr-1 already messaged for vacancy 7: %v", h2.client.sent)
		}
	}
}

// Blacklists: person ids and company substrings drop candidates before any
// AI call.
func TestBlacklists(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.Setka.BlacklistPersons = []string{"hr-1"}
		c.Features.Setka.BlacklistCompanies = []string{"othercorp"}
	})
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	for _, s := range h.client.sent {
		if s == "hr-1" {
			t.Fatalf("hr-1 blacklisted: %v", h.client.sent)
		}
	}
	if len(h.client.sent) == 0 {
		t.Fatal("non-blacklisted candidates must still flow")
	}

	// The whole company blacklisted → no candidates at all.
	h2 := newHarness(t, func(c *config.Config) {
		c.Features.Setka.BlacklistCompanies = []string{"acme"}
	})
	h2.seedApplied(t, "acc1:r1:7")
	h2.runDM(t)
	if len(h2.client.sent) != 0 {
		t.Fatalf("company blacklist must stop everyone: %v", h2.client.sent)
	}
}

// require_can_message: a Setka ghost (not registered) is skipped.
func TestRequireCanMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.client.canMessage = map[string]bool{"hr-1": false}
	h.seedApplied(t, "acc1:r1:7")
	h.runDM(t)
	for _, s := range h.client.sent {
		if s == "hr-1" {
			t.Fatalf("ghost must be skipped: %v", h.client.sent)
		}
	}

	// require_can_message=false lets it through.
	h2 := newHarness(t, func(c *config.Config) {
		b := false
		c.Features.Setka.RequireCanMessage = &b
	})
	h2.client.canMessage = map[string]bool{"hr-1": false}
	h2.seedApplied(t, "acc1:r1:7")
	h2.runDM(t)
	found := false
	for _, s := range h2.client.sent {
		if s == "hr-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("require_can_message=false must pass hr-1: %v", h2.client.sent)
	}
}

// T64: a SendDM refusal with ACCOUNT_NOT_ACTIVATED ("призрак" из hh,
// t63-setka-recon.md §3.2) is a PERMANENT skip: the ledger row is skipped
// with the stable reason (the idempotency check keeps every later scan
// away), the metric counts it, nothing parks in review, and a repeated
// scan never retries this person. Flow errors out loud on other failures.
func TestSendAccountNotActivatedPermanentSkip(t *testing.T) {
	h := newHarness(t, nil)
	h.client.sendErr = fmt.Errorf("setka: send: %w", setka.ErrAccountNotActivated)
	h.seedApplied(t, "acc1:r1:7")
	if err := h.f.HandleDM(context.Background(), h.runScan(t, "acc1")[0]); err != nil {
		t.Fatalf("account_not_activated skip must not fail the flow: %v", err)
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("nothing may be sent: %v", h.client.sent)
	}
	day := testNow.UTC().Format("2006-01-02")
	snap, err := h.db.MetricsSnapshot(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if snap["setka.dm_not_activated"] != 2 { // hr-1 и hr-2 прошли статус-фильтр
		t.Fatalf("metric setka.dm_not_activated = %d, want 2 (snapshot %+v)", snap["setka.dm_not_activated"], snap)
	}
	if len(h.review.items) != 0 {
		t.Fatalf("permanent skip must not park review items: %+v", h.review.items)
	}
	// Permanent: the ledger rows exist → the idempotency check blocks any
	// future DM to these people, and the done marker keeps the application
	// out of the next scan.
	for _, person := range []string{"hr-1", "hr-2"} {
		ok, err := h.db.SetkaDMExists(context.Background(), "acc1", person, 7)
		if err != nil || !ok {
			t.Fatalf("%s: ledger row must exist (skip_reason=account_not_activated): %v", person, err)
		}
	}
	if !h.markerSet("acc1:r1:7") {
		t.Fatal("done marker must be set — no retry on the next scan")
	}
	tasks := h.runScan(t, "acc1")
	if len(tasks) != 0 {
		t.Fatalf("next scan must not re-enqueue: %+v", tasks)
	}
}

// T64: every other send failure keeps the former policy — the row is
// skipped with the transport reason and the human sees a setka_send_failed
// review item.
func TestSendOtherFailureKeepsParkPolicy(t *testing.T) {
	h := newHarness(t, nil)
	h.client.sendErr = errors.New("транспорт умер")
	h.seedApplied(t, "acc1:r1:7")
	if err := h.f.HandleDM(context.Background(), h.runScan(t, "acc1")[0]); err != nil {
		t.Fatalf("dm: %v", err)
	}
	if len(h.client.sent) != 0 {
		t.Fatalf("nothing may be sent: %v", h.client.sent)
	}
	found := false
	for _, it := range h.review.items {
		if it.Kind == "setka_send_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("setka_send_failed review item expected: %+v", h.review.items)
	}
}

// Golden prompts: the AI sees the FULL vacancy dump, the person profile and
// the apply context (classify + personalize inputs).
func TestGoldenPromptInputs(t *testing.T) {
	h := newHarness(t, nil)
	h.seedApplied(t, "acc1:r1:7")
	// Full vacancy dump: the feature must fetch the card with all fields.
	h.f.VacancyFor = func(context.Context, string, int64) (*hhapi.Vacancy, error) {
		return &hhapi.Vacancy{
			ID: 7, Name: "Middle DevOps-инженер", EmployerID: 42, EmployerName: "Acme",
			Description: "Kubernetes, CI/CD, Docker. Удалённо.", Area: "Москва",
		}, nil
	}
	h.runDM(t)

	var classifyIn, personalizeIn string
	var personalizeAll []string
	for i, task := range h.ai.calls {
		switch task {
		case ai.TaskSetkaClassify:
			classifyIn = h.ai.inputs[i]
		case ai.TaskSetkaPersonalize:
			personalizeAll = append(personalizeAll, h.ai.inputs[i])
		}
	}
	if classifyIn == "" || len(personalizeAll) == 0 {
		t.Fatalf("both tasks must run, calls: %v", h.ai.calls)
	}
	personalizeIn = strings.Join(personalizeAll, "\n")
	for _, in := range []string{classifyIn, personalizeIn} {
		for _, want := range []string{"Middle DevOps-инженер", "Acme", "Kubernetes, CI/CD, Docker. Удалённо."} {
			if !strings.Contains(in, want) {
				t.Fatalf("input misses vacancy part %q: %s", want, in)
			}
		}
	}
	for _, want := range []string{"hr-1", "Мария", "профиль hr-1"} {
		if !strings.Contains(personalizeIn, want) {
			t.Fatalf("personalize input misses person part %q: %s", want, personalizeIn)
		}
	}
	if !strings.Contains(personalizeIn, "applied_at") {
		t.Fatalf("personalize input misses the apply context: %s", personalizeIn)
	}
}

// Disabled feature: the scan and the DM handler are no-ops.
func TestDisabledNoop(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Features.Setka.Enabled = false })
	h.seedApplied(t, "acc1:r1:7")
	tasks := h.runScan(t, "acc1")
	if len(tasks) != 0 {
		t.Fatalf("disabled scan must not enqueue: %d", len(tasks))
	}
	if err := h.f.RegisterTasks(context.Background(), h.q, h.reg); err != nil {
		t.Fatalf("register disabled: %v", err)
	}
}

// Feature identity.
func TestFeatureIdentity(t *testing.T) {
	h := newHarness(t, nil)
	if h.f.Name() != "setka" {
		t.Fatalf("name = %s", h.f.Name())
	}
	if !h.f.Enabled() {
		t.Fatal("must be enabled")
	}
}

func (h *harness) markerSet(dedup string) bool {
	v, ok, err := h.db.FeatureStateGet(context.Background(), FeatureName, "acc1", doneMarker(dedup))
	if err != nil {
		return false
	}
	_ = v
	return ok
}

func ctxOf() context.Context { return context.Background() }
