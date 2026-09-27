package ai

// JevSolver is the optional System-One prefilter (T33). For the three
// classification-only tasks it asks the Jev-compatible endpoint typed
// questions about the task input and, when the calibrated confidence clears
// the configured threshold, produces the task's strict-schema verdict
// WITHOUT an LLM call. Jev never generates text — answers are option
// strings/indices and numbers only — so the solver maps them onto the enums
// of internal/ai/schemas/*.json.
//
// Outcome taxonomy (mirror of the runner routing in runner.go):
//   - decided   — confidence ≥ threshold → schema-shaped verdict;
//   - undecided — confidence < threshold, or the input is outside Jev's
//     competence (no URLs to classify; contacts present but Jev cannot
//     extract their values) → escalate to the LLM;
//   - failed    — transport error, HTTP error or malformed answers →
//     escalate to the LLM (fail-safe: the prefilter can only save calls,
//     never break a run).
//
// Capability note (contact_extraction): a confident "yes, there are
// contacts" still escalates — value/normalized/source_quote require text
// generation Jev cannot do, and an empty-string skeleton would silently
// drop the contacts the LLM would have extracted. The confident "no"
// (contacts: []) is decided locally and saves the LLM call. The
// contact_type choice still rides in every request per the T33 contract.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Decision provenance values (Decision.Source).
const (
	SourceLLM = "llm"
	SourceJev = "jev"
)

// errJevUndecided marks a normal escalation: the input is outside Jev's
// competence (not a failure — no warning, no error counter).
var errJevUndecided = errors.New("jev: task input is outside jev competence")

// Classification option sets. MUST stay in lockstep with the enums in
// internal/ai/schemas/message_classification.json, link_detection.json and
// contact_extraction.json.
var (
	jevClassOptions       = []string{"rejection", "invitation", "test_assignment", "question", "offer", "spam", "other"}
	jevLinkOptions        = []string{"google_form", "yandex_form", "ats", "calendar", "test_platform", "messenger", "other"}
	jevContactTypeOptions = []string{"email", "phone", "telegram", "whatsapp", "vk", "other_messenger", "url", "other"}
)

// JevSolver routes enrolled tasks to the Jev endpoint. Safe for concurrent
// use; counters feed monitoring (SolverStats).
type JevSolver struct {
	client    *JevClient
	threshold float64
	tasks     map[TaskName]struct{}
	log       *slog.Logger

	decided   atomic.Int64
	escalated atomic.Int64
	failed    atomic.Int64
}

// NewJevSolver validates the prefilter wiring up front (fail fast).
func NewJevSolver(client *JevClient, threshold float64, tasks []TaskName, log *slog.Logger) (*JevSolver, error) {
	if client == nil {
		return nil, fmt.Errorf("jev: nil client")
	}
	if threshold <= 0 || threshold > 1 {
		return nil, fmt.Errorf("jev: confidence threshold %v: must be in (0,1]", threshold)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("jev: no tasks enrolled")
	}
	if log == nil {
		log = slog.Default()
	}
	s := &JevSolver{
		client:    client,
		threshold: threshold,
		tasks:     make(map[TaskName]struct{}, len(tasks)),
		log:       log,
	}
	for _, t := range tasks {
		switch t {
		case TaskClassification, TaskLinkDetection, TaskContactExtract:
			s.tasks[t] = struct{}{}
		default:
			return nil, fmt.Errorf("jev: task %q is not jev-compatible", t)
		}
	}
	return s, nil
}

// Handles reports whether the task is enrolled in the prefilter.
func (s *JevSolver) Handles(task TaskName) bool {
	_, ok := s.tasks[task]
	return ok
}

// Threshold returns the configured confidence gate.
func (s *JevSolver) Threshold() float64 { return s.threshold }

// Solve runs one prefilter round. Returns (verdict, confidence, nil) when
// Jev decided, (nil, confidence, nil) on a normal escalation and an error
// when Jev is unavailable or answered malformed data (also escalates).
func (s *JevSolver) Solve(ctx context.Context, task TaskName, input any) (json.RawMessage, float64, error) {
	verdict, conf, err := s.solve(ctx, task, input)
	if errors.Is(err, errJevUndecided) {
		s.escalated.Add(1)
		return nil, conf, nil
	}
	if err != nil {
		s.failed.Add(1)
		return nil, 0, err
	}
	if verdict == nil { // confidence below threshold
		s.escalated.Add(1)
		return nil, conf, nil
	}
	s.decided.Add(1)
	return verdict, conf, nil
}

