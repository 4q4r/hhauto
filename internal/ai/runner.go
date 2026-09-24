package ai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"hhauto/internal/ai/schemas"
	"hhauto/internal/aiprompts"
	"hhauto/internal/config"
)

// Budget policies when the daily budget is exhausted.
const (
	PolicyNeedsHumanOnly = "needs_human_only"
	PolicyHardStop       = "hard_stop"
)

// Pipeline limits.
const (
	maxValidationAttempts = 3 // original call + 2 re-asks
	reasonBudget          = "budget_exhausted"
	reasonSchemaInvalid   = "schema_invalid"
	reasonInjection       = "injection_suspected"
	reasonOfferInvitation = "offer_or_invitation"
	reasonGrayBand        = "gray_band"
)

// LedgerStore is the minimal audit-trail contract; Wave 3 adapts it to the
// store DAO (AIDecisionsAdd / AIDecisionsSumTokensDay). nil is allowed and
// disables both auditing and budget enforcement (tests).
type LedgerStore interface {
	Add(rec *Decision) error
	SumTokensDay(day string) (int, float64)
}

// BudgetConfig mirrors [ai] daily_budget_* and on_budget_exceed.
type BudgetConfig struct {
	DailyTokens int
	DailyUSD    float64
	OnExceed    string
}

// RunnerOption customizes the Runner.
type RunnerOption func(*runner) error

// WithBudget installs the daily-budget guard.
func WithBudget(b BudgetConfig) RunnerOption {
	return func(r *runner) error {
		switch b.OnExceed {
		case PolicyNeedsHumanOnly, PolicyHardStop:
		default:
			return fmt.Errorf("ai: on_budget_exceed %q: must be %q or %q", b.OnExceed, PolicyNeedsHumanOnly, PolicyHardStop)
		}
		r.budget = b
		return nil
	}
}

// Runner is the single entry point for every AI task: prompt registry →
// untrusted-input guard → cache → budget → provider (with model fallback) →
// strict schema validation → deterministic post-checks → ledger.
type runner struct {
	providers map[string]Provider // config provider id → dialect adapter
	bound     map[string]Provider // model profile → bound provider
	fallback  map[string]Provider // model profile → fallback bound provider
	models    map[string]config.AIModel
	tasks     map[string]config.AITask
	ledger    LedgerStore
	cache     CacheStore
	log       *slog.Logger
	prompts   *PromptRegistry
	validator *validator
	guard     *Guard
	budget    BudgetConfig
	now       func() time.Time
}

