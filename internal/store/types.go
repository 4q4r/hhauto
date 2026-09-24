package store

import "time"

// Domain structs. Nullable columns map to pointer fields; empty-string
// status/state fields fall back to the schema DEFAULT on insert.

type Account struct {
	ID          string
	DisplayName string
	HhUserID    *int64
	Status      string // "" -> 'unknown'
	LastOKAt    *time.Time
	CreatedAt   time.Time // filled on insert
	UpdatedAt   time.Time // filled on insert/update
}

type Resume struct {
	ID         int64 // assigned on insert, unchanged on conflict
	AccountID  string
	HhResumeID string
	Title      string
	Status     string
	Raw        string
	UpdatedAt  time.Time
}

type VacancySeen struct {
	AccountID   string
	VacancyID   int64
	Source      string
	FirstSeenAt time.Time
	Meta        string
}

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

type InterviewLink struct {
	ID         int64 // assigned on insert, unchanged on conflict
	AccountID  string
	ChatID     *int64
	URL        string
	Provider   string
	Status     string // "" -> 'detected'
	DetectedAt time.Time
}

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
	Outcome     string // "" -> 'pending'
}

type Schedule struct {
	ID        string
	AccountID string
	Spec      string
	Enabled   bool
	LastRunAt *time.Time
	NextRunAt *time.Time
	UpdatedAt time.Time
}

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

type EmployerRating struct {
	EmployerID   int64
	ScoreAll     float64
	ScoreRecent  float64
	PeriodDays   int
	ReviewsCount int
	RecommendPct int
	FetchedAt    time.Time
}

type OutboxItem struct {
	ID          int64 // assigned on insert
	EndpointID  string
	Payload     string
	Attempts    int
	NextRetryAt *time.Time
	Delivered   bool
	CreatedAt   time.Time // filled on insert
}

type CaptchaEvent struct {
	ID         int64 // assigned on insert
	AccountID  string
	ProviderID string
	Kind       string
	Ok         bool
	LatencyMS  int
	CreatedAt  time.Time
}

type ProfileFact struct {
	AccountID string
	Question  string
	Answer    string
	UpdatedAt time.Time
}
