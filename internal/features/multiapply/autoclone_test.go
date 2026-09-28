package multiapply

import (
	"context"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// T26 autoclone: exhaustion → clone → implicit clone run with the inherited
// config, reapply policy gating, depth/limit caps, review + clone stop on the
// hh limit / stuck not_finished, and a full dry-run tract without mutations.

func boolPtrT(v bool) *bool { return &v }

func twoVacancies(h *harness) {
	h.searchItems = []string{
		vacItem(101, "Go dev", 11, "EmpA", ""),
		vacItem(102, "Go senior", 12, "EmpB", ""),
	}
	h.searchFound = len(h.searchItems)
}

func buildCloneFeature(t *testing.T, h *harness, mutate func(*Options)) *Feature {
	t.Helper()
	opts := Options{
		Config:   h.cfg,
		DB:       h.db,
		APIFor:   func(string) (hhapi.HHAPI, error) { return h.api, nil },
		ChainFor: nil,
		Runner:   h.runner,
		ProfileFor: func(context.Context, string) (profile.Profile, error) {
			return h.profile, nil
		},
		Notifier: h.notify,
		Now:      func() time.Time { return now },
		Log:      quietLogger(),
	}
	if mutate != nil {
		mutate(&opts)
	}
	return New(opts)
}

func metricsOf(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	got, err := db.MetricsSnapshot(context.Background(), featurekit.DayUTC(now))
	if err != nil {
		t.Fatalf("MetricsSnapshot: %v", err)
	}
	return got
}

func TestAutoCloneExhaustionClonesAndReapplies(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(ctx, "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}

	// Parent applied both, clone created once, clone pass re-applied both.
	if got := h.applyCalls; len(got) != 4 || got[0] != 101 || got[1] != 102 || got[2] != 101 || got[3] != 102 {
		t.Fatalf("applyCalls = %v, want [101 102 101 102] (parent pass + clone pass)", got)
	}
	if h.cloneNext != 1 {
		t.Fatalf("clone calls = %d, want 1", h.cloneNext)
	}

	// Lineage recorded locally; hh mirrors it via parentResumeId on native
	// clones (the shard fixture carries it too).
	clone, err := h.db.ResumeClonesGet(ctx, "clone1")
	if err != nil || clone == nil {
		t.Fatalf("ResumeClonesGet(clone1) = %v, %v; want row", clone, err)
	}
	if clone.ParentHash != "hash1" || clone.Depth != 1 || clone.AccountID != "acc1" {
		t.Fatalf("lineage = %+v, want parent hash1 depth 1", clone)
	}

	// Accounting: applications carry the clone resume hash.
	apps, err := h.db.ApplicationsListByAccountAndVacancies(ctx, "acc1", []int64{101})
	if err != nil {
		t.Fatalf("applications: %v", err)
	}
	hashes := map[string]int{}
	for _, a := range apps {
		if a.Status != "applied" {
			t.Fatalf("application %+v: want applied", a)
		}
		hashes[a.HhResumeID]++
	}
	if hashes["hash1"] != 1 || hashes["clone1"] != 1 {
		t.Fatalf("applied-by hashes = %v, want one per lineage resume", hashes)
	}
}

func TestAutoCloneNewOnlySkipsTouched(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyNewOnly,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	// The clone ran but re-applied nothing: every vacancy is already touched
	// by the lineage.
	if got := h.applyCalls; len(got) != 2 {
		t.Fatalf("applyCalls = %v, want [101 102] (parent only)", got)
	}
	if h.cloneNext != 1 {
		t.Fatalf("clone calls = %d, want 1 (clone is still created)", h.cloneNext)
	}
	m := metricsOf(t, h.db)
	if m["apply.skip.reapply_touched"] != 2 {
		t.Fatalf("apply.skip.reapply_touched = %d, want 2", m["apply.skip.reapply_touched"])
	}
}

func TestAutoCloneSkipRecentRejections(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplySkipRecentRejections,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)

	v101, v102 := int64(101), int64(102)
	recent := now.Add(-5 * 24 * time.Hour)
	old := now.Add(-40 * 24 * time.Hour)
	if err := h.db.ChatsUpsert(context.Background(), &store.Chat{
		AccountID: "acc1", ChatikChatID: "ch1", VacancyID: &v101,
		State: "rejected", LastMessageAt: &recent, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("chats upsert: %v", err)
	}
	if err := h.db.ChatsUpsert(context.Background(), &store.Chat{
		AccountID: "acc1", ChatikChatID: "ch2", VacancyID: &v102,
		State: "rejected", LastMessageAt: &old, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("chats upsert: %v", err)
	}

	f := buildCloneFeature(t, h, nil)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	// 101: rejected 5 days ago (inside the 30-day window) → skipped by the
	// clone. 102: rejected 40 days ago (outside) → re-applied.
	if got := h.applyCalls; len(got) != 3 || got[2] != 102 {
		t.Fatalf("applyCalls = %v, want [101 102 102]", got)
	}
}

func TestAutoCloneDepthGateBlocksCloneOfClone(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 5, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	// The clone pass exhausts the pool again; max_clone_depth=1 forbids the
	// clone-of-clone.
	if h.cloneNext != 1 {
		t.Fatalf("clone calls = %d, want 1 (depth gate)", h.cloneNext)
	}
	m := metricsOf(t, h.db)
	if m["autoclone.skip_depth"] != 1 {
		t.Fatalf("autoclone.skip_depth = %d, want 1", m["autoclone.skip_depth"])
	}
}

func TestAutoCloneMaxPerResumeGate(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 1, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile #1: %v", err)
	}
	if h.clonePosts != 1 {
		t.Fatalf("clone posts after run 1 = %d, want 1", h.clonePosts)
	}
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile #2: %v", err)
	}
	// The second pass is fully exhausted; with the depth budget spent the
	// direct-children cap (1) blocks another clone of the parent.
	if h.clonePosts != 1 {
		t.Fatalf("clone posts after run 2 = %d, want 1 (max_clones_per_resume)", h.clonePosts)
	}
	m := metricsOf(t, h.db)
	if m["autoclone.skip_limit"] != 1 {
		t.Fatalf("autoclone.skip_limit = %d, want 1", m["autoclone.skip_limit"])
	}
}

func TestAutoCloneHHLimitReviewsAndStopsAccount(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyNewOnly,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	h.cloneFail = func(int) (int, string) { return 403, `{"error":"resume limit exceeded"}` }
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	open, err := h.db.ReviewItemsOpen(context.Background(), "acc1", 100)
	if err != nil {
		t.Fatalf("review open: %v", err)
	}
	found := false
	for _, it := range open {
		if it.Kind == "autoclone_stopped" && strings.Contains(it.Reason, "limit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no autoclone_stopped review item in %+v", open)
	}
	if _, ok, _ := h.db.FeatureStateGet(context.Background(), "multiapply", "acc1", cloneStopKey); !ok {
		t.Fatalf("clone stop flag not set")
	}

	// The stop flag freezes all further clone attempts for the account.
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile #2: %v", err)
	}
	if h.clonePosts != 1 {
		t.Fatalf("clone posts = %d, want 1 (account clone stop)", h.clonePosts)
	}
}

