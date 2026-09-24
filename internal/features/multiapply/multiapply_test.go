package multiapply

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

func TestHappyPathSearchSourceAppliesWithLetter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.MinEmployerRating = 0
		c.Runs["run1"] = r
	})
	h.searchItems = []string{vacItem(777001, "Go Backend", 9001, "Corp LLC", "")}
	h.searchFound = 1
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[777001] = popupWith(true, false, "ENABLED") // letter required

	h.run(t)

	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777001 {
		t.Fatalf("apply calls = %v, want [777001]", h.applyCalls)
	}
	body := h.applyBodies[0]
	for _, want := range []string{"resume_hash=hash1", "vacancy_id=777001", "letter="} {
		if !strings.Contains(body, want) {
			t.Fatalf("apply body missing %q: %s", want, body)
		}
	}
	if !strings.Contains(body, letterEscape("Здравствуйте! Готов обсудить детали.")) {
		t.Fatalf("apply body missing AI letter: %s", body)
	}
	app := mustApplication(t, h.db, "acc1", 777001)
	if app.Status != "applied" || app.RunID != "run1" {
		t.Fatalf("application = %+v", app)
	}
	snap := mustMetrics(t, h.db)
	if snap["apply.ok"] != 1 {
		t.Fatalf("metrics = %v", snap)
	}
	if !h.notify.has(webhook.EvApplySent) {
		t.Fatalf("no %s webhook emitted", webhook.EvApplySent)
	}
	// The intent is done (hard barrier for the same vacancy).
	claim, _ := featurekit.BeginIntent(t.Context(), h.db, "apply:acc1:777001", "{}")
	if claim.Step != featurekit.IntentReconcileFirst {
		t.Fatalf("intent step = %v, want reconcile-first (done)", claim.Step)
	}
}

func TestSuitableSourceUsedWhenNoQuery(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Query = ""
		c.Runs["run1"] = r
	})
	h.suitable = []string{vacItem(777002, "Go Developer", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if len(h.applyCalls) != 1 || h.applyCalls[0] != 777002 {
		t.Fatalf("apply calls = %v, want [777002] from the suitable source", h.applyCalls)
	}
}

func TestExcludeWordInNameSkipsBeforePopup(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.ExcludeWords = []string{"стажировка"}
		c.Runs["run1"] = r
	})
	h.searchItems = []string{vacItem(777003, "Стажировка Go-разработчиком", 9001, "Corp LLC", "")}

	h.run(t)

	if len(h.applyCalls) != 0 || h.popupReqs[777003] != 0 {
		t.Fatalf("early exit violated: applies=%v popupReqs=%v", h.applyCalls, h.popupReqs)
	}
	if mustMetrics(t, h.db)["apply.skip.exclude_words"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

func TestExcludeInDescriptionOnlyWhenEnabled(t *testing.T) {
	desc := `"description":"Java побочно, но ищем Go"`

	// Off (default): description is not scanned.
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.ExcludeWords = []string{"java"}
		c.Runs["run1"] = r
	})
	h.searchItems = []string{vacItem(777004, "Go Dev", 9001, "Corp LLC", desc)}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.run(t)
	if len(h.applyCalls) != 1 {
		t.Fatalf("exclude_in_description=false: applies = %v, want applied", h.applyCalls)
	}

	// On: description scan skips.
	h2 := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.ExcludeWords = []string{"java"}
		r.Filters.ExcludeInDesc = true
		c.Runs["run1"] = r
	})
	h2.searchItems = []string{vacItem(777004, "Go Dev", 9001, "Corp LLC", desc)}
	h2.run(t)
	if len(h2.applyCalls) != 0 {
		t.Fatalf("exclude_in_description=true: applies = %v, want none", h2.applyCalls)
	}
}

