package autoclone

// The T29 improve pipeline: plan (per-field AI proposals on the clone's
// facts) → apply (one builder update POST with the whole mutated split,
// per-field retry ≤1) → artifact (было/стало per field, stored on the
// lineage row). The invariants live here: only tracked clones are ever
// mutated (ErrNotClone otherwise), needs_human/always_ask verdicts park
// for review instead of applying, dry-run plans without posting.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/review"
	"hhauto/internal/store"
)

// ErrNotClone guards the base-resume invariant: the hash is not a tracked
// clone of the given parent, so it is never mutated.
var ErrNotClone = errors.New("autoclone: target is not a tracked clone (base resumes are never mutated)")

// Field diff statuses (DiffArtifact.Fields[].Status).
const (
	StatusImproved         = "improved"
	StatusUnchanged        = "unchanged"
	StatusNeedsHuman       = "needs_human"
	StatusAlwaysAsk        = "always_ask"
	StatusValidationFailed = "validation_failed"
	StatusError            = "error"
	StatusSkipped          = "skipped"
)

// Review kinds the improver parks for human attention.
const (
	ReviewKindImproveNeedsHuman = "autoclone_improve_needs_human"
	ReviewKindImproveAlwaysAsk  = "autoclone_improve_always_ask"
)

// FieldDiff is the было/стало audit record of one field target.
type FieldDiff struct {
	Field    string          `json:"field"`               // spec id ("title", "experience_description", ...)
	Locator  string          `json:"locator"`             // concrete payload path ("resume.title", "profile.experience.0.description")
	Status   string          `json:"status"`              // improved | unchanged | needs_human | always_ask | validation_failed | error | skipped
	Before   json.RawMessage `json:"before,omitempty"`    // current value (unwrapped scalar)
	After    json.RawMessage `json:"after,omitempty"`     // proposed value (applied or review-pending)
	Reason   string          `json:"reason,omitempty"`    // skip/failure reason
	ReviewID int64           `json:"review_id,omitempty"` // parked review item
}

// DiffArtifact is the whole-improvement audit artifact (schema-versioned;
// stored on the clone's lineage row and returned to the caller for logs).
type DiffArtifact struct {
	Schema     string      `json:"schema"`
	CloneHash  string      `json:"clone_hash"`
	ParentHash string      `json:"parent_hash"`
	CreatedAt  time.Time   `json:"created_at"`
	DryRun     bool        `json:"dry_run"`
	Applied    bool        `json:"applied"`
	Fields     []FieldDiff `json:"fields"`
}

// artifactSchemaV1 pins the diff artifact shape consumers parse.
const artifactSchemaV1 = "hhauto.improve-diff/v1"

// defaultFieldCallPause spaces consecutive per-field AI calls: back-to-back
// provider requests trip the Z.AI HTTP 429 rate limit and mark every field
// error (T41: the owner asked for strictly sequential, spaced requests).
const defaultFieldCallPause = 2 * time.Second

// Options wires the improver.
type Options struct {
	API      hhapi.ResumeContentEditor // content read/update surface
	Runner   ai.FieldPromptRunner      // AI gateway (required when enabled)
	DB       *store.DB                 // lineage invariant + diff persistence
	Review   review.Reviewer           // review parking (optional in tests)
	Registry *PromptRegistry           // per-field prompts
	// Editor applies the plan through the real editor pages in a browser
	// (T30): the /profile/** POST family is edge-gated for off-browser
	// TLS clients, so the browser path is the working apply. nil = the
	// builder-update API path (its live degradation: parked statuses).
	Editor EditorRunner
	Cfg    config.AutoCloneImprove
	DryRun bool
	Now    func() time.Time
	Log    *slog.Logger
	// ProfileFor loads the trusted candidate context (profile.md text
	// plus stored owner Q&A rules); nil or a load failure degrades to an
	// empty context — the improve run itself never fails on it.
	ProfileFor func(ctx context.Context, accountID string) (profile.Profile, error)
	// BasePrompt is the resume_improve task's base system prompt (builtin
	// v5 or the [ai.tasks] TOML override) the per-field prompt composes
	// on top of (the RunWithSystem contract); empty = field prompt only.
	BasePrompt string
	// InterCallPause is the pause between consecutive per-field AI calls
	// (rate-limit spacing); zero = defaultFieldCallPause (2s). Tests
	// shrink it to keep the suite fast.
	InterCallPause time.Duration
}