func TestAutoCloneNotFinishedTimesOutToReview(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	h.cloneStatus = "not_finished" // the fake never flips it
	f := buildCloneFeature(t, h, func(o *Options) {
		o.ClonePollInterval = time.Millisecond
		o.CloneFinishTimeout = 30 * time.Millisecond
	})

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if h.cloneNext != 1 {
		t.Fatalf("clone calls = %d, want 1", h.cloneNext)
	}
	// No clone pass: the parent's applies are the only ones.
	if got := h.applyCalls; len(got) != 2 {
		t.Fatalf("applyCalls = %v, want [101 102]", got)
	}
	open, err := h.db.ReviewItemsOpen(context.Background(), "acc1", 100)
	if err != nil {
		t.Fatalf("review open: %v", err)
	}
	found := false
	for _, it := range open {
		if it.Kind == "autoclone_stopped" && strings.Contains(it.Reason, "not_finished") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no autoclone_stopped (not_finished) review item in %+v", open)
	}
}

func TestAutoCloneDryRunMakesNoMutations(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.General.DryRun = true
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if h.cloneNext != 0 {
		t.Fatalf("clone calls = %d, want 0 (dry-run never mutates)", h.cloneNext)
	}
	clones, err := h.db.ResumeClonesListByAccount(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("list clones: %v", err)
	}
	if len(clones) != 0 {
		t.Fatalf("lineage rows = %d, want 0", len(clones))
	}
	m := metricsOf(t, h.db)
	if m["autoclone.dry_run"] != 1 {
		t.Fatalf("autoclone.dry_run = %d, want 1", m["autoclone.dry_run"])
	}
}