// solve is the pipeline body: build questions → call → map answers to the
// task schema (the threshold gate happens per task inside the mappers).
func (s *JevSolver) solve(ctx context.Context, task TaskName, input any) (json.RawMessage, float64, error) {
	inputJSON, err := json.Marshal(input) // compact by construction
	if err != nil {
		return nil, 0, fmt.Errorf("jev: encode input: %w", err)
	}
	var req *JevRequest
	switch task {
	case TaskClassification:
		req = jevClassificationRequest(inputJSON)
	case TaskLinkDetection:
		req, err = jevLinkRequest(inputJSON)
	case TaskContactExtract:
		req = jevContactRequest(inputJSON)
	default:
		return nil, 0, fmt.Errorf("jev: task %q is not jev-compatible", task)
	}
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.client.Solve(ctx, *req)
	if err != nil {
		return nil, 0, err
	}
	return s.verdict(task, inputJSON, resp)
}

// verdict maps the answers onto the task's strict schema and applies the
// threshold gate; nil verdict = escalate (undecided).
func (s *JevSolver) verdict(task TaskName, inputJSON []byte, resp JevResponse) (json.RawMessage, float64, error) {
	byID := make(map[string]JevAnswer, len(resp.Answers))
	for _, a := range resp.Answers {
		byID[a.ID] = a
	}
	switch task {
	case TaskClassification:
		return s.verdictClassification(byID)
	case TaskLinkDetection:
		return s.verdictLinkDetection(inputJSON, byID)
	case TaskContactExtract:
		return s.verdictContact(byID)
	default:
		return nil, 0, fmt.Errorf("jev: task %q is not jev-compatible", task)
	}
}

// verdictClassification maps the category choice onto message_classification.
func (s *JevSolver) verdictClassification(byID map[string]JevAnswer) (json.RawMessage, float64, error) {
	a, ok := byID["category"]
	if !ok {
		return nil, 0, fmt.Errorf("jev: missing answer %q", "category")
	}
	category, err := jevChoiceValue(a, jevClassOptions)
	if err != nil {
		return nil, 0, err
	}
	if a.Confidence < s.threshold {
		return nil, a.Confidence, nil
	}
	return jevMarshal(map[string]any{
		"confidence": a.Confidence, "needs_human": false,
		"needs_human_reason": nil, "injection_suspected": false,
		"category":       category,
		"evidence_quote": nil, // Jev returns choices only; evidence stays with the LLM path
	}), a.Confidence, nil
}

// verdictLinkDetection maps the per-URL choices onto link_detection; the
// run's confidence is the minimum across answers (all-or-nothing gate).
func (s *JevSolver) verdictLinkDetection(inputJSON []byte, byID map[string]JevAnswer) (json.RawMessage, float64, error) {
	var probe struct {
		URLs []string `json:"urls"`
	}
	if err := json.Unmarshal(inputJSON, &probe); err != nil || len(probe.URLs) == 0 {
		return nil, 0, errJevUndecided
	}
	links := make([]map[string]any, 0, len(probe.URLs))
	conf := 1.0
	for i, u := range probe.URLs {
		id := fmt.Sprintf("link_%d", i)
		a, ok := byID[id]
		if !ok {
			return nil, 0, fmt.Errorf("jev: missing answer %q", id)
		}
		kind, err := jevChoiceValue(a, jevLinkOptions)
		if err != nil {
			return nil, 0, err
		}
		if a.Confidence < conf {
			conf = a.Confidence
		}
		links = append(links, map[string]any{
			"url": u, "kind": kind, "risk": jevLinkRisk(kind),
			// Conservative recommendation: Jev links always go to review;
			// evidence stays with the LLM path (Jev returns choices only).
			"recommendation": "review", "evidence_quote": nil,
		})
	}
	if conf < s.threshold {
		return nil, conf, nil
	}
	return jevMarshal(map[string]any{
		"confidence": conf, "needs_human": false,
		"needs_human_reason": nil, "injection_suspected": false,
		"links":   links,
		"summary": fmt.Sprintf("Jev: %d внешняя(ие) ссылка(и) — классифицированы, на проверку", len(links)),
	}), conf, nil
}

// verdictContact maps the has_contacts noul onto contact_extraction. A
// confident "no" decides (contacts: []); a confident "yes" escalates —
// value/normalized/source_quote require text generation Jev cannot do, and
// a skeleton would silently drop the contacts the LLM would extract.
func (s *JevSolver) verdictContact(byID map[string]JevAnswer) (json.RawMessage, float64, error) {
	a, ok := byID["has_contacts"]
	if !ok {
		return nil, 0, fmt.Errorf("jev: missing answer %q", "has_contacts")
	}
	has, err := jevNoulValue(a)
	if err != nil {
		return nil, 0, err
	}
	// contact_type rides in every request per the T33 contract; its value
	// is validated so a malformed backend surfaces as an error.
	if t, ok := byID["contact_type"]; ok {
		if _, err := jevChoiceValue(t, jevContactTypeOptions); err != nil {
			return nil, 0, err
		}
	}
	if a.Confidence < s.threshold {
		return nil, a.Confidence, nil
	}
	if has {
		return nil, a.Confidence, errJevUndecided
	}
	return jevMarshal(map[string]any{
		"confidence": a.Confidence, "needs_human": false,
		"needs_human_reason": nil, "injection_suspected": false,
		"contacts": []any{},
	}), a.Confidence, nil
}

