package store

import "time"

// Domain structs. Nullable columns map to pointer fields; empty-string
// status/state fields fall back to the schema DEFAULT on insert.

// Account is a configured hh.ru account row and its session health.
type Account struct {
	ID          string
	DisplayName string
	HhUserID    *int64
	Status      string // "" -> 'unknown'
	LastOKAt    *time.Time
	CreatedAt   time.Time // filled on insert
	UpdatedAt   time.Time // filled on insert/update
}

// Resume is a cached resume snapshot (write-through from the live hh API).
type Resume struct {
	ID         int64 // assigned on insert, unchanged on conflict
	AccountID  string
	HhResumeID string
	Title      string
	Status     string
	Raw        string
	UpdatedAt  time.Time
}

// ResumeClone is one hhauto-tracked clone lineage edge: clone_hash was
// created as a clone of parent_hash at the given depth (root resume depth 0,
// direct clones 1, …). hh.ru does not expose the clone parent (no
// parentResumeId on live clones), so this table is the single source of
// lineage truth (T26, migration 0003).
type ResumeClone struct {
	CloneHash  string
	ParentHash string
	Depth      int
	AccountID  string
	CreatedAt  time.Time // filled when zero
}

// VacancySeen deduplicates vacancies per account+vacancy (UNIQUE).
type VacancySeen struct {
	AccountID   string
	VacancyID   int64
	Source      string
	FirstSeenAt time.Time
	Meta        string
}

// Application is one apply attempt with its dedup key and funnel status.
type Application struct {
	ID            int64 // assigned on insert
	AccountID     string
	RunID         string
	HhResumeID    string
	VacancyID     int64
	EmployerID    int64
	DedupKey      string
	Status        string // CHECK-enforced: queued|dry_run|applied|unknown|failed|skipped_*
	Letter        *string
	FailReason    *string
	CorrelationID *string
	CreatedAt     time.Time // filled on insert
	AppliedAt     *time.Time
}

// Chat tracks one chatik conversation and its chatops state machine.
type Chat struct {
	ID                  int64 // assigned on insert, unchanged on conflict
	AccountID           string
	ChatikChatID        string
	TopicID             *string
	VacancyID           *int64
	EmployerID          *int64
	EmployerName        *string
	State               string // "" -> 'active'
	RejectionConfidence *float64
	LastMessageAt       *time.Time
	LastOutgoingAt      *time.Time
	LeaveCount          int
	UpdatedAt           time.Time
}

// Message is a single chat message (in or out), unique per message_id.
type Message struct {
	ID        int64 // assigned on insert
	ChatID    int64
	MessageID string
	Direction string // 'in' | 'out'
	Author    *string
	Text      string
	SentAt    time.Time
	CreatedAt time.Time // filled on insert
}

// Contact is one collected employer contact (card, chat or email source).
type Contact struct {
	ID          int64 // assigned on insert, unchanged on conflict
	AccountID   string
	EmployerID  int64
	Source      string // 'card' | 'chat' | 'email'
	Name        *string
	Role        *string
	Phones      string
	Emails      string
	Messengers  string
	Raw         string
	CollectedAt time.Time
}

// Assessment is a detected screening test and its lifecycle.
type Assessment struct {
	ID          int64 // assigned on insert
	AccountID   string
	VacancyID   *int64
	Kind        string // 'hh_internal' | 'external_form' | 'unknown'
	Provider    *string
	ExternalURL *string
	Status      string // "" -> 'detected'
	Answers     *string
	Result      *string
	CreatedAt   time.Time
	FinishedAt  *time.Time
}

// InterviewLink is a detected interview link from a chat.
type InterviewLink struct {
	ID         int64 // assigned on insert, unchanged on conflict
	AccountID  string
	ChatID     *int64
	URL        string
	Provider   string
	Status     string // "" -> 'detected'
	DetectedAt time.Time
}

// PingJob is a scheduled warm-up follow-up ping with attempt accounting.
type PingJob struct {
	ID          int64
	AccountID   string
	ChatID      int64
	DueAt       time.Time
	Attempts    int
	MaxAttempts int
	State       string // pending|sent|cancelled|exhausted
	CreatedAt   time.Time
	SentAt      *time.Time
}

// AIDecision is the audit ledger row for one AI run (model, prompt, usage, cost).
type AIDecision struct {
	ID          string    // ULID, filled when empty
	CreatedAt   time.Time // filled when zero
	AccountID   string
	Task        string
	EntityType  string
	EntityID    string
	RunID       string
	Model       string
	PromptVer   string
	InputDigest string
	Verdict     string
	Score       *float64
	Confidence  *float64
	NeedsHuman  bool
	Injection   bool
	LatencyMS   int
	TokensIn    int
	TokensOut   int
	CacheHit    bool
	CostUSD     float64 // per-decision cost in USD (migration 0002)
	Outcome     string  // "" -> 'pending'
}

// Schedule is one persisted schedule row (next_run survives restarts).
type Schedule struct {
	ID        string
	AccountID string
	Spec      string
	Enabled   bool
	LastRunAt *time.Time
	NextRunAt *time.Time
	UpdatedAt time.Time
}

// ReviewItem is an operator-review task surfaced by the engine.
type ReviewItem struct {
	ID         int64 // assigned on insert
	AccountID  string
	Kind       string
	Reason     string
	Payload    string
	Status     string // open|resolved|ignored
	Resolution *string
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// EmployerRating is a cached employer rating snapshot for filtering.
type EmployerRating struct {
	EmployerID   int64
	ScoreAll     float64
	ScoreRecent  float64
	PeriodDays   int
	ReviewsCount int
	RecommendPct int
	FetchedAt    time.Time
}

// OutboxItem is a webhook payload pending delivery/redelivery.
type OutboxItem struct {
	ID          int64 // assigned on insert
	EndpointID  string
	Payload     string
	Attempts    int
	NextRetryAt *time.Time
	Delivered   bool
	CreatedAt   time.Time // filled on insert
}

// CaptchaEvent records one captcha solve attempt and its latency.
type CaptchaEvent struct {
	ID         int64 // assigned on insert
	AccountID  string
	ProviderID string
	Kind       string
	Ok         bool
	LatencyMS  int
	CreatedAt  time.Time
}

// ProfileFact is one user-supplied Q&A fact used in AI prompts.
type ProfileFact struct {
	AccountID string
	Question  string
	Answer    string
	UpdatedAt time.Time
}