func TestBlacklistByIDAndName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		black    []string
		empID    int64
		empName  string
		wantSkip bool
	}{
		{"by id", []string{"9001"}, 9001, "Corp LLC", true},
		{"by name", []string{"corp llc"}, 9002, "Corp LLC", true},
		{"not listed", []string{"9009", "Other LLC"}, 9001, "Corp LLC", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *config.Config) {
				r := c.Runs["run1"]
				r.Filters.CompanyBlacklist = tc.black
				c.Runs["run1"] = r
			})
			h.searchItems = []string{vacItem(777005, "Go Dev", tc.empID, tc.empName, "")}
			h.ratings[tc.empID] = hhRating(4.5, 4.7, 100)
			h.run(t)
			if tc.wantSkip && len(h.applyCalls) != 0 {
				t.Fatalf("applies = %v, want none", h.applyCalls)
			}
			if !tc.wantSkip && len(h.applyCalls) != 1 {
				t.Fatalf("applies = %v, want 1", h.applyCalls)
			}
		})
	}
}

func TestWhitelistGatesAndBypassesStages(t *testing.T) {
	// Whitelisted employer + bypass ["F4","F6_threshold"]:
	// rating widget is never hit and a low AI score does not skip,
	// but needs_human still does.
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.CompanyWhitelist = []string{"Corp LLC"}
		r.Filters.WhitelistBypass = []string{"F4", "F6_threshold"}
		r.Filters.MinEmployerRating = 4.9
		c.Runs["run1"] = r
	})
	h.runner.scoring = scoringVerdict(5, "weak", false, false) // would fail the threshold
	h.searchItems = []string{vacItem(777006, "Go Dev", 9001, "Corp LLC", "")}
	h.run(t)

	if len(h.applyCalls) != 1 {
		t.Fatalf("applies = %v, want 1 (F4+F6_threshold bypassed for whitelisted)", h.applyCalls)
	}
	if h.ratingReqs[9001] != 0 {
		t.Fatalf("rating requests = %d, want 0 (F4 bypassed)", h.ratingReqs[9001])
	}
	if mustMetrics(t, h.db)["apply.skip.score_below_threshold"] != 0 {
		t.Fatalf("threshold must be bypassed: %v", mustMetrics(t, h.db))
	}

	// Non-whitelisted employer is skipped outright.
	h2 := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.CompanyWhitelist = []string{"Corp LLC"}
		c.Runs["run1"] = r
	})
	h2.searchItems = []string{vacItem(777007, "Go Dev", 9002, "Other LLC", "")}
	h2.run(t)
	if len(h2.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (not whitelisted)", h2.applyCalls)
	}
	if mustMetrics(t, h2.db)["apply.skip.not_whitelisted"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h2.db))
	}
}

func TestEmployerRatingLowSkips(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.MinEmployerRating = 4.0
		r.Filters.RatingPeriod = "all"
		c.Runs["run1"] = r
	})
	h.searchItems = []string{vacItem(777008, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(3.2, 3.9, 40)

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.rating_low"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	// no_reviews policy=human: review item persisted and vacancy skipped.
	h2 := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.Filters.MinEmployerRating = 4.0
		r.Filters.NoReviewsPolicy = "human"
		c.Runs["run1"] = r
	})
	h2.searchItems = []string{vacItem(777008, "Go Dev", 9001, "Corp LLC", "")}
	h2.ratings[9001] = hhRating(0, 0, 0)
	h2.run(t)
	if len(h2.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none", h2.applyCalls)
	}
	open, err := h2.db.ReviewItemsOpen(t.Context(), "acc1", 10)
	if err != nil || len(open) == 0 {
		t.Fatalf("review items = %d err=%v, want >=1", len(open), err)
	}
}

