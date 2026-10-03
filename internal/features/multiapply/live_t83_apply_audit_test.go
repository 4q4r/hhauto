//go:build live

package multiapply

// T83 apply POST audit (phase B2): the real gate→solve→apply chain with a
// logging decorator around the production hhapi client — the probe prints
// exactly what was POSTed (test answers included) and what hh answered
// (topic id / error), then audits the negotiations listing.
//
//	go test -tags live -run TestLiveT83ApplyAudit -v ./internal/features/multiapply/
//
// Safe to repeat: the vacancy has NO negotiation topic (verified), so the
// POST is the first effective response.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// t83LogAPI wraps the production client, logging every ApplyPopup,
// VacancyTests and Apply exchange (the rest passes through via embedding).
type t83LogAPI struct {
	hhapi.HHAPI
	t *testing.T
}

func (w *t83LogAPI) ApplyPopup(ctx context.Context, vacancyID int64) (*hhapi.ApplyMeta, error) {
	meta, err := w.HHAPI.ApplyPopup(ctx, vacancyID)
	raw, _ := json.Marshal(meta)
	w.t.Logf("ApplyPopup(%d) -> meta=%s err=%v", vacancyID, raw, err)
	return meta, err
}

func (w *t83LogAPI) VacancyTests(ctx context.Context, vacancyID int64) (*hhapi.VacancyTest, error) {
	vt, err := w.HHAPI.VacancyTests(ctx, vacancyID)
	if err != nil {
		w.t.Logf("VacancyTests(%d) -> err=%v", vacancyID, err)
		return vt, err
	}
	w.t.Logf("VacancyTests(%d) -> uidPk=%q guid=%q startTime=%q required=%v tasks=%d",
		vacancyID, vt.UIDPk, vt.GUID, vt.StartTime, vt.Required, len(vt.Tasks))
	for i, task := range vt.Tasks {
		w.t.Logf("  task[%d] id=%q q=%.80q solutions=%d", i, task.ID, task.Question, len(task.Solutions))
	}
	return vt, nil
}

func (w *t83LogAPI) Apply(ctx context.Context, req hhapi.ApplyRequest) (hhapi.NegotiationRef, error) {
	ref, err := w.HHAPI.Apply(ctx, req)
	if req.Test != nil {
		for _, a := range req.Test.Answers {
			w.t.Logf("Apply(%d): answer task=%q solution=%q text=%.120q", req.VacancyID, a.TaskID, a.SolutionID, a.Text)
		}
		w.t.Logf("Apply(%d): test coords uidPk=%q guid=%q startTime=%q required=%v letterLen=%d",
			req.VacancyID, req.Test.UIDPk, req.Test.GUID, req.Test.StartTime, req.Test.Required, len(req.Letter))
	}
	w.t.Logf("Apply(%d) -> ref=%+v err=%v", req.VacancyID, ref, err)
	return ref, err
}

func (w *t83LogAPI) Negotiations(ctx context.Context, page int) ([]hhapi.Negotiation, error) {
	topics, err := w.HHAPI.Negotiations(ctx, page)
	w.t.Logf("Negotiations(page=%d) -> %d topics err=%v", page, len(topics), err)
	for _, n := range topics {
		if n.VacancyID == 137339427 {
			w.t.Logf("  TOPIC FOR 137339427: %+v", n)
		}
	}
	return topics, err
}

func TestLiveT83ApplyAudit(t *testing.T) {
	cfg, realDB, client := t83LoadAll(t)
	defer realDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	prof, err := profile.LoadUserContext(ctx, *cfg, "main", realDB, client)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	var run config.Run
	for _, r := range cfg.Runs {
		if r.Enabled {
			run = r
			break
		}
	}
	logged := &t83LogAPI{HHAPI: client, t: t}

	tasksB := make(map[string]config.AITask, len(cfg.AI.Tasks)+1)
	for k, task := range cfg.AI.Tasks {
		tasksB[k] = task
	}
	tasksB[string(ai.TaskTestAnswer)] = cfg.AI.Tasks["cover_letter"]

	probeDB, err := store.Open(ctx, fmt.Sprintf("%s/t83-b2.db", t.TempDir()), store.Options{})
	if err != nil {
		t.Fatalf("temp db: %v", err)
	}
	defer probeDB.Close()
	f := New(Options{
		Config: *cfg, DB: probeDB,
		APIFor:     func(string) (hhapi.HHAPI, error) { return logged, nil },
		Runner:     t83ProbeRunner(t, cfg, tasksB),
		ProfileFor: func(context.Context, string) (profile.Profile, error) { return prof, nil },
		Log:        quietLogger(),
	})
	v := hhapi.Vacancy{ID: 137339427, Name: "QA Automation Engnieer(Python) Сетевой ОС", EmployerName: "Бифорком Тек"}
	stop, skip, letterRequired, tests := f.popupGate(ctx, "main", logged, v)
	t.Logf("popupGate: stop=%q skip=%q letterRequired=%v tests=%v", stop, skip, letterRequired, tests != nil)
	if stop != "" || skip != "" {
		t.Fatalf("gate settled early: stop=%q skip=%q", stop, skip)
	}
	stop, skip = f.applyOne(ctx, "t83-b2", run, "main", logged, prof, "hash1", v, letterRequired, tests, nil, newPassStats())
	t.Logf("applyOne: stop=%q skip=%q (empty both = applied)", stop, skip)
}