// pending is one planned mutation awaiting the apply step.
type pending struct {
	spec        FieldSpec
	item        int
	locator     string
	value       json.RawMessage
	skillLevels map[string]string // key_skills only: verdict levels by skill name (T57)
}

// Improver improves one fresh clone per call.
type Improver struct {
	opt Options
}

// New validates the wiring (fail fast — a broken improve section must stop
// the process, not degrade into skipped fields at clone time).
func New(o Options) (*Improver, error) {
	if !o.Cfg.Enabled {
		return nil, errors.New("autoclone: improve is disabled")
	}
	if o.API == nil {
		return nil, errors.New("autoclone: improve requires the resume content editor")
	}
	if o.Runner == nil {
		return nil, errors.New("autoclone: improve requires the AI runner")
	}
	if o.DB == nil {
		return nil, errors.New("autoclone: improve requires the store (lineage invariant)")
	}
	if o.Registry == nil {
		return nil, errors.New("autoclone: improve requires the prompt registry")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.InterCallPause <= 0 {
		o.InterCallPause = defaultFieldCallPause
	}
	return &Improver{opt: o}, nil
}

// ImproveClone runs the improve flow for one verified clone. Per-field
// failures (AI error, needs_human, validation) degrade to skipped fields
// recorded in the artifact; only invariant/read failures return an error.
func (im *Improver) ImproveClone(ctx context.Context, accountID, parentHash, cloneHash string) (*DiffArtifact, error) {
	im.opt.Log.Info("autoclone: improve clone start",
		"account", accountID, "parent", parentHash, "hash", cloneHash, "dry_run", im.opt.DryRun,
		"fields", strings.Join(im.opt.Cfg.Fields, ","))
	if err := im.requireCloneLineage(ctx, accountID, parentHash, cloneHash); err != nil {
		return nil, err
	}
	live, err := im.opt.API.ResumeContent(ctx, cloneHash)
	if err != nil {
		return nil, fmt.Errorf("autoclone: live content: %w", err)
	}
	im.opt.Log.Info("autoclone: improve live content read", "hash", cloneHash)

	artifact := &DiffArtifact{
		Schema:     artifactSchemaV1,
		CloneHash:  cloneHash,
		ParentHash: parentHash,
		CreatedAt:  im.opt.Now().UTC(),
		DryRun:     im.opt.DryRun,
	}
	mutated := live.Clone()
	pendings := im.planFields(ctx, accountID, cloneHash, live, mutated, artifact)
	if im.opt.DryRun || len(pendings) == 0 {
		im.opt.Log.Info("autoclone: improve clone done (no apply)",
			"hash", cloneHash, "dry_run", im.opt.DryRun, "planned_fields", len(artifact.Fields), "pending", len(pendings))
		im.persist(ctx, cloneHash, artifact)
		return artifact, nil
	}

	if im.opt.Editor != nil {
		if aerr := im.applyPendingViaEditor(ctx, accountID, cloneHash, live, pendings, artifact); aerr != nil {
			return nil, aerr
		}
	} else {
		im.applyPending(ctx, cloneHash, live, mutated, pendings, artifact)
	}
	im.applyKeySkillLevels(ctx, cloneHash, pendings, artifact)
	artifact.Applied = artifactHasStatus(artifact, StatusImproved)
	im.opt.Log.Info("autoclone: improve clone done",
		"hash", cloneHash, "planned_fields", len(artifact.Fields), "pending", len(pendings), "applied", artifact.Applied)
	im.persist(ctx, cloneHash, artifact)
	return artifact, nil
}

// applyPendingViaEditor applies the plan through the browser editor
// runner (T30). A degradation-class failure (Chrome missing, cookies
// unavailable, browser start failure) parks the whole plan for manual
// apply — the review semantics — and flips the planned fields to error;
// per-change failures land as StatusError on their own fields. Invariant
// failures (not a tracked clone, read failures) return an error.
func (im *Improver) applyPendingViaEditor(ctx context.Context, accountID, cloneHash string,
	live *hhapi.ResumeContent, pendings []pending, artifact *DiffArtifact,
) error {
	plan := make([]FieldChange, 0, len(pendings))
	for _, p := range pendings {
		plan = append(plan, FieldChange{
			Field: p.spec.ID, Locator: p.locator, Screen: editorScreensFor(p.spec.ID, live),
			Kind: p.spec.Kind, Label: p.spec.Label, Item: p.item,
			Value: editorNaturalValue(p.spec, p.value),
		})
	}
	res, err := im.opt.Editor.ApplyFieldPlan(ctx, cloneHash, plan)
	if err != nil {
		if !errors.Is(err, ErrEditorUnavailable) && !errors.Is(err, ErrChromeMissingEditor) {
			return err
		}
		for i := range artifact.Fields {
			if artifact.Fields[i].Status != StatusImproved {
				continue
			}
			artifact.Fields[i].Status = StatusError
			artifact.Fields[i].Reason = err.Error()
		}
		im.parkReview(ctx, accountID, ReviewKindImproveNeedsHuman,
			"improve plan parked: the browser editor is unavailable, apply manually ("+err.Error()+")",
			reviewPayload(cloneHash, "", json.RawMessage(reviewPlannedValues(plan))))
		return nil
	}
	failed := make(map[string]string, len(res.Failed))
	for _, f := range res.Failed {
		failed[f.Field] = f.Err
	}
	for i := range artifact.Fields {
		f := &artifact.Fields[i]
		if f.Status != StatusImproved {
			continue
		}
		if reason, bad := failed[f.Locator]; bad {
			f.Status, f.Reason = StatusError, reason
		}
	}
	return nil
}

// reviewPlannedValues renders the parked plan for the review payload.
func reviewPlannedValues(plan []FieldChange) string {
	b, _ := json.Marshal(map[string]any{"planned": plan})
	return string(b)
}

// requireCloneLineage enforces the base-resume invariant: only a tracked
// clone of THIS parent for THIS account is ever mutated.
func (im *Improver) requireCloneLineage(ctx context.Context, accountID, parentHash, cloneHash string) error {
	row, err := im.opt.DB.ResumeClonesGet(ctx, cloneHash)
	if err != nil {
		return fmt.Errorf("autoclone: lineage lookup: %w", err)
	}
	if row == nil || row.ParentHash != parentHash || row.AccountID != accountID {
		return fmt.Errorf("%w: hash %q (parent %q, account %q)", ErrNotClone, cloneHash, parentHash, accountID)
	}
	return nil
}

// planFields runs one AI call per field target (experience fields expand
// per item) and writes accepted values into mutated. It returns the
// mutations awaiting the apply step.
func (im *Improver) planFields(ctx context.Context, accountID, cloneHash string,
	live, mutated *hhapi.ResumeContent, artifact *DiffArtifact) []pending {
	candidateCtx := im.candidateContext(ctx, accountID)
	called := false // the previous target made its AI call
	var pendings []pending
	im.opt.Log.Info("autoclone: improve plan start",
		"account", accountID, "hash", cloneHash, "configured_fields", len(im.opt.Cfg.Fields))
	for _, id := range im.opt.Cfg.Fields {
		spec, ok := Spec(id)
		if !ok || im.opt.Registry.Prompt(id) == "" {
			im.opt.Log.Info("autoclone: improve field not promptable; skipping", "field", id)
			continue // not promptable — nothing to plan
		}
		for _, tg := range expandTargets(spec, live) {
			diff := FieldDiff{Field: id, Locator: tg.locator, Before: tg.current}
			if isEmptyValue(tg.current) {
				diff.Status = StatusSkipped
				diff.Reason = "empty current value: nothing to ground the improvement on"
				artifact.Fields = append(artifact.Fields, diff)
				im.opt.Log.Info("autoclone: improve field skipped (empty current value)",
					"field", id, "locator", tg.locator)
				continue
			}
			if called {
				// T41: strictly sequential field calls — the 2s pause
				// keeps consecutive provider requests off the 429 wall.
				time.Sleep(im.opt.InterCallPause)
			}
			called = true
			started := time.Now()
			im.opt.Log.Info("autoclone: improve field asking AI",
				"field", id, "locator", tg.locator, "label", spec.Label)
			proposed, status, reason, reviewID, verdictLevels := im.propose(ctx, accountID, cloneHash, spec, tg, live, candidateCtx)
			diff.Status, diff.Reason, diff.ReviewID = status, reason, reviewID
			if proposed != nil {
				diff.After = displayValue(spec, proposed)
			}
			im.opt.Log.Info("autoclone: improve field verdict",
				"field", id, "locator", tg.locator, "status", status,
				"reason", reason, "elapsed", time.Since(started).Round(time.Second).String())
			if status == StatusImproved {
				if werr := writeValue(spec, mutated, tg.item, proposed); werr != nil {
					diff.Status, diff.Reason = StatusError, werr.Error()
				} else {
					pendings = append(pendings, pending{spec: spec, item: tg.item, locator: tg.locator, value: proposed, skillLevels: verdictLevels})
				}
			}
			// always_ask proposals park for review; nothing applies.
			artifact.Fields = append(artifact.Fields, diff)
		}
	}
	im.opt.Log.Info("autoclone: improve plan complete",
		"account", accountID, "hash", cloneHash,
		"planned_fields", len(artifact.Fields), "pending_apply", len(pendings))
	return pendings
}

// applyPending posts the mutated split and runs the per-field retry: hh
// validates field by field, so the non-named fields land on a rejected
// POST; each named field gets exactly one retry with a deterministic fix,
// then it is skipped (and rolled back in the payload).
func (im *Improver) applyPending(ctx context.Context, cloneHash string,
	live, payload *hhapi.ResumeContent, pendings []pending, artifact *DiffArtifact) {
	err := im.opt.API.UpdateResumeContent(ctx, cloneHash, payload, pendingKeys(pendings))
	if err == nil {
		return
	}
	var ve *hhapi.ResumeUpdateValidationError
	if !errors.As(err, &ve) {
		// Transport-level failure: nothing is verified; park everything.
		for i := range artifact.Fields {
			if artifact.Fields[i].Status == StatusImproved {
				artifact.Fields[i].Status = StatusError
				artifact.Fields[i].Reason = err.Error()
			}
		}
		return
	}
	retry := im.retryRejectedFields(ctx, cloneHash, live, payload, pendings, ve.Codes, artifact)
	if len(retry) == 0 {
		return
	}
	if rerr := im.opt.API.UpdateResumeContent(ctx, cloneHash, payload, pendingKeys(retry)); rerr != nil {
		im.settleRetryOutcome(artifact, retry, rerr)
	}
}

// pendingKeys maps the plan's pending fields to their content keys
// ("side.key") — the apply/verify scope of the update call.
func pendingKeys(pendings []pending) []string {
	keys := make([]string, 0, len(pendings))
	for _, p := range pendings {
		keys = append(keys, p.spec.Side+"."+p.spec.Key)
	}
	return keys
}

// retryRejectedFields handles the first validation verdict: fixes the
// named fields once (rollback otherwise) and returns the retry set.
func (im *Improver) retryRejectedFields(ctx context.Context, cloneHash string,
	live, payload *hhapi.ResumeContent, pendings []pending, codes []string, artifact *DiffArtifact) []pending {
	retriable := map[string]bool{}
	for _, code := range codes {
		key := code
		if i := strings.IndexByte(code, ':'); i >= 0 {
			key = code[:i]
		}
		for _, p := range pendings {
			if p.spec.Key == key {
				retriable[p.locator] = true
			}
		}
	}
	var retry []pending
	for _, p := range pendings {
		i := locateField(artifact, p.locator)
		if !retriable[p.locator] {
			continue // landed fine (hh rejected other fields)
		}
		fixed, ok := fixValidationValue(&p.spec, live, p.value, codes)
		if ok {
			if werr := writeValue(p.spec, payload, p.item, fixed); werr == nil {
				p.value = fixed
				retry = append(retry, p)
				artifact.Fields[i].After = displayValue(p.spec, fixed)
				continue
			}
		}
		artifact.Fields[i].Status = StatusValidationFailed
		artifact.Fields[i].Reason = strings.Join(codes, ",")
		// The rejected value never landed: roll the payload back to the
		// live value for this field so the retry POST is exact.
		if orig := restoreRaw(live, p); orig != nil {
			if werr := writeValue(p.spec, payload, p.item, orig); werr != nil {
				im.opt.Log.Warn("autoclone: rollback write failed", "locator", p.locator, "err", werr)
			}
		}
	}
	_ = ctx
	_ = cloneHash
	return retry
}

// settleRetryOutcome marks the retried fields by the outcome of the
// second POST.
func (im *Improver) settleRetryOutcome(artifact *DiffArtifact, retry []pending, err error) {
	var rve *hhapi.ResumeUpdateValidationError
	if errors.As(err, &rve) {
		for _, p := range retry {
			i := locateField(artifact, p.locator)
			artifact.Fields[i].Status = StatusValidationFailed
			artifact.Fields[i].Reason = strings.Join(rve.Codes, ",")
		}
		return
	}
	for _, p := range retry {
		i := locateField(artifact, p.locator)
		artifact.Fields[i].Status = StatusError
		artifact.Fields[i].Reason = err.Error()
	}
}

// candidateContext loads the trusted candidate context once per run and
// renders the profile sections (profile text plus stored owner Q&A; the
// resume digest stays out — the live resume facts already ride per
// field). Absence degrades to "" — never a failed run.
func (im *Improver) candidateContext(ctx context.Context, accountID string) string {
	if im.opt.ProfileFor == nil {
		return ""
	}
	p, err := im.opt.ProfileFor(ctx, accountID)
	if err != nil {
		im.opt.Log.Warn("autoclone: profile load failed; improving without candidate context",
			"account", accountID, "err", err)
		return ""
	}
	return profile.RenderForPrompt(p, profile.SectionProfile, profile.SectionFacts)
}

// fieldPrompt resolves the field's system prompt with the candidate
// context substituted into {{candidate_context}} (an absent profile
// substitutes an explicit note — never a dangling template marker) and
// composes it on top of the base prompt (base first, field instructions
// after — the RunWithSystem contract).
func (im *Improver) fieldPrompt(fieldID, candidateCtx string) string {
	subst := strings.TrimSpace(candidateCtx)
	if subst == "" {
		subst = "(профиль не предоставлен)"
	}
	field := strings.ReplaceAll(im.opt.Registry.Prompt(fieldID), "{{candidate_context}}", subst)
	if base := strings.TrimSpace(im.opt.BasePrompt); base != "" {
		return base + "\n\n" + field
	}
	return field
}

// propose asks the AI for one field target's new value and returns the
// validated payload value (nil when nothing may apply). For key_skills the
// verdict's optional key_skill_levels ride along (T57; nil otherwise).
func (im *Improver) propose(ctx context.Context, accountID, cloneHash string, spec FieldSpec,
	tg target, live *hhapi.ResumeContent, candidateCtx string) (value json.RawMessage, status, reason string, reviewID int64, skillLevels map[string]string) {
	verdict, _, err := im.opt.Runner.RunWithSystem(ctx, accountID, "resume", cloneHash, "",
		ai.TaskResumeImprove, im.fieldPrompt(spec.ID, candidateCtx), ai.ImproveInput{
			Field:            spec.ID,
			FieldLabel:       spec.Label,
			CurrentValue:     tg.current,
			TrustedFacts:     factsFor(spec, live, tg.item),
			CandidateContext: candidateCtx,
		})
	if err != nil {
		return nil, StatusError, err.Error(), 0, nil
	}
	out, ok := decodeImproveVerdict(verdict)
	if !ok {
		return nil, StatusError, "decode verdict failed", 0, nil
	}
	if out.NeedsHuman {
		// The AI proposal rides the parked item (best effort): it is the
		// "стало" the human decides on, and the value a later resolve
		// apply re-validates against the live resume (T54).
		proposal, _ := slotValue(spec, verdict)
		id := im.parkReview(ctx, accountID, ReviewKindImproveNeedsHuman,
			"improve verdict needs human: "+out.Reason, needReviewPayload(cloneHash, spec, tg, proposal))
		return nil, StatusNeedsHuman, out.Reason, id, nil
	}
	if out.Unchanged {
		return nil, StatusUnchanged, "", 0, nil
	}
	proposed, err := slotValue(spec, verdict)
	if err != nil {
		return nil, StatusError, err.Error(), 0, nil
	}
	validated, verr := ValidateValueAt(spec, live, tg.item, proposed)
	if verr != nil {
		return nil, StatusValidationFailed, verr.Error(), 0, nil
	}
	if rawEqualT(validated, tg.raw) {
		return nil, StatusUnchanged, "", 0, nil
	}
	// always_ask: the value is good but a human approves it first.
	for _, f := range im.opt.Cfg.AlwaysAsk {
		if f == spec.ID {
			id := im.parkReview(ctx, accountID, ReviewKindImproveAlwaysAsk,
				"improved "+spec.Label+" waits for owner approval", reviewPayload(cloneHash, spec.ID, validated))
			return validated, StatusAlwaysAsk, "", id, nil
		}
	}
	var levels map[string]string
	if spec.ID == "key_skills" {
		levels = keySkillVerdictLevels(verdict)
	}
	return validated, StatusImproved, "", 0, levels
}

// keySkillLeveler is the optional API surface of the T57 levels step. The
// real content surface (*hhapi.Client and the CLI lineageEditor wrapper)
// implements it; test fakes may substitute their own or omit it (the step
// then degrades to a loud no-op — the content apply is unaffected).
type keySkillLeveler interface {
	UserSkills(ctx context.Context, resumeHash string) ([]hhapi.UserSkill, error)
	SetKeySkillLevels(ctx context.Context, resumeHash string, sets []hhapi.KeySkillLevel) error
}

// applyKeySkillLevels runs the T57 levels step after the content apply: no
// resume key skill may stay on «Уровень не указан». Skills without a level
// get the verdict level (default Средний); leveled skills are never sent
// (byte-for-byte preservation). The UserSkills re-read is the verify gate.
// A step failure flips the key_skills artifact field (when improved this
// run) to error — it never fails the run: the content apply already landed.
func (im *Improver) applyKeySkillLevels(ctx context.Context, cloneHash string, pendings []pending, artifact *DiffArtifact) {
	lvl, ok := im.opt.API.(keySkillLeveler)
	if !ok {
		im.opt.Log.Warn("autoclone: key-skill levels step skipped: the content surface does not implement the levels contract")
		return
	}
	verdictLevels := map[string]string{}
	for _, p := range pendings {
		for k, v := range p.skillLevels {
			verdictLevels[k] = v
		}
	}
	skills, err := lvl.UserSkills(ctx, cloneHash)
	if err != nil {
		im.failKeySkillLevels(artifact, "read skill levels: "+err.Error())
		return
	}
	sets := keySkillLevelSets(skills, verdictLevels)
	if len(sets) == 0 {
		return // every key skill already carries a level
	}
	if err := lvl.SetKeySkillLevels(ctx, cloneHash, sets); err != nil {
		im.failKeySkillLevels(artifact, "set skill levels: "+err.Error())
		return
	}
	got, err := lvl.UserSkills(ctx, cloneHash)
	if err != nil {
		im.failKeySkillLevels(artifact, "verify skill levels: "+err.Error())
		return
	}
	byID := make(map[int64]int64, len(got))
	for _, s := range got {
		byID[s.ID] = s.LevelID
	}
	var missing []int64
	for _, s := range sets {
		if byID[s.SkillID] != s.LevelID {
			missing = append(missing, s.SkillID)
		}
	}
	if len(missing) > 0 {
		im.failKeySkillLevels(artifact, fmt.Sprintf("verify skill levels: skills %v still on «Уровень не указан»", missing))
	}
}

// failKeySkillLevels attributes a levels-step failure to the key_skills
// artifact field when this run improved it; without such a field the log
// carries the failure (nothing else to mark).
func (im *Improver) failKeySkillLevels(artifact *DiffArtifact, reason string) {
	for i := range artifact.Fields {
		f := &artifact.Fields[i]
		if f.Field == "key_skills" && f.Status == StatusImproved {
			f.Status, f.Reason = StatusError, "skill levels: "+reason
			return
		}
	}
	im.opt.Log.Warn("autoclone: key-skill levels step failed", "reason", reason)
}

// parkReview records one review item (best effort: a broken reviewer must
// not flip the field's status — the artifact already says needs_human /
// always_ask).
func (im *Improver) parkReview(ctx context.Context, accountID, kind, reason, payload string) int64 {
	if im.opt.Review == nil {
		return 0
	}
	id, err := im.opt.Review.Add(ctx, accountID, kind, reason, payload)
	if err != nil {
		im.opt.Log.Warn("autoclone: review parking failed", "err", err)
		return 0
	}
	return id
}

// persist stores the artifact on the lineage row (best effort — an audit
// write failure is logged loudly but does not fail an applied improvement).
func (im *Improver) persist(ctx context.Context, cloneHash string, artifact *DiffArtifact) {
	b, err := json.Marshal(artifact)
	if err != nil {
		im.opt.Log.Error("autoclone: encode improve diff", "clone", cloneHash, "err", err)
		return
	}
	if err := im.opt.DB.ResumeClonesSetImproveDiff(ctx, cloneHash, string(b)); err != nil {
		im.opt.Log.Error("autoclone: persist improve diff", "clone", cloneHash, "err", err)
	}
}

// improveVerdictShape is the verdict projection the planner consumes.
type improveVerdictShape struct {
	NeedsHuman bool   `json:"needs_human"`
	Reason     string `json:"needs_human_reason"`
	Unchanged  bool   `json:"unchanged"`
}

func decodeImproveVerdict(verdict json.RawMessage) (improveVerdictShape, bool) {
	var out improveVerdictShape
	if err := json.Unmarshal(verdict, &out); err != nil {
		return out, false
	}
	return out, true
}

func reviewPayload(cloneHash, field string, value json.RawMessage) string {
	b, _ := json.Marshal(map[string]any{"clone": cloneHash, "field": field, "proposed": value})
	return string(b)
}

// needReviewPayload is the needs_human parking payload (T54): the full
// resolution input — resume hash, field, locator, item index, было
// (current) and стало (the AI proposal, nil when the verdict carried no
// value slot).
func needReviewPayload(cloneHash string, spec FieldSpec, tg target, proposal any) string {
	b, _ := json.Marshal(map[string]any{
		"clone":   cloneHash,
		"field":   spec.ID,
		"locator": tg.locator,
		"item":    tg.item,
		"before":  tg.current,
		"after":   proposal,
	})
	return string(b)
}

func locateField(a *DiffArtifact, locator string) int {
	for i := range a.Fields {
		if a.Fields[i].Locator == locator {
			return i
		}
	}
	return -1
}

func artifactHasStatus(a *DiffArtifact, status string) bool {
	for i := range a.Fields {
		if a.Fields[i].Status == status {
			return true
		}
	}
	return false
}

func isEmptyValue(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null" || string(raw) == `""` || string(raw) == `[]`
}

func rawEqualT(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}