func TestAutoCloneCloneRunInheritsLimitsAndOverride(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		run := c.Runs["run1"]
		run.MaxPerDay = 10
		c.Runs["run1"] = run
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
			RunOverride: config.RunOverride{MaxPerDay: 1},
		}
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	// The parent (MaxPerDay=10) applied both; the clone run inherits the run
	// identity, so the shared counter (2) already exceeds the overridden
	// clone-run cap (1): the clone pass stops on quota before applying.
	if got := h.applyCalls; len(got) != 2 {
		t.Fatalf("applyCalls = %v, want [101 102]", got)
	}
	m := metricsOf(t, h.db)
	if m["apply.stop.run_quota"] != 1 {
		t.Fatalf("apply.stop.run_quota = %d, want 1 (override propagated to the clone run)", m["apply.stop.run_quota"])
	}
}

func TestLetterModeTemplateOnly(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		run := c.Runs["run1"]
		run.LetterMode = config.LetterModeTemplate
		run.LetterTemplate = "ШАБЛОН ОТПРАВЛЕН"
		c.Runs["run1"] = run
	})
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if len(h.applyBodies) != 2 {
		t.Fatalf("apply bodies = %d, want 2", len(h.applyBodies))
	}
	for i, b := range h.applyBodies {
		if !strings.Contains(b, "%D0%A8%D0%90%D0%91%D0%9B%D0%9E%D0%9D") {
			t.Fatalf("apply body %d carries no verbatim template: %q", i, b)
		}
	}
	for _, task := range h.runner.calls {
		if task == "cover_letter" {
			t.Fatalf("letter_mode=template must not call the AI, calls=%v", h.runner.calls)
		}
	}
}

func TestLetterModeTemplateAIPolishesAndFallsBack(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		run := c.Runs["run1"]
		run.LetterMode = config.LetterModeTemplateAI
		run.LetterTemplate = "БАЗА"
		c.Runs["run1"] = run
	})
	h.runner.letter = letterVerdict("ОТПОЛИРОВАНО")
	twoVacancies(h)
	f := buildCloneFeature(t, h, nil)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if len(h.applyBodies) != 2 {
		t.Fatalf("apply bodies = %d, want 2", len(h.applyBodies))
	}
	if !strings.Contains(h.applyBodies[0], "%D0%9E%D0%A2%D0%9F%D0%9E%D0%9B%D0%98%D0%A0%D0%9E%D0%92%D0%90%D0%9D%D0%9E") {
		t.Fatalf("template_ai letter not used: %q", h.applyBodies[0])
	}
	letterInput := ""
	for i, task := range h.runner.calls {
		if task == ai.TaskCoverLetter {
			letterInput = h.runner.inputs[i]
		}
	}
	if !strings.Contains(letterInput, `"letter_mode":"template_ai"`) || !strings.Contains(letterInput, `"template":"БАЗА"`) {
		t.Fatalf("AI input misses template_ai wiring: %s", letterInput)
	}

	// AI failure falls back to the raw template (never empty). The AI filter
	// is off here so the broken runner only affects the letter path.
	h2 := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.AIFilter = false
		run := c.Runs["run1"]
		run.LetterMode = config.LetterModeTemplateAI
		run.LetterTemplate = "БАЗА"
		c.Runs["run1"] = run
	})
	h2.runner.err = context.DeadlineExceeded
	twoVacancies(h2)
	f2 := buildCloneFeature(t, h2, nil)
	if err := f2.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile fallback: %v", err)
	}
	if !strings.Contains(h2.applyBodies[0], "%D0%91%D0%90%D0%97%D0%90") {
		t.Fatalf("template fallback not used: %q", h2.applyBodies[0])
	}
}

