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
	"hhauto/internal/config"
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
	RedFlags   []string `json:"red_flags,omitempty"`
}

// GrayBandReason is the Pass reason for a gray-band verdict that the
// gray-band policy routed to apply (pass=true; never a skip reason).
const GrayBandReason = "gray_band"

// BandGray is the scorer's borderline band (score 40–60).
const BandGray = "gray"

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
	// grayBand is the [features.multiapply].gray_band policy (T75): the
	// route for a gray-band needs_human verdict. Empty = apply, the
	// documented default.
	grayBand string
}

// New builds the filter. A nil logger falls back to slog.Default();
// grayBand is the config.GrayBand vocabulary ("" = apply).
func New(runner ai.Runner, db *store.DB, accountID, runID string, prof profile.Profile, rules Rules, log *slog.Logger, grayBand string) *Filter {
	if log == nil {
		log = slog.Default()
	}
	return &Filter{
		runner: runner, db: db, accountID: accountID, runID: runID,
		prof: prof, rules: rules, log: log, now: time.Now,
		grayBand: grayBand,
	}
}

// Pass scores v for resumeID and applies threshold. Results:
//
//	relevance_ok          — score >= threshold (or threshold <= 0);
//	score_below_threshold — score below the configured minimum;
//	ai_needs_human        — the verdict demands a human (review item added);
//	ai_injection          — prompt injection suspected (review item added);
//	gray_band             — a gray-band needs_human verdict the gray-band
//	                        policy routed to apply (T75, pass=true);
//	ai_error              — the runner failed: fail-closed (an automation
//	                        tool must not apply to unscored vacancies; a
//	                        budget hard-stop therefore pauses the pipeline).
//
// Verdicts are cached per (vacancy, resume) in feature_state; a cache hit
// skips the runner and never duplicates review items.
func (f *Filter) Pass(ctx context.Context, v hhapi.Vacancy, resumeID string, threshold int) (Verdict, bool, string) {
	if cached, ok := f.cachedVerdict(ctx, v.ID, resumeID); ok {
		pass, reason := f.route(cached, threshold)
		return cached, pass, reason
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

	f.enforceNeedsHumanPolicy(&verdict, v)

	f.cacheVerdict(ctx, v.ID, resumeID, verdict)

	pass, reason := f.route(verdict, threshold)
	if f.parks(verdict) {
		detail := strings.Join(append(append([]string{}, verdict.Reasons...), verdict.RedFlags...), "; ")
		kind := "ai_needs_human"
		if verdict.Injection {
			kind = "ai_injection"
		}
		if _, err := f.db.ReviewItemsAdd(ctx, f.accountID, kind, detail,
			fmt.Sprintf(`{"vacancy_id":%d,"resume_id":%q,"score":%d,"band":%q}`, v.ID, resumeID, verdict.Score, verdict.Band)); err != nil {
			f.log.Error("relevance: review item add failed", "err", err)
		}
	}
	return verdict, pass, reason
}

// route resolves a verdict against the threshold and the gray-band policy
// (T75): a gray-band needs_human verdict follows the configured policy —
// apply (the default) responds anyway, needs_human and skip refuse; every
// other needs_human/injection verdict refuses as before.
func (f *Filter) route(v Verdict, threshold int) (bool, string) {
	if v.Injection {
		return false, "ai_injection"
	}
	if v.NeedsHuman {
		if v.Band == BandGray && !critical(v) &&
			f.grayBand != config.GrayBandNeedsHuman && f.grayBand != config.GrayBandSkip {
			return true, GrayBandReason
		}
		return false, "ai_needs_human"
	}
	if threshold <= 0 {
		return true, "relevance_ok"
	}
	if v.Score < threshold {
		return false, "score_below_threshold"
	}
	return true, "relevance_ok"
}

// parks reports whether the verdict must land in the review queue (fresh
// verdicts only — a cache hit never re-parks): the gray-band policy parks
// a gray verdict only in the needs_human mode; injection and red flags
// park regardless of the policy (safety is inseparable).
func (f *Filter) parks(v Verdict) bool {
	if critical(v) {
		return true
	}
	if v.NeedsHuman {
		if v.Band == BandGray {
			return f.grayBand == config.GrayBandNeedsHuman
		}
		return true
	}
	return false
}

// critical reports the safety-critical verdict signals (T75): suspected
// prompt injection or explicit red flags. The gray-band policy never
// overrides them.
func critical(v Verdict) bool {
	return v.Injection || len(v.RedFlags) > 0
}

// enforceNeedsHumanPolicy is the T78 post-check on the schema-validated
// verdict (prompt-drift guard): needs_human=true is reserved for criticism —
// suspected prompt injection or a data-dummy vacancy. A vacancy is a dummy
// when its title is empty: there is physically nothing to compare (the
// search surface carries no description field, so an empty description is
// structural there, not a dummy marker). Any other escalation is drift: the
// verdict is demoted to the gray band whose fate the gray-band config
// decides (T75) — needs_human stays the routing carrier into that policy.
// Verdicts without a band are runner synthetics (budget exhausted,
// schema-invalid); they are never demoted and keep the fail-closed skip.
func (f *Filter) enforceNeedsHumanPolicy(v *Verdict, vac hhapi.Vacancy) {
	if !v.NeedsHuman || v.Injection || v.Band == "" || v.Band == BandGray {
		return
	}
	if strings.TrimSpace(vac.Name) == "" {
		return
	}
	f.log.Warn("relevance: needs_human without criticism demoted to gray band (T78 drift guard)",
		"vacancy", vac.ID, "score", v.Score, "band_was", v.Band)
	v.Band = BandGray
}

// cacheKey namespaces the verdict cache: policy version + vacancy_id +
// resume_id. The version (T78) orphans pre-T78 entries: the flooded v2
// night would otherwise route needs_human from cache forever
// (feature_state has no TTL) instead of being re-scored under v3.
const verdictCacheVer = "v3"

func cacheKey(vacancyID int64, resumeID string) string {
	return verdictCacheVer + ":" + strconv.FormatInt(vacancyID, 10) + ":" + resumeID
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