func TestDedupExistingApplicationSkips(t *testing.T) {
	h := newHarness(t, nil)
	key := featurekit.DedupKey("acc1", "hash1", 777009)
	mustCreateApplication(t, h.db, key, "applied")
	h.searchItems = []string{vacItem(777009, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if len(h.applyCalls) != 0 || h.popupReqs[777009] != 0 {
		t.Fatalf("dedup violated: applies=%v popupReqs=%v", h.applyCalls, h.popupReqs)
	}
	if mustMetrics(t, h.db)["apply.skip.already_applied"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

func TestDedupNativeAutoresponse(t *testing.T) {
	raw := `"autoResponse":{"acceptAutoResponse":false}`
	build := func(dedupNative bool) *harness {
		h := newHarness(t, func(c *config.Config) {
			c.Features.MultiApply.DedupNativeResp = dedupNative
		})
		h.searchItems = []string{vacItem(777010, "Go Dev", 9001, "Corp LLC", raw)}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		return h
	}
	h := build(true)
	h.run(t)
	if len(h.applyCalls) != 0 {
		t.Fatalf("dedup_native: applies = %v, want none", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.native_autoresponse"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}

	h2 := build(false)
	h2.run(t)
	if len(h2.applyCalls) != 1 {
		t.Fatalf("no dedup_native: applies = %v, want 1", h2.applyCalls)
	}
}

func TestAIFilterSkipsBelowThreshold(t *testing.T) {
	h := newHarness(t, nil)
	h.runner.scoring = scoringVerdict(30, "weak", false, false)
	h.searchItems = []string{vacItem(777011, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if len(h.applyCalls) != 0 || h.popupReqs[777011] != 0 {
		t.Fatalf("applies=%v popupReqs=%v, want none (AI skip precedes popup)", h.applyCalls, h.popupReqs)
	}
	if mustMetrics(t, h.db)["apply.skip.score_below_threshold"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

func TestRunLevelAIScoreOverride(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		r := c.Runs["run1"]
		r.AIFilterOverrides.MinScore = 90
		c.Runs["run1"] = r
	})
	h.runner.scoring = scoringVerdict(75, "ok", false, false) // above run default 60, below override 90
	h.searchItems = []string{vacItem(777012, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)
	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (run override 90)", h.applyCalls)
	}
}

func TestAINeedsHumanAndInjectionRouteToReview(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict string
		metric  string
	}{
		{"needs_human", scoringVerdict(50, "gray", true, false), "apply.skip.ai_needs_human"},
		{"injection", scoringVerdict(95, "strong", false, true), "apply.skip.ai_injection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.runner.scoring = tc.verdict
			h.searchItems = []string{vacItem(777013, "Go Dev", 9001, "Corp LLC", "")}
			h.ratings[9001] = hhRating(4.5, 4.7, 100)
			h.run(t)
			if len(h.applyCalls) != 0 {
				t.Fatalf("applies = %v, want none", h.applyCalls)
			}
			if mustMetrics(t, h.db)[tc.metric] != 1 {
				t.Fatalf("metrics = %v", mustMetrics(t, h.db))
			}
			open, _ := h.db.ReviewItemsOpen(t.Context(), "acc1", 10)
			if len(open) == 0 {
				t.Fatalf("want a review item")
			}
		})
	}
}

func TestAIFilterOffSkipsScoring(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Features.MultiApply.AIFilter = false })
	h.searchItems = []string{vacItem(777014, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)
	if len(h.applyCalls) != 1 {
		t.Fatalf("applies = %v, want 1", h.applyCalls)
	}
	if len(h.runner.calls) != 0 {
		t.Fatalf("runner calls = %v, want none (ai_filter off)", h.runner.calls)
	}
}

func TestPopupFeasibilityGates(t *testing.T) {
	t.Run("inconsistencies skip", func(t *testing.T) {
		h := newHarness(t, nil)
		h.searchItems = []string{vacItem(777015, "Go Dev", 9001, "Corp LLC", "")}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.popupByVac[777015] = popupWith(false, true, "ENABLED")
		h.run(t)
		if len(h.applyCalls) != 0 {
			t.Fatalf("applies = %v, want none", h.applyCalls)
		}
		if mustMetrics(t, h.db)["apply.skip.inconsistencies"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
	})
	t.Run("chat disabled skip", func(t *testing.T) {
		h := newHarness(t, nil)
		h.searchItems = []string{vacItem(777016, "Go Dev", 9001, "Corp LLC", "")}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.popupByVac[777016] = popupWith(false, false, "DISABLED")
		h.run(t)
		if len(h.applyCalls) != 0 {
			t.Fatalf("applies = %v, want none", h.applyCalls)
		}
		if mustMetrics(t, h.db)["apply.skip.chat_disabled"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
	})
}

func TestQuotas(t *testing.T) {
	t.Run("run max_per_day stops profile and queues tomorrow", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			r := c.Runs["run1"]
			r.MaxPerDay = 1
			c.Runs["run1"] = r
		})
		h.searchItems = []string{
			vacItem(777017, "Go Dev 1", 9001, "Corp LLC", ""),
			vacItem(777018, "Go Dev 2", 9002, "Other LLC", ""),
		}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.ratings[9002] = hhRating(4.5, 4.7, 100)
		h.run(t)

		if len(h.applyCalls) != 1 || h.applyCalls[0] != 777017 {
			t.Fatalf("applies = %v, want exactly [777017] (quota stop)", h.applyCalls)
		}
		// queue_tomorrow: first the seeded run task, then the resume task.
		seed, ok := h.q.Dequeue(t.Context(), "acc1")
		if !ok || seed.DedupKey != "multiapply:run1" {
			t.Fatalf("seed task = %+v ok=%v", seed, ok)
		}
		tomorrow, ok := h.q.Dequeue(t.Context(), "acc1")
		want := "multiapply:run1:" + featurekit.DayUTC(now.Add(24*time.Hour))
		if !ok || tomorrow.DedupKey != want {
			t.Fatalf("tomorrow task = %+v ok=%v, want dedup %q", tomorrow, ok, want)
		}
		if !tomorrow.Deadline.After(now.Add(12 * time.Hour)) {
			t.Fatalf("tomorrow deadline = %v", tomorrow.Deadline)
		}
		if mustMetrics(t, h.db)["apply.stop.run_quota"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
	})
	t.Run("account daily cap stops profile", func(t *testing.T) {
		h := newHarness(t, func(c *config.Config) {
			a := c.Accounts["acc1"]
			a.DailyApplyCap = 1
			c.Accounts["acc1"] = a
			// A second run on the same account contributes to the account cap.
			c.Runs["run2"] = config.Run{Enabled: true, Account: "acc1", ResumeID: "hash1", Query: "go", MaxPerDay: 10}
		})
		// One application already counted today via run2.
		mustCreateApplication(t, h.db, featurekit.DedupKey("acc1", "hash1", 42), "applied")
		h.searchItems = []string{vacItem(777019, "Go Dev", 9001, "Corp LLC", "")}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.run(t)
		if len(h.applyCalls) != 0 {
			t.Fatalf("applies = %v, want none (account cap)", h.applyCalls)
		}
		if mustMetrics(t, h.db)["apply.stop.account_cap"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
	})
	t.Run("per-employer 2 per day", func(t *testing.T) {
		h := newHarness(t, nil)
		h.searchItems = []string{
			vacItem(777020, "Go Dev A", 9001, "Corp LLC", ""),
			vacItem(777021, "Go Dev B", 9001, "Corp LLC", ""),
			vacItem(777022, "Go Dev C", 9001, "Corp LLC", ""),
		}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.run(t)
		if len(h.applyCalls) != 2 {
			t.Fatalf("applies = %v, want 2 (employer cap)", h.applyCalls)
		}
		if mustMetrics(t, h.db)["apply.skip.employer_daily_cap"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
	})
}

func TestAlreadyRespondedMarkerMarksAppliedSilently(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{vacItem(777023, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(int, int64, string) error {
		return &respErr{status: 400, body: `{"error":"вы уже откликались на эту вакансию"}`}
	}
	h.run(t)

	if len(h.applyCalls) != 1 {
		t.Fatalf("apply calls = %d, want 1", len(h.applyCalls))
	}
	app := mustApplication(t, h.db, "acc1", 777023)
	if app.Status != "applied" {
		t.Fatalf("status = %q, want applied (silent)", app.Status)
	}
	if h.notify.has(webhook.EvApplySent) {
		t.Fatalf("already-responded must stay silent (no webhook)")
	}
	if mustMetrics(t, h.db)["apply.already"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

func TestCaptchaOnApplySolvesAndRetriesOnce(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{vacItem(777024, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(call int, _ int64, _ string) error {
		if call == 1 {
			return &respErr{redirect: "/captcha/image?backurl=x"}
		}
		return nil
	}
	h.run(t)

	if len(h.applyCalls) != 2 {
		t.Fatalf("apply calls = %d, want 2 (captcha + single retry)", len(h.applyCalls))
	}
	if h.chain.solves != 1 {
		t.Fatalf("chain solves = %d, want 1", h.chain.solves)
	}
	if mustMetrics(t, h.db)["apply.ok"] != 1 || mustMetrics(t, h.db)["apply.captcha_retry"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

func TestAmbiguousReconcileThreeOutcomes(t *testing.T) {
	t.Run("found in negotiations → applied", func(t *testing.T) {
		h := newHarness(t, nil)
		h.searchItems = []string{vacItem(777025, "Go Dev", 9001, "Corp LLC", "")}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.applyErr = func(int, int64, string) error {
			return &respErr{status: 502, body: "bad gateway"}
		}
		h.negPages = [][]hhapi.Negotiation{{{ID: "n1", VacancyID: 777025, State: "ACTIVE"}}}
		h.run(t)

		app := mustApplication(t, h.db, "acc1", 777025)
		if app.Status != "applied" {
			t.Fatalf("status = %q, want applied via reconcile", app.Status)
		}
		if mustMetrics(t, h.db)["apply.reconciled"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
		if len(h.applyCalls) != 1 {
			t.Fatalf("apply calls = %d, want 1 (no blind retry)", len(h.applyCalls))
		}
	})
	t.Run("not found → abandoned and safe retry once", func(t *testing.T) {
		h := newHarness(t, nil)
		h.searchItems = []string{vacItem(777026, "Go Dev", 9001, "Corp LLC", "")}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.applyErr = func(call int, _ int64, _ string) error {
			if call == 1 {
				return &respErr{status: 502, body: "bad gateway"}
			}
			return nil // retry lands cleanly
		}
		h.negPages = [][]hhapi.Negotiation{{{ID: "n9", VacancyID: 111111, State: "ACTIVE"}}} // other vacancy
		h.run(t)

		if len(h.applyCalls) != 2 {
			t.Fatalf("apply calls = %d, want 2 (ambiguous → reconcile miss → one retry)", len(h.applyCalls))
		}
		app := mustApplication(t, h.db, "acc1", 777026)
		if app.Status != "applied" {
			t.Fatalf("status = %q, want applied", app.Status)
		}
		if mustMetrics(t, h.db)["apply.ok"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
	})
	t.Run("undeterminable → review item and pending intent", func(t *testing.T) {
		h := newHarness(t, nil)
		h.searchItems = []string{vacItem(777027, "Go Dev", 9001, "Corp LLC", "")}
		h.ratings[9001] = hhRating(4.5, 4.7, 100)
		h.applyErr = func(int, int64, string) error {
			return &respErr{status: 502, body: "bad gateway"}
		}
		h.negFail = true
		h.run(t)

		if len(h.applyCalls) != 1 {
			t.Fatalf("apply calls = %d, want 1 (no retries on undeterminable)", len(h.applyCalls))
		}
		open, _ := h.db.ReviewItemsOpen(t.Context(), "acc1", 10)
		if len(open) != 1 || open[0].Kind != "apply_ambiguous" {
			t.Fatalf("review items = %+v, want apply_ambiguous", open)
		}
		claim, _ := featurekit.BeginIntent(t.Context(), h.db, "apply:acc1:777027", "{}")
		if claim.Step != featurekit.IntentSkipPending {
			t.Fatalf("intent step = %v, want pending (left for human)", claim.Step)
		}
		if mustMetrics(t, h.db)["apply.ambiguous_review"] != 1 {
			t.Fatalf("metrics = %v", mustMetrics(t, h.db))
		}
	})
}

func TestLoggedOutStopsRunAndAccount(t *testing.T) {
	h := newHarness(t, nil)
	mustUpsertAccount(t, h.db, "acc1")
	h.searchItems = []string{vacItem(777028, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.applyErr = func(int, int64, string) error {
		return &respErr{redirect: "/account/login?backurl=x"}
	}
	h.run(t)

	acc, _ := h.db.AccountsGet(t.Context(), "acc1")
	if acc == nil || acc.Status != "logged_out" {
		t.Fatalf("account = %+v, want logged_out", acc)
	}
	if mustMetrics(t, h.db)["apply.logged_out"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

func TestDryRunFullPassNoMutation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.General.DryRun = true })
	h.searchItems = []string{vacItem(777029, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.popupByVac[777029] = popupWith(true, false, "ENABLED")

	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("apply calls = %v, want none in dry-run", h.applyCalls)
	}
	// Full pipeline walk happened: popup was read, AI ran (letter generated).
	if h.popupReqs[777029] == 0 {
		t.Fatalf("popup not inspected in dry-run")
	}
	foundCover := false
	for _, task := range h.runner.calls {
		if task == ai.TaskCoverLetter {
			foundCover = true
		}
	}
	if !foundCover {
		t.Fatalf("cover letter task not exercised in dry-run: %v", h.runner.calls)
	}
	app := mustApplication(t, h.db, "acc1", 777029)
	if app.Status != "dry_run" {
		t.Fatalf("application status = %q, want dry_run", app.Status)
	}
	if h.notify.has(webhook.EvApplySent) {
		t.Fatalf("dry-run must not emit apply_sent")
	}
	if mustMetrics(t, h.db)["apply.dry_run"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
	// No intent claimed: a real run could proceed for this vacancy later.
	claim, _ := featurekit.BeginIntent(t.Context(), h.db, "apply:acc1:777029", "{}")
	if claim.Step != featurekit.IntentProceed {
		t.Fatalf("intent step = %v, want proceed (dry-run claims nothing)", claim.Step)
	}
}

func TestIntentPendingSkip(t *testing.T) {
	h := newHarness(t, nil)
	h.searchItems = []string{vacItem(777030, "Go Dev", 9001, "Corp LLC", "")}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	// Pre-existing pending intent from a crashed earlier run.
	if _, err := h.db.IntentsBegin(t.Context(), "apply:acc1:777030", "pending", "{}"); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	h.run(t)

	if len(h.applyCalls) != 0 {
		t.Fatalf("applies = %v, want none (pending intent)", h.applyCalls)
	}
	if mustMetrics(t, h.db)["apply.skip.intent_pending"] != 1 {
		t.Fatalf("metrics = %v", mustMetrics(t, h.db))
	}
}

func TestPrioritizeOnlineHR(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Features.MultiApply.PrioritizeOnlineHR = true })
	h.searchItems = []string{
		vacItem(777031, "Offline HR", 9001, "Corp LLC", `"employerManager":{"latestActivity":"offline"}`),
		vacItem(777032, "Online HR", 9002, "Online LLC", `"employerManager":{"latestActivity":"online"}`),
	}
	h.ratings[9001] = hhRating(4.5, 4.7, 100)
	h.ratings[9002] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if len(h.applyCalls) != 2 || h.applyCalls[0] != 777032 {
		t.Fatalf("apply order = %v, want online-HR vacancy first", h.applyCalls)
	}
}

func TestPaginationCappedAtFivePages(t *testing.T) {
	h := newHarness(t, nil)
	// 1000 found → 20 pages at 50/page; the same item every page (in-batch dedup).
	h.searchItems = []string{vacItem(777033, "Go Dev", 9001, "Corp LLC", "")}
	h.searchFound = 1000
	h.ratings[9001] = hhRating(4.5, 4.7, 100)

	h.run(t)

	if h.searchReqs != 5 {
		t.Fatalf("search page requests = %d, want 5 (max_pages)", h.searchReqs)
	}
	if len(h.applyCalls) != 1 {
		t.Fatalf("applies = %v, want 1 (same vacancy every page, in-batch dedup)", h.applyCalls)
	}
}

func TestFeatureContract(t *testing.T) {
	h := newHarness(t, nil)
	f := h.feature(t)
	if f.Name() != "multiapply" {
		t.Fatalf("Name = %q", f.Name())
	}
	if !f.Enabled() {
		t.Fatalf("Enabled = false, want true")
	}
	h2 := newHarness(t, func(c *config.Config) { c.Features.MultiApply.Enabled = false })
	if h2.feature(t).Enabled() {
		t.Fatalf("Enabled = true, want false")
	}
	if _, ok := h2.cfg.Runs["run1"]; !ok {
		t.Fatalf("config fixture broken")
	}
	// Disabled run: RunProfile reports an error.
	err := h2.feature(t).RunProfile(t.Context(), "run1")
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("RunProfile err = %v, want ErrDisabled", err)
	}
	// Unknown run id.
	if err := h.feature(t).RunProfile(t.Context(), "nope"); err == nil {
		t.Fatalf("unknown run must error")
	}
}

func TestRegisterTasksSeedsRunsAndSpecs(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Runs["run2"] = config.Run{
			Enabled: true, Account: "acc1", ResumeID: "hash1",
			Windows: []config.Window{{TZ: "UTC", Time: []string{"09:30"}, Days: []string{"mon", "tue"}}},
		}
	})
	f := h.feature(t)
	q, reg := queue.New(), scheduler.NewRegistry()
	if err := f.RegisterTasks(t.Context(), q, reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if _, ok := reg.Get("runs.run2"); !ok {
		t.Fatalf("window spec for run2 not registered: %v", reg.Names())
	}
	if q.Len("acc1") != 2 {
		t.Fatalf("seeded tasks = %d, want 2 (run1+run2)", q.Len("acc1"))
	}
}

// --- small store/test helpers ---

func mustApplication(t *testing.T, db *store.DB, account string, vacancy int64) *store.Application {
	t.Helper()
	app, err := db.ApplicationsGetByDedupKey(t.Context(), featurekit.DedupKey(account, "hash1", vacancy))
	if err != nil || app == nil {
		t.Fatalf("application %d missing: %v", vacancy, err)
	}
	return app
}

func mustCreateApplication(t *testing.T, db *store.DB, key, status string) {
	t.Helper()
	err := db.ApplicationsCreate(t.Context(), &store.Application{
		AccountID: "acc1", RunID: "run1", HhResumeID: "hash1",
		VacancyID: 42, EmployerID: 9, DedupKey: key, Status: status, AppliedAt: ptrTime(now),
	})
	if err != nil {
		t.Fatalf("seed application: %v", err)
	}
}

func mustUpsertAccount(t *testing.T, db *store.DB, id string) {
	t.Helper()
	if err := db.AccountsUpsert(t.Context(), store.Account{ID: id, Status: "ok"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func mustMetrics(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	m, err := db.MetricsSnapshot(t.Context(), featurekit.DayUTC(now))
	if err != nil {
		t.Fatalf("metrics snapshot: %v", err)
	}
	return m
}

func ptrTime(t time.Time) *time.Time { return &t }

// hhRating builds an hhapi.EmployerRating pointer inline for brevity.
func hhRating(all, recent float64, reviews int) *hhapi.EmployerRating {
	return &hhapi.EmployerRating{EmployerID: 1, ScoreAll: all, ScoreRecent: recent, ReviewsCount: reviews, FetchedAt: time.Now()}
}

// letterEscape percent-encodes the letter the way url.Values.Encode does
// (space → "+", uppercase hex) for form-body substring checks.
func letterEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' {
			b.WriteByte('+')
		} else if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
