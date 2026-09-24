// Package ai is the single LLM gateway: multi-dialect providers, per-task
// prompts (builtin -> TOML override), injection guard, cache, budget,
// validation and decision ledger. LLMs NEVER act: they return structured
// verdicts; Go code executes.
package ai

import (
	"context"
	"encoding/json"
	"time"
)

// TaskName enumerates the 8 AI tasks.
type TaskName string

// The eight task names as they appear in config [ai.tasks] and the
// ai_decisions ledger.
const (
	TaskVacancyScoring TaskName = "vacancy_scoring"
	TaskCoverLetter    TaskName = "cover_letter"
	TaskChatReply      TaskName = "chat_reply"
	TaskTestAnswer     TaskName = "test_answer"
	TaskWarmupPing     TaskName = "warmup_ping"
	TaskClassification TaskName = "message_classification"
	TaskContactExtract TaskName = "contact_extraction"
	TaskLinkDetection  TaskName = "link_detection"
)

// Common fields present in EVERY task schema output.
type Common struct {
	Confidence         float64 `json:"confidence"`
	NeedsHuman         bool    `json:"needs_human"`
	NeedsHumanReason   *string `json:"needs_human_reason"`
	InjectionSuspected bool    `json:"injection_suspected"`
}

// Message is a provider-agnostic chat message.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// Request is a normalized completion request.
type Request struct {
	Task            TaskName
	System          string
	Messages        []Message
	JSONSchema      json.RawMessage // strict schema; provider adapter maps per dialect
	MaxOutputTokens int
	Temperature     float64
	Effort          string // none|minimal|low|medium|high|xhigh|max
}

// Response is a normalized completion result.
type Response struct {
	Content   string
	Model     string
	TokensIn  int
	TokensOut int
	Cached    bool
	LatencyMS int64
	Err       error
}

// Provider is the transport-level interface implemented per dialect.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
	Dialect() string // openai-chat | openai-responses | anthropic-messages
}

// Decision is one audited AI verdict (persisted by the ledger).
type Decision struct {
	ID          string
	CreatedAt   time.Time
	AccountID   string
	Task        TaskName
	EntityType  string // vacancy|chat|message|test|link
	EntityID    string
	RunID       string
	Model       string
	PromptVer   string
	InputDigest string
	Verdict     json.RawMessage
	Score       float64
	Confidence  float64
	NeedsHuman  bool
	Injection   bool
	LatencyMS   int64
	TokensIn    int
	TokensOut   int
	CacheHit    bool
	Outcome     string // pending|approved|edited|rejected|auto
}

// Runner is THE entry point for all features. Implementations wrap
// prompt registry, guard, cache, budget, provider failover and ledger.
type Runner interface {
	// Run executes one AI task and returns the parsed JSON output
	// (already schema-validated) plus the audited decision.
	Run(ctx context.Context, accountID, entityType, entityID, runID string,
		task TaskName, input any) (json.RawMessage, Decision, error)
}