// New validates the whole reference graph up front (fail fast, no silent
// fallbacks) and returns a Runner. providers are keyed by config provider id;
// models by model profile; tasks by TaskName.
func New(
	providers map[string]Provider,
	models map[string]config.AIModel,
	tasks map[string]config.AITask,
	ledger LedgerStore,
	cache CacheStore,
	log *slog.Logger,
	opts ...RunnerOption,
) (Runner, error) {
	if log == nil {
		log = slog.Default()
	}
	prompts, err := NewPromptRegistry(tasks, aiprompts.FS)
	if err != nil {
		return nil, err
	}
	val, err := newValidator()
	if err != nil {
		return nil, err
	}
	r := &runner{
		providers: providers,
		bound:     map[string]Provider{},
		fallback:  map[string]Provider{},
		models:    models,
		tasks:     tasks,
		ledger:    ledger,
		cache:     cache,
		log:       log,
		prompts:   prompts,
		validator: val,
		guard:     NewGuard(),
		now:       time.Now,
	}
	for name := range tasks {
		if _, ok := taskByName(name); !ok {
			return nil, fmt.Errorf("ai: tasks[%q]: unknown task", name)
		}
	}
	for profile, m := range models {
		p, ok := providers[m.Provider]
		if !ok {
			return nil, fmt.Errorf("ai: models[%q]: provider %q is not configured", profile, m.Provider)
		}
		bound, err := NewBoundProvider(p, m.Model)
		if err != nil {
			return nil, fmt.Errorf("ai: models[%q]: %w", profile, err)
		}
		r.bound[profile] = bound
		if m.FallbackModelRef != "" {
			fb, ok := models[m.FallbackModelRef]
			if !ok {
				return nil, fmt.Errorf("ai: models[%q]: fallback %q is not a configured model", profile, m.FallbackModelRef)
			}
			fbp, ok := providers[fb.Provider]
			if !ok {
				return nil, fmt.Errorf("ai: models[%q]: fallback provider %q is not configured", profile, fb.Provider)
			}
			boundFB, err := NewBoundProvider(fbp, fb.Model)
			if err != nil {
				return nil, fmt.Errorf("ai: models[%q]: fallback: %w", profile, err)
			}
			r.fallback[profile] = boundFB
		}
	}
	for name, t := range tasks {
		if _, ok := r.bound[t.ModelProfile]; !ok {
			return nil, fmt.Errorf("ai: tasks[%q]: model profile %q is not configured", name, t.ModelProfile)
		}
	}
	for _, o := range opts {
		if err := o(r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Run executes one AI task and returns the schema-validated verdict plus the
// audited decision. See package docs for the pipeline order and the failure
// semantics (re-ask on invalid output, needs_human verdicts, model fallback).
func (r *runner) Run(ctx context.Context, accountID, entityType, entityID, runID string,
	task TaskName, input any) (json.RawMessage, Decision, error) {

	taskCfg, ok := r.tasks[string(task)]
	if !ok {
		return nil, Decision{}, fmt.Errorf("ai: task %q is not configured", task)
	}
	system, promptVer, err := r.prompts.SystemPrompt(task)
	if err != nil {
		return nil, Decision{}, err
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, Decision{}, fmt.Errorf("ai: encode input: %w", err)
	}
	open, close_ := r.delimiters()
	userBlock := open + "\n" + string(inputJSON) + "\n" + close_
	flagged, _ := r.guard.Inspect(userBlock)

	model := r.models[taskCfg.ModelProfile]
	schema, err := r.taskSchema(task)
	if err != nil {
		return nil, Decision{}, err
	}
	baseReq := Request{
		Task:            task,
		System:          system,
		Messages:        []Message{{Role: "user", Content: userBlock}},
		JSONSchema:      schema,
		MaxOutputTokens: model.MaxOutputTokens,
		Temperature:     model.Temperature,
		Effort:          model.ReasoningEffort,
	}
	provider := r.bound[taskCfg.ModelProfile]

	dec := Decision{
		ID:          ulid.Make().String(),
		CreatedAt:   r.now().UTC(),
		AccountID:   accountID,
		Task:        task,
		EntityType:  entityType,
		EntityID:    entityID,
		RunID:       runID,
		Model:       model.Model,
		PromptVer:   promptVer,
		InputDigest: inputDigest(inputJSON),
		Injection:   flagged,
		Outcome:     "pending",
	}
	start := r.now()

	// 1) Cache: identical (task, prompt, model, params, input) within TTL.
	cacheKey := cacheKey(task, promptVer, model, schema, inputJSON)
	if r.cache != nil {
		if cached, ok, err := r.cache.Get(ctx, cacheKey); err != nil {
			r.log.Warn("ai: cache get failed; treating as miss", "err", err)
		} else if ok {
			dec.CacheHit = true
			dec.LatencyMS = r.now().Sub(start).Milliseconds()
			if err := r.record(&dec, cached); err != nil {
				return cached, dec, err
			}
			return cached, dec, nil
		}
	}

	// 2) Budget: consulted before any provider call.
	if r.ledger != nil && (r.budget.DailyTokens > 0 || r.budget.DailyUSD > 0) {
		tokens, cost := r.ledger.SumTokensDay(r.day())
		exhausted := (r.budget.DailyTokens > 0 && tokens >= r.budget.DailyTokens) ||
			(r.budget.DailyUSD > 0 && cost >= r.budget.DailyUSD)
		if exhausted {
			switch r.budget.OnExceed {
			case PolicyHardStop:
				return nil, dec, fmt.Errorf("ai: daily budget exhausted (tokens=%d cost=%.4f)", tokens, cost)
			default: // PolicyNeedsHumanOnly
				verdict := syntheticVerdict(reasonBudget, flagged)
				dec.NeedsHuman = true
				dec.LatencyMS = r.now().Sub(start).Milliseconds()
				if err := r.record(&dec, verdict); err != nil {
					return verdict, dec, err
				}
				return verdict, dec, nil
			}
		}
	}

	// 3) Provider call with re-ask loop and model fallback.
	resp, invalid, err := r.callWithValidation(ctx, provider, taskCfg.ModelProfile, baseReq)
	if err != nil {
		return nil, dec, err
	}
	dec.LatencyMS = r.now().Sub(start).Milliseconds()
	dec.TokensIn = resp.TokensIn
	dec.TokensOut = resp.TokensOut
	dec.Model = resp.Model

	// 4) Deterministic post-checks (guard flag wins, never blocks).
	verdict := []byte(resp.Content)
	if invalid {
		verdict = syntheticVerdict(reasonSchemaInvalid, flagged)
		dec.NeedsHuman = true
	} else {
		verdict = r.postChecks(task, verdict, flagged)
		dec.Confidence = commonFloat(verdict, "confidence")
		dec.NeedsHuman = commonBool(verdict, "needs_human")
		dec.Injection = commonBool(verdict, "injection_suspected")
		if task == TaskVacancyScoring {
			dec.Score = commonFloat(verdict, "score")
		}
	}

	// 5) Cache the validated verdict.
	if r.cache != nil && taskCfg.CacheTTLHours > 0 {
		if err := r.cache.Set(ctx, cacheKey, verdict, time.Duration(taskCfg.CacheTTLHours)*time.Hour); err != nil {
			r.log.Warn("ai: cache set failed", "err", err)
		}
	}

	if err := r.record(&dec, verdict); err != nil {
		return verdict, dec, err
	}
	return verdict, dec, nil
}

// callWithValidation performs the provider call; invalid model output is
// re-asked with the validator error (at most two retries). A provider failure
// falls back to the profile's fallback model (once) before returning the
// error. invalid=true means every attempt failed validation and resp carries
// the last raw response (the caller synthesizes the needs_human verdict).
func (r *runner) callWithValidation(ctx context.Context, provider Provider, profile string, req Request) (resp Response, invalid bool, err error) {
	resp, invalid, err = r.reaskLoop(ctx, provider, req)
	if err == nil {
		return resp, invalid, nil
	}
	fb, ok := r.fallback[profile]
	if !ok {
		return Response{}, false, err
	}
	fbProfile := r.models[profile].FallbackModelRef
	r.log.Warn("ai: primary model failed; falling back",
		"task", string(req.Task), "primary", r.models[profile].Model,
		"fallback", r.models[fbProfile].Model, "err", err)
	resp, invalid, err = r.reaskLoop(ctx, fb, req)
	if err != nil {
		return Response{}, false, err
	}
	resp.Model = r.models[fbProfile].Model
	return resp, invalid, nil
}

// reaskLoop runs the provider and, while the output fails schema validation,
// re-asks with the validator error. After maxValidationAttempts it returns
// the last raw response with invalid=true (never an error).
func (r *runner) reaskLoop(ctx context.Context, provider Provider, req Request) (resp Response, invalid bool, err error) {
	attemptReq := req
	var lastErr error
	var lastRaw string
	for attempt := 0; attempt < maxValidationAttempts; attempt++ {
		if attempt > 0 {
			attemptReq = req
			attemptReq.Messages = append(append([]Message{}, req.Messages...),
				Message{Role: "assistant", Content: lastRaw},
				Message{Role: "user", Content: reAskPrompt(lastErr)})
		}
		resp, err = r.providerCall(ctx, provider, attemptReq)
		if err != nil {
			return Response{}, false, err
		}
		lastRaw = resp.Content
		lastErr = r.validator.validate(req.Task, json.RawMessage(resp.Content))
		if lastErr == nil {
			return resp, false, nil
		}
	}
	r.log.Warn("ai: model output failed validation after retries",
		"task", string(req.Task), "err", lastErr)
	return Response{Content: lastRaw}, true, nil
}

func reAskPrompt(err error) string {
	msg := err.Error()
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return "Твой предыдущий ответ не прошёл валидацию: " + msg +
		"\nВерни ровно один валидный JSON-объект по переданной схеме, без markdown, без пояснений."
}

func (r *runner) providerCall(ctx context.Context, p Provider, req Request) (Response, error) {
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return Response{}, err
	}
	if strings.TrimSpace(resp.Content) == "" {
		return Response{}, fmt.Errorf("ai: provider returned empty content")
	}
	return resp, nil
}

func (r *runner) modelsProfileFallback(profile string) string {
	return r.models[profile].FallbackModelRef
}

// postChecks mutates the verdict map with deterministic rules: the guard flag
// raises needs_human/injection_suspected (never blocks), offer/invitation
// always route to a human, gray band needs a human, weak band caps score at 25.
func (r *runner) postChecks(task TaskName, verdict json.RawMessage, flagged bool) json.RawMessage {
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(verdict)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		// Unparseable output never reaches here (validator would have
		// caught it); stay defensive anyway.
		return syntheticVerdict(reasonSchemaInvalid, flagged)
	}
	forceReason := ""
	if flagged {
		m["injection_suspected"] = true
		m["needs_human"] = true
		forceReason = reasonInjection
	}
	switch task {
	case TaskClassification:
		if cat, _ := m["category"].(string); cat == "offer" || cat == "invitation" {
			m["needs_human"] = true
			if forceReason == "" {
				forceReason = reasonOfferInvitation
			}
		}
	case TaskVacancyScoring:
		if band, _ := m["band"].(string); band == "gray" {
			m["needs_human"] = true
			if forceReason == "" {
				forceReason = reasonGrayBand
			}
		}
		if band, _ := m["band"].(string); band == "weak" {
			if score, ok := asInt(m["score"]); ok && score > 25 {
				m["score"] = 25
			}
		}
	}
	if forceReason != "" {
		if cur, ok := m["needs_human_reason"].(string); !ok || cur == "" {
			m["needs_human_reason"] = forceReason
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return syntheticVerdict(reasonSchemaInvalid, flagged)
	}
	return out
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func commonBool(raw json.RawMessage, key string) bool {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return false
	}
	b, _ := generic[key].(bool)
	return b
}

func commonFloat(raw json.RawMessage, key string) float64 {
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return 0
	}
	switch n := generic[key].(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// record appends the decision to the ledger when one is configured. Audit
// failures fail the run loudly: the caller still receives the verdict for
// observability, but the error is non-nil so the audit gap is visible.
func (r *runner) record(dec *Decision, verdict json.RawMessage) error {
	dec.Verdict = json.RawMessage(verdict)
	if r.ledger == nil {
		return nil
	}
	rec := &Decision{
		ID: dec.ID, CreatedAt: dec.CreatedAt, AccountID: dec.AccountID,
		Task: dec.Task, EntityType: dec.EntityType, EntityID: dec.EntityID,
		RunID: dec.RunID, Model: dec.Model, PromptVer: dec.PromptVer,
		InputDigest: dec.InputDigest, Verdict: verdict,
		Score: dec.Score, Confidence: dec.Confidence,
		NeedsHuman: dec.NeedsHuman, Injection: dec.Injection,
		LatencyMS: dec.LatencyMS, TokensIn: dec.TokensIn, TokensOut: dec.TokensOut,
		CacheHit: dec.CacheHit, Outcome: dec.Outcome,
	}
	if err := r.ledger.Add(rec); err != nil {
		r.log.Error("ai: ledger write failed; audit trail incomplete", "err", err)
		return fmt.Errorf("ai: ledger: %w", err)
	}
	return nil
}

func (r *runner) taskSchema(task TaskName) (json.RawMessage, error) {
	raw, err := schemas.FS.ReadFile(string(task) + ".json")
	if err != nil {
		return nil, fmt.Errorf("ai: schema for %q: %w", task, err)
	}
	return json.RawMessage(raw), nil
}

// delimiters generates random markers so injected content cannot predict or
// forge the untrusted-block boundaries.
func (r *runner) delimiters() (string, string) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable for a security boundary
		panic(fmt.Sprintf("ai: rand: %v", err))
	}
	tag := "untrusted_" + hex.EncodeToString(b[:])
	return "<" + tag + ">", "</" + tag + ">"
}

func (r *runner) day() string {
	return r.now().UTC().Format("2006-01-02")
}

func cacheKey(task TaskName, promptVer string, model config.AIModel, schema json.RawMessage, input []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%.3f|%s|%s|%s",
		task, promptVer, model.Model, model.MaxOutputTokens, model.Temperature,
		model.ReasoningEffort, inputDigest(schema), inputDigest(input))
	return hex.EncodeToString(h.Sum(nil))
}

func inputDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func syntheticVerdict(reason string, injection bool) json.RawMessage {
	m := map[string]any{
		"confidence":          0,
		"needs_human":         true,
		"needs_human_reason":  reason,
		"injection_suspected": injection,
	}
	out, _ := json.Marshal(m)
	return out
}

// compile-time contract check
var _ Runner = (*runner)(nil)
