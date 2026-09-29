package multiapply

// T68 regression tests:
//  1. a repeated dry-run must not re-propose the vacancies its previous pass
//     already surveyed — the survey dedup must see dry_run rows (live mode
//     stays blind to them: a survey row never blocks a real application);
//  2. a dry-run pass must respect run.max_per_day — it shows what WOULD have
//     been sent today, so the quota counts today's survey rows;
//  3. run-task seeding is a fair round-robin over the enabled profiles, not
//     a random map order: three consecutive registrations must cover all
//     runs, and within one pass the order is FIFO by id.

import (
	"testing"

	"hhauto/internal/config"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
)

func TestDryRunRepeatDoesNotReproposeSurveyed(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.General.DryRun = true
		r := c.Runs["run1"]
		r.MaxPerDay = 0
		c.Runs["run1"] = r
	})
	h.searchItems = []string{
		vacItem(777100, "Go Dev 1", 9001, "Corp LLC", ""),
		vacItem(777101, "Go Dev 2", 9002, "Other LLC", ""),
	}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.ratings[9002] = hhRating(4.5, 4.7, 100)

	f := h.feature(t)
	for pass := 0; pass < 2; pass++ {
		h.q, h.reg = queue.New(), scheduler.NewRegistry()
		if err := f.RegisterTasks(t.Context(), h.q, h.reg); err != nil {
			t.Fatalf("RegisterTasks (pass %d): %v", pass+1, err)
		}
		if err := f.RunProfile(t.Context(), "run1"); err != nil {
			t.Fatalf("RunProfile (pass %d): %v", pass+1, err)
		}
	}

	m := mustMetrics(t, h.db)
	if m["apply.dry_run"] != 2 {
		t.Fatalf("apply.dry_run = %d, want 2 (the second pass must not re-propose the surveyed vacancies)", m["apply.dry_run"])
	}
	if m["apply.skip.already_applied"] != 2 {
		t.Fatalf("apply.skip.already_applied = %d, want 2 (the survey dedup must see dry_run rows)", m["apply.skip.already_applied"])
	}
}

func TestDryRunRespectsRunQuota(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.General.DryRun = true
		r := c.Runs["run1"]
		r.MaxPerDay = 3
		c.Runs["run1"] = r
	})
	for i := 0; i < 5; i++ {
		emp := int64(9100 + i)
		h.searchItems = append(h.searchItems, vacItem(int64(777110+i), "Go Dev", emp, "Corp LLC", ""))
		h.ratings[emp] = hhRating(4.5, 4.7, 100)
	}

	f := h.feature(t)
	h.q, h.reg = queue.New(), scheduler.NewRegistry()
	if err := f.RegisterTasks(t.Context(), h.q, h.reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if err := f.RunProfile(t.Context(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}

	m := mustMetrics(t, h.db)
	if m["apply.dry_run"] != 3 {
		t.Fatalf("apply.dry_run = %d, want 3 (max_per_day caps the dry-run survey)", m["apply.dry_run"])
	}
	if m["apply.stop.run_quota"] != 1 {
		t.Fatalf("apply.stop.run_quota = %d, want 1", m["apply.stop.run_quota"])
	}
}

func TestRegisterTasksSeedsRunsRoundRobin(t *testing.T) {
	// Map iteration would randomize the seeding order on every process
	// start; a --once pass that spends its budget on the first run task
	// then processes a random single profile and starves the rest. The
	// scenario is repeated: EVERY cycle of four consecutive registrations
	// must cover all three runs without repeats, then wrap.
	for scenario := 0; scenario < 40; scenario++ {
		h := newHarness(t, func(c *config.Config) {
			r := c.Runs["run1"]
			r.Enabled = false
			c.Runs["run1"] = r
			for _, id := range []string{"runA", "runB", "runC"} {
				c.Runs[id] = config.Run{Enabled: true, Account: "acc1", ResumeID: "hash1", Query: "go"}
			}
		})
		f := h.feature(t)
		var heads []string
		for pass := 0; pass < 4; pass++ {
			q := queue.New()
			if err := f.RegisterTasks(t.Context(), q, scheduler.NewRegistry()); err != nil {
				t.Fatalf("RegisterTasks (scenario %d, pass %d): %v", scenario, pass, err)
			}
			task, ok := q.Dequeue(t.Context(), "acc1")
			if !ok {
				t.Fatalf("Dequeue (scenario %d, pass %d): queue empty", scenario, pass)
			}
			heads = append(heads, task.RunID)
		}
		covered := map[string]bool{heads[0]: true, heads[1]: true, heads[2]: true}
		if len(covered) != 3 {
			t.Fatalf("heads %v (scenario %d): three consecutive seedings must cover all runs, not starve", heads, scenario)
		}
		if heads[3] != heads[0] {
			t.Fatalf("heads %v (scenario %d): the fourth seeding must wrap to the first", heads, scenario)
		}
	}
}

func TestRegisterTasksSeedsSortedWithinPass(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Enabled = false
		c.Runs["run1"] = r
		for _, id := range []string{"runA", "runB", "runC"} {
			c.Runs[id] = config.Run{Enabled: true, Account: "acc1", ResumeID: "hash1", Query: "go"}
		}
	})
	f := h.feature(t)
	q := queue.New()
	if err := f.RegisterTasks(t.Context(), q, scheduler.NewRegistry()); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	// Dequeue is blocking: pull exactly the number of seeded tasks.
	var order []string
	for range []string{"runA", "runB", "runC"} {
		task, ok := q.Dequeue(t.Context(), "acc1")
		if !ok {
			break
		}
		order = append(order, task.RunID)
	}
	want := []string{"runA", "runB", "runC"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("seed order = %v, want %v (deterministic FIFO by id within a pass)", order, want)
	}
}
