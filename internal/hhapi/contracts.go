// Package hhapi is the ONLY place that talks HTTP to hh.ru.
// Endpoint URLs come from config (overridable); nothing else in the codebase
// may hardcode hh.ru URLs. Contracts below are consumed by feature packages.
package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Typed errors map to anti-bot/session states; callers branch on these.
var (
	ErrRateLimited = errors.New("hhapi: 429 rate limited")
	ErrCaptcha     = errors.New("hhapi: captcha challenge")
	ErrLoggedOut   = errors.New("hhapi: session expired")
	ErrForbidden   = errors.New("hhapi: 403")
	ErrValidation  = errors.New("hhapi: 400/422 validation")
	ErrNotFound    = errors.New("hhapi: not found")
	ErrUpstream    = errors.New("hhapi: 5xx")
	ErrNetwork     = errors.New("hhapi: network")
)

// User is the authenticated account owner (hh /me payload).
type User struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
}

// Resume is one resume as hh reports it (mine list view).
type Resume struct {
	HHID       string          `json:"hh_id"` // resume hash
	Title      string          `json:"title"`
	Status     string          `json:"status"`
	Percent    int             `json:"percent"`
	Searchable bool            `json:"searchable"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Raw        json.RawMessage `json:"raw,omitempty"`

	// T28 lineage fields (native clone discovery): the resume this one was
	// cloned from (hh sets it on native clones) and the creation instant.
	// Both empty/zero for resumes hh reports without those attributes, so
	// the serialized shape stays backwards compatible.
	ParentResumeID string    `json:"parent_resume_id,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitempty"`

	// T36 server lift timer: hh reports when the resume may be lifted next
	// (_attributes.nextTouchAt, ms epoch or ISO string). Zero = the field
	// was absent/null for this resume.
	NextTouchAt time.Time `json:"next_touch_at,omitempty"`
}

// Salary is the vacancy salary band as published.
type Salary struct {
	From     int    `json:"from,omitempty"`
	To       int    `json:"to,omitempty"`
	Currency string `json:"currency,omitempty"`
	Gross    bool   `json:"gross,omitempty"`
}

// Vacancy is one vacancy card as parsed from card or search payloads.
type Vacancy struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	EmployerID   int64     `json:"employer_id"`
	EmployerName string    `json:"employer_name"`
	Badges       []string  `json:"badges,omitempty"` // employer badge labels from the raw (search items: company.badges, T70)
	Area         string    `json:"area"`
	Salary       *Salary   `json:"salary,omitempty"`
	PublishedAt  time.Time `json:"published_at"`
	Description  string    `json:"description,omitempty"`
	// Search-snippet substance (T81): filled by vacancyFromItem from the
	// SSR search item's snippet node; the scorer decides on these.
	SnippetResponsibility string          `json:"snippet_responsibility,omitempty"`
	SnippetRequirement    string          `json:"snippet_requirement,omitempty"`
	SnippetSkill          string          `json:"snippet_skill,omitempty"`
	AlternateURL          string          `json:"alternate_url,omitempty"`
	EmployerOnline        bool            `json:"employer_online,omitempty"` // manager activity (SSR)
	AcceptTemp            bool            `json:"accept_temp,omitempty"`
	HasTests              bool            `json:"has_tests,omitempty"`
	Archived              bool            `json:"archived,omitempty"` // card payload only (T89 re-exam)
	Raw                   json.RawMessage `json:"raw,omitempty"`
}

// VacancyPage is one search result page.
type VacancyPage struct {
	Items []Vacancy `json:"items"`
	Page  int       `json:"page"`
	Pages int       `json:"pages"`
	Found int       `json:"found"`
}

// Employer is one employer card payload.
type Employer struct {
	ID      int64           `json:"id"`
	Name    string          `json:"name"`
	SiteURL string          `json:"site_url,omitempty"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// EmployerRating carries both all-time and recent-window scores (filter WF2-F4).
type EmployerRating struct {
	EmployerID   int64     `json:"employer_id"`
	ScoreAll     float64   `json:"score_all"`
	ScoreRecent  float64   `json:"score_recent"` // within recent window (e.g. last3Months)
	ReviewsCount int       `json:"reviews_count"`
	RecommendPct int       `json:"recommend_percent"`
	FetchedAt    time.Time `json:"fetched_at"`
	// ReviewsCountRecent mirrors last3MonthsReviewsInfo.reviewsCount from the
	// live widget (0 when the block is absent). See parseEmployerRating for
	// the ScoreRecent window heuristic.
	ReviewsCountRecent int `json:"reviews_count_recent,omitempty"`
}

// ApplyMeta is the response-popup metadata that gates the apply flow.
// Live payload: a JSON envelope whose structured part lives in
// `responseStatus` (test / shortVacancy / resumeInconsistencies dict /
// alreadyApplied / responseImpossible) plus `responsePopup` — see
// testdata/live/popup.json.
type ApplyMeta struct {
	HasTests bool `json:"has_tests"`
	// TestRequired is hh's own popup refusal {"type":"test-required"} —
	// the T83 live form of a test-gated vacancy (no responseStatus modal
	// at all). Same handling as HasTests.
	TestRequired          bool     `json:"test_required,omitempty"`
	LetterRequired        bool     `json:"letter_required"` // shortVacancy.@responseLetterRequired
	ResumeInconsistencies []string `json:"resume_inconsistencies,omitempty"`
	AcceptAutoResponse    bool     `json:"accept_auto_response,omitempty"`   // shortVacancy.autoResponse
	ChatWritePossibility  string   `json:"chat_write_possibility,omitempty"` // shortVacancy.chatWritePossibility
	// Structural gates surfaced by the live popup envelope.
	AlreadyApplied     bool `json:"already_applied,omitempty"`
	ResponseImpossible bool `json:"response_impossible,omitempty"`
}

// ApplyRequest is the input of a vacancy response submission.
type ApplyRequest struct {
	VacancyID  int64
	ResumeHash string
	Letter     string
	// Test (T80) carries the solved vacancy test: when set, the apply POST
	// includes the test submission fields (the same /applicant/vacancy_response/popup
	// endpoint answers test-gated vacancies).
	Test *ApplyTest
}

// NegotiationRef is the handle returned right after an apply.
type NegotiationRef struct {
	ID      string `json:"id"`
	Vacancy int64  `json:"vacancy"`
	State   string `json:"state"`
}

// Negotiation is one applicant negotiation from the negotiations list.
type Negotiation struct {
	ID           string    `json:"id"`
	VacancyID    int64     `json:"vacancy_id"`
	VacancyName  string    `json:"vacancy_name"`
	EmployerID   int64     `json:"employer_id"`
	EmployerName string    `json:"employer_name"`
	State        string    `json:"state"` // e.g. "refused" => WF7 trigger
	UpdatedAt    time.Time `json:"updated_at"`
	// ChatID is the topic's chatik chat id (live item "chatId"; T123) —
	// the direct key for ChatHistory, no chat listing needed.
	ChatID string `json:"chat_id,omitempty"`
}

// Chat is one chatik conversation header.
type Chat struct {
	ID            string    `json:"id"` // chatik chat id
	TopicID       string    `json:"topic_id,omitempty"`
	VacancyID     int64     `json:"vacancy_id,omitempty"`
	EmployerName  string    `json:"employer_name,omitempty"`
	LastMessageAt time.Time `json:"last_message_at,omitempty"`
	Unread        int       `json:"unread,omitempty"`
	// ApplicantState is the live negotiation state for the chat's topic:
	// resources.negotiation_topics[topic].currentApplicantState, falling
	// back to lastMessage.workflowTransition.applicantState. "DISCARD"
	// marks the structural refusal (RULING R9) — use IsStructuralRefusal.
	ApplicantState string `json:"applicant_state,omitempty"`
}

// IsStructuralRefusal reports whether a negotiation/applicant state marks a
// structural refusal of the application (RULING R9). The live hh enum spells
// it "DISCARD" (chats resources.negotiation_topics, popup
// responseStatus.negotiations.topicList[].lastState); the donor-documented
// "refused" spelling stays accepted for the SSR negotiations surface.
func IsStructuralRefusal(state string) bool {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "DISCARD", "REFUSED":
		return true
	}
	return false
}

