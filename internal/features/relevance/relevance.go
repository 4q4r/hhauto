// Package relevance implements the WF2-F6 AI relevance filter: vacancy
// scoring through the ai.Runner (vacancy_scoring task) with a threshold,
// needs_human/injection routing into the review queue and a per
// (vacancy, resume) verdict cache in feature_state.
package relevance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/features/featurekit"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
)

// feature is the feature_state namespace for the verdict cache.
const feature = "relevance"

// Verdict is the parsed vacancy_scoring outcome (subset the filter acts on).
type Verdict struct {
	Score      int      `json:"score"`
	Band       string   `json:"band"`
	NeedsHuman bool     `json:"needs_human"`
	Injection  bool     `json:"injection_suspected"`
	Confidence float64  `json:"confidence"`
	Reasons    []string `json:"reasons,omitempty"`
}

// Rules carries the run-level instruction context for the scorer.
type Rules struct {
	ExcludeWords      []string `json:"exclude_words,omitempty"`
	TargetRoles       []string `json:"target_roles,omitempty"`
	ExtraInstructions string   `json:"extra_instructions,omitempty"`
}

// scoringInput is the trusted-JSON input handed to the runner (the guard
// wraps it in untrusted delimiters inside the ai package).
type scoringInput struct {
	Vacancy      hhapi.Vacancy     `json:"vacancy"`
	ResumeDigest string            `json:"resume_digest"`
	ProfileText  string            `json:"profile_text"`
	Facts        map[string]string `json:"facts"`
	Rules        Rules             `json:"rules"`
}

// Filter scores vacancies for one account/run against a threshold.
type Filter struct {
	runner    ai.Runner
	db        *store.DB
	accountID string
	runID     string
	prof      profile.Profile
	rules     Rules
	log       *slog.Logger
	now       func() time.Time
}

// New builds the filter. A nil logger falls back to slog.Default().
func New(runner ai.Runner, db *store.DB, accountID, runID string, prof profile.Profile, rules Rules, log *slog.Logger) *Filter {
	if log == nil {
		log = slog.Default()
	}
	return &Filter{
		runner: runner, db: db, accountID: accountID, runID: runID,
		prof: prof, rules: rules, log: log, now: time.Now,
	}
}

// Pass scores v for resumeID and applies threshold. Results:
//
//	relevance_ok          — score >= threshold (or threshold <= 0);
//	score_below_threshold — score below the configured minimum;
//	ai_needs_human        — the verdict demands a human (review item added);
//	ai_injection          — prompt injection suspected (review item added);
//	ai_error              — the runner failed: fail-closed (an automation
//	                        tool must not apply to unscored vacancies; a
//	                        budget hard-stop therefore pauses the pipeline).
//
// Verdicts are cached per (vacancy, resume) in feature_state; a cache hit
// skips the runner and never duplicates review items.
func (f *Filter) Pass(ctx context.Context, v hhapi.Vacancy, resumeID string, threshold int) (Verdict, bool, string) {
	if cached, ok := f.cachedVerdict(ctx, v.ID, resumeID); ok {
		return cached, decide(cached, threshold), cachedReason(cached, threshold)
	}

	raw, _, err := f.runner.Run(ctx, f.accountID, "vacancy", strconv.FormatInt(v.ID, 10), f.runID,
		ai.TaskVacancyScoring, scoringInput{
			Vacancy:      v,
			ResumeDigest: f.prof.ResumeDigest,
			ProfileText:  f.prof.ProfileText,
			Facts:        f.prof.Facts,
			Rules:        f.rules,
		})
	if err != nil {
		f.log.Warn("relevance: runner failed; skipping vacancy (fail-closed)",
			"vacancy", v.ID, "err", err)
		_ = f.db.MetricsIncr(ctx, "relevance.ai_error", featurekit.DayUTC(f.now()))
		return Verdict{}, false, "ai_error"
	}

	var verdict Verdict
	if err := json.Unmarshal(raw, &verdict); err != nil {
		f.log.Warn("relevance: verdict decode failed; skipping vacancy", "vacancy", v.ID, "err", err)
		return Verdict{}, false, "ai_error"
	}

	f.cacheVerdict(ctx, v.ID, resumeID, verdict)

	if verdict.NeedsHuman || verdict.Injection {
		kind, reason := "ai_needs_human", "ai_needs_human"
		if verdict.Injection {
			kind, reason = "ai_injection", "ai_injection"
		}
		detail := ""
		if len(verdict.Reasons) > 0 {
			detail = strings.Join(verdict.Reasons, "; ")
		}
		if _, err := f.db.ReviewItemsAdd(ctx, f.accountID, kind, detail,
			fmt.Sprintf(`{"vacancy_id":%d,"resume_id":%q,"score":%d,"band":%q}`, v.ID, resumeID, verdict.Score, verdict.Band)); err != nil {
			f.log.Error("relevance: review item add failed", "err", err)
		}
		return verdict, false, reason
	}
	return verdict, decide(verdict, threshold), cachedReason(verdict, threshold)
}

// decide applies the threshold to a verdict.
func decide(v Verdict, threshold int) bool {
	if v.NeedsHuman || v.Injection {
		return false
	}
	if threshold <= 0 {
		return true
	}
	return v.Score >= threshold
}

func cachedReason(v Verdict, threshold int) string {
	if v.NeedsHuman {
		return "ai_needs_human"
	}
	if v.Injection {
		return "ai_injection"
	}
	if threshold > 0 && v.Score < threshold {
		return "score_below_threshold"
	}
	return "relevance_ok"
}

// cacheKey namespaces the verdict cache: vacancy_id + resume_id.
func cacheKey(vacancyID int64, resumeID string) string {
	return strconv.FormatInt(vacancyID, 10) + ":" + resumeID
}

func (f *Filter) cachedVerdict(ctx context.Context, vacancyID int64, resumeID string) (Verdict, bool) {
	raw, ok, err := f.db.FeatureStateGet(ctx, feature, f.accountID, cacheKey(vacancyID, resumeID))
	if err != nil || !ok {
		return Verdict{}, false
	}
	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, false
	}
	return v, true
}

func (f *Filter) cacheVerdict(ctx context.Context, vacancyID int64, resumeID string, v Verdict) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := f.db.FeatureStateSet(ctx, feature, f.accountID, cacheKey(vacancyID, resumeID), string(b)); err != nil {
		f.log.Warn("relevance: cache write failed", "vacancy", vacancyID, "err", err)
	}
}

// RolesFromFacts extracts the target-role list from the profile facts
// (comma-separated fact "target_roles"; empty when absent).
func RolesFromFacts(prof profile.Profile) []string {
	raw := strings.TrimSpace(prof.Facts["target_roles"])
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}