// jevMarshal renders a verdict map deterministically.
func jevMarshal(m map[string]any) json.RawMessage {
	out, err := json.Marshal(m)
	if err != nil { // map[string]any with plain values cannot fail
		panic(fmt.Sprintf("jev: marshal verdict: %v", err))
	}
	return out
}

// jevChoiceValue resolves a choice answer's value: an option string or a
// zero-based option index; anything else (or an unknown option) is an error
// that escalates the run to the LLM.
func jevChoiceValue(a JevAnswer, options []string) (string, error) {
	var opt string
	if err := json.Unmarshal(a.Value, &opt); err == nil {
		for _, o := range options {
			if o == opt {
				return opt, nil
			}
		}
		return "", fmt.Errorf("jev: unknown choice value %q", opt)
	}
	var idx float64
	if err := json.Unmarshal(a.Value, &idx); err == nil {
		i := int(idx)
		if i >= 0 && float64(i) == idx && i < len(options) {
			return options[i], nil
		}
		return "", fmt.Errorf("jev: choice index %v out of range", idx)
	}
	return "", fmt.Errorf("jev: unsupported choice value %s", string(a.Value))
}

// jevNoulValue resolves a noul answer: a boolean, or a number read as the
// yes-probability (≥0.5 = yes).
func jevNoulValue(a JevAnswer) (bool, error) {
	var b bool
	if err := json.Unmarshal(a.Value, &b); err == nil {
		return b, nil
	}
	var f float64
	if err := json.Unmarshal(a.Value, &f); err == nil {
		return f >= 0.5, nil
	}
	return false, fmt.Errorf("jev: unsupported noul value %s", string(a.Value))
}

// jevLinkRisk is the deterministic conservative risk mapping: external forms
// and unknown targets are medium, everything else low. Jev never emits
// "high", so enabling the prefilter can only add detections, never
// auto-reviews (interview routes high-risk links to the review queue).
func jevLinkRisk(kind string) string {
	switch kind {
	case "google_form", "yandex_form", "other":
		return "medium"
	default: // ats, calendar, test_platform, messenger
		return "low"
	}
}

// jevClassificationRequest asks the single category choice.
func jevClassificationRequest(inputJSON []byte) *JevRequest {
	return &JevRequest{
		State: string(inputJSON),
		Questions: []JevQuestion{{
			ID:      "category",
			Type:    JevQuestionChoice,
			Prompt:  "К какой категории относится сообщение работодателя? Выбери одну опцию.",
			Options: jevClassOptions,
		}},
	}
}

// jevLinkRequest asks one choice per URL candidate from input.urls.
func jevLinkRequest(inputJSON []byte) (*JevRequest, error) {
	var probe struct {
		URLs []string `json:"urls"`
	}
	if err := json.Unmarshal(inputJSON, &probe); err != nil || len(probe.URLs) == 0 {
		// Nothing to classify — outside Jev's competence, escalate quietly.
		return nil, errJevUndecided
	}
	qs := make([]JevQuestion, 0, len(probe.URLs))
	for i, u := range probe.URLs {
		qs = append(qs, JevQuestion{
			ID:      fmt.Sprintf("link_%d", i),
			Type:    JevQuestionChoice,
			Prompt:  "К какому типу относится ссылка " + u + "? Выбери одну опцию.",
			Options: jevLinkOptions,
		})
	}
	return &JevRequest{State: string(inputJSON), Questions: qs}, nil
}

// jevContactRequest asks the has-contacts noul plus the contact-type choice
// (the T33 question set for contact_extraction).
func jevContactRequest(inputJSON []byte) *JevRequest {
	return &JevRequest{
		State: string(inputJSON),
		Questions: []JevQuestion{
			{
				ID:     "has_contacts",
				Type:   JevQuestionNoul,
				Prompt: "Содержит ли текст контактные данные (телефон, email, ник в мессенджере)?",
			},
			{
				ID:      "contact_type",
				Type:    JevQuestionChoice,
				Prompt:  "Какой основной тип контакта содержится в тексте? Выбери одну опцию.",
				Options: jevContactTypeOptions,
			},
		},
	}
}

// SolverStats is the metrics snapshot of one solver.
type SolverStats struct {
	Decided   int64 `json:"decided"`
	Escalated int64 `json:"escalated"`
	Failed    int64 `json:"failed"`
}

// Stats returns the current solver counters.
func (s *JevSolver) Stats() SolverStats {
	return SolverStats{
		Decided:   s.decided.Load(),
		Escalated: s.escalated.Load(),
		Failed:    s.failed.Load(),
	}
}
