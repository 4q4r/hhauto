package multiapply

// T84: the apply pass responds best-first — the scorer's verdict orders
// the queue (score desc, deterministic ties) and the daily quotas cut the
// least probable tail instead of whatever the search order surfaced first.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

// scoredRunner scripts one verdict per vacancy id (the shared fake scores
// every vacancy alike, which cannot expose ordering).
type scoredRunner struct {
	byVac map[int64]string
}

func (r *scoredRunner) Run(_ context.Context, _, _, _, _ string, task ai.TaskName, input any) (json.RawMessage, ai.Decision, error) {
	if task != ai.TaskVacancyScoring {
		return nil, ai.Decision{}, fmt.Errorf("t84: unexpected task %v", task)
	}
	b, err := json.Marshal(input)
	if err != nil {
		return nil, ai.Decision{}, err
	}
	var probe struct {
		Vacancy struct {
			ID int64 `json:"id"`
		} `json:"vacancy"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, ai.Decision{}, err
	}
	v, ok := r.byVac[probe.Vacancy.ID]
	if !ok {
		return nil, ai.Decision{}, fmt.Errorf("t84: no scripted verdict for vacancy %d", probe.Vacancy.ID)
	}
	return json.RawMessage(v), ai.Decision{}, nil
}

// t84Run drives run1 through the harness with a test-specific runner.
func t84Run(t *testing.T, h *harness, runner ai.Runner) {
	t.Helper()
	f := New(Options{
		Config:   h.cfg,
		DB:       h.db,
		APIFor:   func(string) (hhapi.HHAPI, error) { return h.api, nil },
		ChainFor: func(string) (captcha.Chain, bool) { return h.chain, true },
		Runner:   runner,
		ProfileFor: func(context.Context, string) (profile.Profile, error) {
			return h.profile, nil
		},
		Notifier: h.notify,
		Now:      func() time.Time { return now },
		Log:      quietLogger(),
	})
	h.q, h.reg = queue.New(), scheduler.NewRegistry()
	if err := f.RegisterTasks(context.Background(), h.q, h.reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
}

// The owner scenario: scores [48, 64, 52] with a daily quota of 2 — the
// pass responds 64 then 52, and the 48 tail stays a candidate for the
// next cycle.
func TestApplyBestFirstUnderQuota(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.MinMatchPercent = 40 // all three clear the gate
		r := c.Runs["run1"]
		r.MaxPerDay = 2
		c.Runs["run1"] = r
	})
	h.searchItems = []string{
		vacItem(777301, "Weak match", 9001, "Corp LLC", ""),
		vacItem(777302, "Best match", 9002, "Other LLC", ""),
		vacItem(777303, "Good match", 9003, "Third LLC", ""),
	}
	t84Run(t, h, &scoredRunner{byVac: map[int64]string{
		777301: scoringVerdict(48, "ok", false, false),
		777302: scoringVerdict(64, "strong", false, false),
		777303: scoringVerdict(52, "ok", false, false),
	}})

	if len(h.applyCalls) != 2 || h.applyCalls[0] != 777302 || h.applyCalls[1] != 777303 {
		t.Fatalf("apply order = %v, want [777302 777303] (best-first, quota cut the 48)", h.applyCalls)
	}
}

// Equal scores resolve deterministically: vacancy id ascending — the same
// pool produces the same queue on every run.
func TestApplyBestFirstTieBreakIsDeterministic(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{
		vacItem(777313, "Tie C", 9001, "Corp LLC", ""),
		vacItem(777311, "Tie A", 9002, "Other LLC", ""),
		vacItem(777312, "Tie B", 9003, "Third LLC", ""),
	}
	t84Run(t, h, &scoredRunner{byVac: map[int64]string{
		777311: scoringVerdict(70, "ok", false, false),
		777312: scoringVerdict(70, "ok", false, false),
		777313: scoringVerdict(70, "ok", false, false),
	}})

	if len(h.applyCalls) != 3 || h.applyCalls[0] != 777311 || h.applyCalls[1] != 777312 || h.applyCalls[2] != 777313 {
		t.Fatalf("apply order = %v, want id-ascending [777311 777312 777313]", h.applyCalls)
	}
}

// The best-first order must not corrupt the per-employer daily bucket:
// the employer cap still consumes that employer's highest-scored
// vacancies first, the rest skips.
func TestApplyBestFirstKeepsEmployerBucket(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{
		vacItem(777321, "Same employer low", 9001, "Corp LLC", ""),
		vacItem(777322, "Same employer best", 9001, "Corp LLC", ""),
		vacItem(777323, "Same employer mid", 9001, "Corp LLC", ""),
	}
	t84Run(t, h, &scoredRunner{byVac: map[int64]string{
		777321: scoringVerdict(70, "ok", false, false),
		777322: scoringVerdict(95, "strong", false, false),
		777323: scoringVerdict(85, "strong", false, false),
	}})

	if len(h.applyCalls) != 2 || h.applyCalls[0] != 777322 || h.applyCalls[1] != 777323 {
		t.Fatalf("apply order = %v, want [777322 777323] (employer cap 2 keeps the best)", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.employer_daily_cap"] != 1 {
		t.Fatalf("metrics = %v, want one employer_daily_cap skip", mustMetrics(t, h.db))
	}
}

// Without the AI filter nothing carries a verdict: no score to rank by,
// the queue keeps the pool order (the documented no-verdict path).
func TestApplyBestFirstUnscoredKeepsPoolOrder(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Features.MultiApply.AIFilter = false
	})
	h.searchItems = []string{
		vacItem(777333, "First", 9001, "Corp LLC", ""),
		vacItem(777331, "Second", 9002, "Other LLC", ""),
		vacItem(777332, "Third", 9003, "Third LLC", ""),
	}
	t84Run(t, h, nil)

	if len(h.applyCalls) != 3 || h.applyCalls[0] != 777333 || h.applyCalls[1] != 777331 || h.applyCalls[2] != 777332 {
		t.Fatalf("apply order = %v, want pool order [777333 777331 777332]", h.applyCalls)
	}
}