func TestLetterModeAICompanyWiring(t *testing.T) {
	h := newHarness(t, nil) // defaults: ai_company
	twoVacancies(h)
	h.popupByVac[101] = popupWith(true, false, "ENABLED")
	h.popupByVac[102] = popupWith(true, false, "ENABLED")
	f := buildCloneFeature(t, h, nil)
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	last := ""
	for i, task := range h.runner.calls {
		if task == ai.TaskCoverLetter {
			last = h.runner.inputs[i]
		}
	}
	if last == "" {
		t.Fatalf("no cover_letter call in %v", h.runner.calls)
	}
	if !strings.Contains(last, `"letter_mode":"ai_company"`) {
		t.Fatalf("AI input misses ai_company mode: %s", last)
	}
}

func TestDeriveCloneRun(t *testing.T) {
	run := config.Run{
		Enabled: true, Account: "acc1", ResumeID: "hash1", MaxPerDay: 7,
		LetterMode: config.LetterModeTemplateAI, LetterTemplate: "T1", Query: "go",
	}
	got := deriveCloneRun(run, config.RunOverride{})
	if got.ResumeID != "hash1" || got.MaxPerDay != 7 || got.LetterMode != config.LetterModeTemplateAI || got.LetterTemplate != "T1" || got.Query != "go" {
		t.Fatalf("deriveCloneRun without override drifted: %+v", got)
	}
	got = deriveCloneRun(run, config.RunOverride{
		MaxPerDay: 3, LetterMode: config.LetterModeTemplate, LetterTemplate: "T2",
	})
	if got.ResumeID != "hash1" || got.Query != "go" {
		t.Fatalf("identity fields must inherit: %+v", got)
	}
	if got.MaxPerDay != 3 || got.LetterMode != config.LetterModeTemplate || got.LetterTemplate != "T2" {
		t.Fatalf("overrides not applied: %+v", got)
	}
}

// T45: hh answers clone_resumes 200 while the cloned resume still reads
// back as an empty not_finished draft. The verify gate must fail the clone
// typed (incomplete), park for review with the incomplete-clone semantics —
// never spawn a clone pass on the unusable duplicate.
func TestAutoCloneIncompleteCloneParksForReview(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *config.Config) {
		c.Features.AutoClone = config.AutoClone{
			Enabled: true, MaxClonesPerResume: 2, MaxCloneDepth: 1,
			Autolift: boolPtrT(true), ReapplyMode: config.ReapplyAll,
			SkipRejectionDays: 30, FinishTimeoutMin: 10,
		}
	})
	twoVacancies(h)
	h.cloneNativeIncomplete = true
	f := buildCloneFeature(t, h, nil)

	if err := f.RunProfile(ctx, "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
	if h.cloneNext != 1 {
		t.Fatalf("clone calls = %d, want 1", h.cloneNext)
	}
	// No clone pass: the parent's applies are the only ones.
	if got := h.applyCalls; len(got) != 2 {
		t.Fatalf("applyCalls = %v, want [101 102]", got)
	}
	open, err := h.db.ReviewItemsOpen(ctx, "acc1", 100)
	if err != nil {
		t.Fatalf("review open: %v", err)
	}
	found := false
	for _, it := range open {
		if it.Kind == "autoclone_stopped" && strings.Contains(it.Reason, "materializ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no autoclone_stopped (materialize) review item in %+v", open)
	}
	m := metricsOf(t, h.db)
	if m["autoclone.incomplete"] != 1 {
		t.Fatalf("autoclone.incomplete = %d, want 1", m["autoclone.incomplete"])
	}
}