// ChatMessage is one message inside a chat history.
type ChatMessage struct {
	ID        string    `json:"id"`
	Author    string    `json:"author,omitempty"`
	Direction string    `json:"direction"` // in | out
	Text      string    `json:"text"`
	SentAt    time.Time `json:"sent_at,omitempty"`
}

// Contact is one employer contact extracted from a card, chat or email.
type Contact struct {
	Type  string `json:"type"` // phone | email | telegram | name | url
	Value string `json:"value"`
}

// SearchQuery mirrors hh vacancy-search filters (native server-side filter F1).
//
// T25 full-filter coverage: the extended fields below are validated upstream
// in internal/config (hh silently ignores unknown values, so the config layer
// is the only defense); the client forwards them as-is.
type SearchQuery struct {
	Text            string
	Areas           []string
	Experience      []string
	Schedule        []string
	Employment      []string
	Industries      []string
	SalaryFrom      int
	Currency        string
	LabelNoAgencies bool
	SearchFields    []string // repeated search_field params: name | company_name | description; empty = param not sent (all hh fields)
	PeriodDays      int
	Page            int
	PerPage         int

	// ProfessionalRoles maps to professional_role (multi): hh role IDs, e.g.
	// "160" (DevOps). Live-verified 2026-09-25.
	ProfessionalRoles []string
	// Metros maps to metro (multi): hh metro-station IDs. Live-verified.
	Metros []string
	// WorkFormats maps to work_format (multi). Allowed values (live-verified
	// 2026-09-25, case-sensitive): "REMOTE", "HYBRID", "ON_SITE".
	// "ONSITE" (no underscore) is silently ignored by hh.
	WorkFormats []string
	// Labels maps to label (multi, OR-semantics; merged with
	// LabelNoAgencies). Known live vocabulary 2026-09-25: with_salary,
	// with_address, not_from_agency, accept_handicapped, accept_kids,
	// accredited_it, accept_labor_contract, night_shifts, low_performance,
	// internship. Free-form by design: hh ships new labels without notice.
	Labels []string
	// Part maps to part (multi): temp | evening | weekend. The values are
	// accepted by hh, but no filtering effect could be verified live
	// (2026-09-25); the UI expresses "Подработка" via catalog pages, not
	// query params. Sent as-is per the full-coverage decision.
	Part []string
	// OnlyWithSalary maps to only_with_salary=true. Live-verified.
	OnlyWithSalary bool
	// OrderBy maps to order_by: relevance | publication_time_desc |
	// salary_desc | salary_asc (validated upstream).
	OrderBy string
}

