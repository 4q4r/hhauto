//go:build live

package multiapply

// T83 LIVE APPLY PROBE (manual, network + owner session + owner AI key
// required; never in the default gate):
//
//	go test -tags live -run TestLiveT83ApplyPath -v ./internal/features/multiapply/
//
// Runs the REAL gate→solve→apply chain for vacancy 137339427 (the one that
// slipped past T77+T80 and died with 400 {"error":"test-required"} on
// 2026-10-03). Two runners, two isolated temp DBs:
//
//	A — the exact production wiring: the runner built from the owner's
//	    [ai.tasks] (resume_improve/vacancy_scoring/cover_letter — no
//	    test_answer task configured) → the expected live outcome is the
//	    fail-closed skip test_required, never a 400 error line.
//	B — the same, plus test_answer bound to the owner's letter model
//	    profile → the AI-solved answers ride the real apply POST (wire
//	    proof for the T80 form: uidPk/guid/startTime/task_*_text).
//
// The owner's config, cookies and session are read-only inputs; the feature
// writes go to throwaway DBs. A successful phase B sends a REAL response to
// the vacancy (owner-approved: QA is a target role).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
	"hhauto/internal/tlsfp"
)

func t83LoadAll(t *testing.T) (*config.Config, *store.DB, *hhapi.Client) {
	t.Helper()
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acct config.Account
	for _, a := range cfg.Accounts {
		acct = a
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	if spec.UserAgent != "" {
		acct.UserAgent = spec.UserAgent
	}
	c, err := hhapi.NewClient(acct, cfg.API.Endpoints, nil, nil, hhapi.WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	rawCk, err := os.ReadFile(filepath.Join(cfg.General.DataDir, "cookies", "main.bin"))
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(rawCk, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	realDB, err := store.Open(ctx, cfg.Storage.Path, store.Options{BusyTimeoutMS: cfg.Storage.BusyTimeoutMS})
	if err != nil {
		t.Fatalf("open real db (read-only profile source): %v", err)
	}
	return cfg, realDB, c
}

// t83ProbeRunner builds a real ai.Runner over the config's providers and
// the given task set (the buildAI wiring, narrowed to what the probe runs).
func t83ProbeRunner(t *testing.T, cfg *config.Config, tasks map[string]config.AITask) ai.Runner {
	t.Helper()
	providers := make(map[string]ai.Provider, len(cfg.AI.Providers))
	for id, p := range cfg.AI.Providers {
		var prov ai.Provider
		var err error
		switch p.Dialect {
		case ai.DialectOpenAIChat:
			prov, err = ai.NewOpenAIChatProvider(id, p)
		case ai.DialectOpenAIResponses:
			prov, err = ai.NewOpenAIResponsesProvider(id, p)
		case ai.DialectAnthropicMessages:
			prov, err = ai.NewAnthropicProvider(id, p)
		default:
			t.Fatalf("provider %q: unknown dialect %q", id, p.Dialect)
		}
		if err != nil {
			t.Fatalf("provider %q: %v", id, err)
		}
		providers[id] = prov
	}
	runner, err := ai.New(providers, cfg.AI.Models, tasks, nil, ai.NewMemoryCache(), quietLogger())
	if err != nil {
		t.Fatalf("ai.New: %v", err)
	}
	return runner
}

func TestLiveT83ApplyPath(t *testing.T) {
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
	v := hhapi.Vacancy{ID: 137339427, Name: "QA Automation Engnieer(Python) Сетевой ОС", EmployerName: "Бифорком Тек"}

	// Phase A — production wiring: no test_answer task configured.
	phase := func(label string, runner ai.Runner) {
		t.Helper()
		probeDB, err := store.Open(ctx, fmt.Sprintf("%s/t83-%s.db", t.TempDir(), label), store.Options{})
		if err != nil {
			t.Fatalf("%s: temp db: %v", label, err)
		}
		defer probeDB.Close()
		f := New(Options{
			Config: *cfg, DB: probeDB,
			APIFor:     func(string) (hhapi.HHAPI, error) { return client, nil },
			Runner:     runner,
			ProfileFor: func(context.Context, string) (profile.Profile, error) { return prof, nil },
			Log:        quietLogger(),
		})
		stop, skip, letterRequired, tests := f.popupGate(ctx, "main", client, v)
		t.Logf("[%s] popupGate: stop=%q skip=%q letterRequired=%v tests=%v", label, stop, skip, letterRequired, tests != nil)
		if stop != "" || skip != "" {
			t.Logf("[%s] settled at the gate: skip=%q (no apply POST)", label, skip)
			return
		}
		stop, skip = f.applyOne(ctx, "t83-"+label, run, "main", client, prof, "hash1", v, letterRequired, tests, nil, newPassStats())
		t.Logf("[%s] applyOne: stop=%q skip=%q (empty both = applied)", label, stop, skip)
	}

	phase("A-prod", t83ProbeRunner(t, cfg, cfg.AI.Tasks))

	tasksB := make(map[string]config.AITask, len(cfg.AI.Tasks)+1)
	for k, task := range cfg.AI.Tasks {
		tasksB[k] = task
	}
	tasksB[string(ai.TaskTestAnswer)] = cfg.AI.Tasks["cover_letter"] // owner's letter model profile
	phase("B-solved", t83ProbeRunner(t, cfg, tasksB))
}