// HHAPI is the full site contract consumed by features.
type HHAPI interface {
	BootstrapSession(ctx context.Context) error // light authed GET: xsrf + liveness
	Me(ctx context.Context) (*User, error)
	ListResumes(ctx context.Context) ([]Resume, error)
	TouchResume(ctx context.Context, resumeHash string) error
	SuitableVacancies(ctx context.Context, resumeHash string, page int) (VacancyPage, error)
	SearchVacancies(ctx context.Context, q SearchQuery) (VacancyPage, error)
	VacancyCard(ctx context.Context, id int64) (*Vacancy, error)
	EmployerCard(ctx context.Context, id int64) (*Employer, error)
	EmployerRating(ctx context.Context, id int64) (*EmployerRating, error)
	ApplyPopup(ctx context.Context, vacancyID int64) (*ApplyMeta, error)
	// VacancyTests (T80) reads the obligatory test attached to a vacancy
	// from the response page state. ErrVacancyTestUnavailable (or a captcha
	// StatusError) means the test cannot be solved — callers skip.
	VacancyTests(ctx context.Context, vacancyID int64) (*VacancyTest, error)
	Apply(ctx context.Context, req ApplyRequest) (NegotiationRef, error)
	Negotiations(ctx context.Context, page int) ([]Negotiation, error)
	// NegotiationsDiscard (T123) reads the SSR negotiations page with the
	// filter=discard parameter. LIVE FACT (probe 2026-10-10): hh serves the
	// mixed list anyway (filterInUse="active") — callers MUST post-filter by
	// IsStructuralRefusal(State); the parameter stays per the donor contract.
	NegotiationsDiscard(ctx context.Context, page int) ([]Negotiation, error)
	ListChats(ctx context.Context, page int) ([]Chat, error)
	ChatHistory(ctx context.Context, chatID string) ([]ChatMessage, error)
	SendMessage(ctx context.Context, chatID, idempotencyKey, text string) error
	LeaveChat(ctx context.Context, chatID string) error
	EmployerContacts(ctx context.Context, employerID int64) ([]Contact, error)
	// Counts reads the vacancy-search counter shard (live shape:
	// {"counts":1630}). query is the raw URL query string mirroring the
	// search filters (e.g. `text=golang&area=1`); empty means the global
	// counter. RULING R15: sanctioned contract extension (preview counts).
	Counts(ctx context.Context, query string) (int, error)
	// CreateResume creates a fresh resume (T47) and returns its hash only
	// after the resumes shard verifies the new entry. An empty title
	// creates the bare not_finished shell hh's wizard starts from; hh
	// IGNORES title/roleIDs at create time (live-verified 2026-09-28 —
	// content belongs to the wizard / resume improve), so callers must
	// fill content separately. A 2xx create absent from the shard is a
	// typed error: no invented hashes.
	CreateResume(ctx context.Context, title string, roleIDs []string) (newHash string, err error)
	// DetachDuplicateExperience prunes the duplicate experience
	// generations a created resume inherited from the account pool
	// (T99): among the records rendering on the resume it keeps the
	// newest per employer and detaches the rest (edit-route fork +
	// fork delete — the pool keeps every record, other resumes never
	// change). Idempotent; a fresh unpublished shell no-ops.
	DetachDuplicateExperience(ctx context.Context, hash string) (DetachExperienceResult, error)
}

// Implementation notes for the hhapi implementer:
//   - constructor signature: NewClient(cfg config.Account, endpoints config.APIEndpoints,
//     dialer proxy.Dialer, log *slog.Logger) (*Client, error)
//   - import internal/proxy for the dialer (no import cycle: proxy never imports hhapi)
//   - every method above maps 1:1 to documented web endpoints; full URLs come
//     from config.APIEndpoints (+ overrides), never hardcoded beyond defaults.
