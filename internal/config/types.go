// Package config defines the full TOML configuration tree for hhauto.
// These types are the INTEGRATION CONTRACT shared by every other package:
// implementers must not rename fields or change types without an orchestrator ruling.
package config

// Config is the root of hhauto.toml.
type Config struct {
	General  General                    `toml:"general"`
	Storage  Storage                    `toml:"storage"`
	Log      Log                        `toml:"log"`
	API      API                        `toml:"api"`
	Captcha  map[string]CaptchaProvider `toml:"captcha"`
	Proxy    map[string]Proxy           `toml:"proxy"`
	Accounts map[string]Account         `toml:"accounts"`
	AI       AI                         `toml:"ai"`
	Features Features                   `toml:"features"`
	Pacing   Pacing                     `toml:"pacing"`
	Webhook  map[string]Webhook         `toml:"webhook"`
	Runs     map[string]Run             `toml:"runs"`
	Review   Review                     `toml:"review"`
	Forms    Forms                      `toml:"forms"`
	Email    Email                      `toml:"email"`
	Warmup   Warmup                     `toml:"warmup"`
	Schedule Schedule                   `toml:"schedule"`
	Browser  Browser                    `toml:"browser"`
}

// General carries deployment-wide paths and the dry-run master switch.
type General struct {
	DataDir  string `toml:"data_dir"`
	Timezone string `toml:"timezone"` // IANA name, default for schedules
	DryRun   bool   `toml:"dry_run"`
}

// Storage points at the SQLite database file.
type Storage struct {
	Path          string `toml:"path"`
	BusyTimeoutMS int    `toml:"busy_timeout_ms"`
}

// Log configures the slog logger (level, format, optional file).
type Log struct {
	Level         string `toml:"level"`
	Format        string `toml:"format"` // text | json
	File          string `toml:"file"`
	RedactSecrets bool   `toml:"redact_secrets"` // validator forbids false
}

// Transport selection vocabulary for [api].transport. TransportTLSClient
// routes hh traffic through bogdanfinn/tls-client with a browser-consistent
// TLS fingerprint (hh.ru fingerprintes the JA3/JA3S of the Go crypto/tls
// stack — live-verified 2026-09-25); TransportHTTP keeps the legacy stdlib
// transport as a debugging fallback.
const (
	TransportTLSClient = "tls-client"
	TransportHTTP      = "http"
)

// API selects the hh surface kind and its endpoint map.
type API struct {
	Kind      string       `toml:"kind"` // "web" | "developer"
	Transport string       `toml:"transport"`
	Endpoints APIEndpoints `toml:"endpoints"`
	Developer DeveloperAPI `toml:"developer"`
}

// APIEndpoints holds the base URLs plus per-endpoint overrides.
type APIEndpoints struct {
	Site       string            `toml:"site"`
	Chatik     string            `toml:"chatik"`
	SearchPage string            `toml:"search_page"`
	Override   map[string]string `toml:"override"` // endpoint name -> full URL
}

// DeveloperAPI configures the hh developer-API surface (token via env).
type DeveloperAPI struct {
	BaseURL  string `toml:"base_url"`
	TokenEnv string `toml:"token_env"`
}

// CaptchaProvider is one captcha-solving service endpoint.
type CaptchaProvider struct {
	Protocol string `toml:"protocol"` // "2captcha" | "anticaptcha"
	BaseURL  string `toml:"base_url"`
	KeyEnv   string `toml:"key_env"`
	Key      string `toml:"key"` // discouraged; validator warns
	PollMS   int    `toml:"poll_interval_ms"`
	TimeoutS int    `toml:"timeout_s"`
}

// Proxy is one outbound proxy definition.
type Proxy struct {
	Type    string `toml:"type"` // socks5 | http | https
	Host    string `toml:"host"`
	Port    int    `toml:"port"`
	User    string `toml:"user"`
	PassEnv string `toml:"pass_env"`
	Pass    string `toml:"pass"` // discouraged
}

// Account is one hh.ru account: identity, transport and rate knobs.
type Account struct {
	DisplayName   string            `toml:"display_name"`
	CookieFile    string            `toml:"cookie_file"`
	ProxyRef      string            `toml:"proxy_ref"` // "" = direct (explicitly allowed)
	UserAgent     string            `toml:"user_agent"`
	Headers       map[string]string `toml:"headers"`
	Concurrency   int               `toml:"concurrency"`
	RPS           float64           `toml:"rps"`
	DailyApplyCap int               `toml:"daily_apply_cap"` // default 200
	CaptchaChain  []string          `toml:"captcha_chain"`   // ordered failover
	ProfileFile   string            `toml:"profile_file"`    // overrides global profile.md
}

// AI aggregates the LLM subsystem: budgets, providers, models, tasks, pricing
// and the optional Jev prefilter.
type AI struct {
	DailyBudgetTokens int                   `toml:"daily_budget_tokens"` // 0 = unlimited
	DailyBudgetUSD    float64               `toml:"daily_budget_usd"`
	OnBudgetExceed    string                `toml:"on_budget_exceed"` // needs_human_only | hard_stop
	Providers         map[string]AIProvider `toml:"providers"`
	Models            map[string]AIModel    `toml:"models"`
	Tasks             map[string]AITask     `toml:"tasks"`
	Pricing           map[string]AIPricing  `toml:"pricing"`
	// Jev (T33): optional System-One prefilter in front of the LLM
	// pipeline. Disabled by default — false means full-AI mode. When
	// enabled, enrolled tasks are classified by the Jev-compatible
	// endpoint first; verdicts at or above confidence_threshold are
	// returned without an LLM call, everything else (low confidence,
	// endpoint unavailable, malformed answers) escalates to the LLM.
	// The API key never lives in this file: it is read from the
	// api_key_env environment variable at startup (fail-fast).
	Jev AIJev `toml:"jev"`
}

// AIJev configures the Jev-compatible prefilter endpoint (T33).
type AIJev struct {
	// Enabled gates the whole prefilter (default false = full AI).
	Enabled bool `toml:"enabled"`
	// Endpoint is the Jev-compatible System-One URL; defaults to
	// https://api.typesafe.ai/v1/systemone. Must be an absolute http(s) URL.
	Endpoint string `toml:"endpoint"`
	// APIKeyEnv names the environment variable holding the Bearer key;
	// required (and resolved fail-fast at startup) when Enabled.
	APIKeyEnv string `toml:"api_key_env"`
	// ConfidenceThreshold gates Jev verdicts: >= threshold is decided by
	// Jev, below escalates to the LLM; default 0.85, must be in (0,1].
	ConfidenceThreshold float64 `toml:"confidence_threshold"`
	// Tasks enrolls tasks in the prefilter; allowed values are the
	// classification-only tasks: message_classification,
	// link_detection, contact_extraction.
	Tasks []string `toml:"tasks"`
}

// AIProvider is one LLM API endpoint (one of the three dialects).
type AIProvider struct {
	Dialect   string `toml:"dialect"` // openai-chat | openai-responses | anthropic-messages
	BaseURL   string `toml:"base_url"`
	APIKeyEnv string `toml:"api_key_env"`
	TimeoutS  int    `toml:"timeout_seconds"`
}

// AIModel binds a model profile (params) to a provider.
type AIModel struct {
	Provider         string  `toml:"provider"`
	Model            string  `toml:"model"`
	ReasoningEffort  string  `toml:"reasoning_effort"`
	MaxOutputTokens  int     `toml:"max_output_tokens"`
	Temperature      float64 `toml:"temperature"`
	FallbackModelRef string  `toml:"fallback"`
}

// AITask wires one AI task to a model profile and its prompt.
type AITask struct {
	ModelProfile  string `toml:"model_profile"`
	PromptInline  string `toml:"inline"`
	PromptFile    string `toml:"file"`
	PromptVersion string `toml:"version"`
	CacheTTLHours int    `toml:"cache_ttl_hours"`
	AutoSend      bool   `toml:"auto_send"`
}

// AIPricing is the per-million-token price used for cost accounting.
type AIPricing struct {
	InputPerMTok  float64 `toml:"input_per_mtok_usd"`
	OutputPerMTok float64 `toml:"output_per_mtok_usd"`
}

// Features toggles every automation module.
type Features struct {
	AutoLift         AutoLift        `toml:"auto_lift"`
	MultiApply       MultiApply      `toml:"multiapply"`
	AutoClone        AutoClone       `toml:"autoclone"`
	TestSolve        TestSolve       `toml:"testsolve"`
	ChatOps          ChatOps         `toml:"chatops"`
	Interview        InterviewDetect `toml:"interview"`
	Contacts         Contacts        `toml:"contacts"`
	EmailDupEnabled  bool            `toml:"emaildup"`
	Relevance        Relevance       `toml:"relevance"`
	ResumeVisibility bool            `toml:"resume_visibility"`
	WebsocketPush    bool            `toml:"websocket_push"`
}

// Cover-letter mode vocabulary for runs[].letter_mode (T26 owner decision #6).
const (
	// LetterModeTemplate sends the run's static template verbatim (no AI).
	LetterModeTemplate = "template"
	// LetterModeTemplateAI polishes the template with the AI (template as the
	// mandatory base); falls back to the raw template when the AI is down.
	LetterModeTemplateAI = "template_ai"
	// LetterModeAICompany is the pre-T26 behavior: the AI composes the letter
	// for the company/vacancy; the template (if set) is context + fallback.
	LetterModeAICompany = "ai_company"
)

// Reapply-mode vocabulary for [features.autoclone].reapply_mode (T26 owner
// decision #1): which vacancies a clone run may (re)apply to.
const (
	// ReapplyAll: everything the source returns (static filters still gate).
	ReapplyAll = "all"
	// ReapplyNewOnly: only vacancies no lineage resume has applied to.
	ReapplyNewOnly = "new_only"
	// ReapplySkipRecentRejections: everything except vacancies a lineage
	// resume was rejected from within skip_rejection_days.
	ReapplySkipRecentRejections = "skip_recent_rejections"
)

// AutoClone configures the resume autoclone loop (T26): when a run's vacancy
// pool is exhausted (everything suitable is already applied by the resume
// lineage), a bounded number of clones is created and used for the next
// passes. Lineage is tracked locally (hh exposes no parentResumeId).
type AutoClone struct {
	Enabled bool `toml:"enabled"`
	// MaxClonesPerResume bounds the direct children per lineage resume.
	MaxClonesPerResume int `toml:"max_clones_per_resume"` // default 2
	// MaxCloneDepth bounds clone-of-clone chains (1 = no chained clones).
	MaxCloneDepth int `toml:"max_clone_depth"` // default 1
	// Autolift includes clones in the autolift resume pool (default true).
	Autolift *bool `toml:"autolift"`
	// ReapplyMode gates which vacancies a clone run applies to.
	ReapplyMode string `toml:"reapply_mode"` // all | new_only | skip_recent_rejections
	// SkipRejectionDays is the rejection-recency window for
	// skip_recent_rejections.
	SkipRejectionDays int `toml:"skip_rejection_days"` // default 30
	// FinishTimeoutMin bounds how long a not_finished clone may be polled
	// before the autoclone is parked for review (hh clones start as
	// status="not_finished"; a stuck clone needs a human).
	FinishTimeoutMin int `toml:"finish_timeout_min"` // default 10
	// CloneOriginalAction is the source-resume policy after a successful
	// full clone (+ improve): "keep" (default) leaves the original as-is,
	// "hide" flips its visibility to no_one (reversible), "delete" removes
	// it permanently. An unknown or failed action degrades to keep with a
	// WARN (fail-safe: the original is never lost to an automation bug).
	CloneOriginalAction string `toml:"clone_original_action"` // keep | hide | delete
	// Improve configures the per-field AI improvement of the clone (T29).
	Improve AutoCloneImprove `toml:"improve"`
	// RunOverride carries point overrides for the implicit clone run; the
	// clone run otherwise inherits the parent run wholesale (filters, limits,
	// letter_mode — T26 owner decision #6).
	RunOverride RunOverride `toml:"run_override"`
}

// Clone-original-action vocabulary for [features.autoclone].clone_original_action.
const (
	// CloneActionKeep leaves the source resume untouched (default).
	CloneActionKeep = "keep"
	// CloneActionHide hides the source resume from everyone (visibility
	// no_one) — reversible through show.
	CloneActionHide = "hide"
	// CloneActionDelete permanently deletes the source resume.
	CloneActionDelete = "delete"
)

// AutoCloneImprove configures the per-field AI improvement of a fresh clone
// (T29): every field in Fields gets its own system prompt (builtin default,
// overridable per field through Prompts), the AI proposes a new value from
// the resume facts only, the dry-run diff shows было/стало per field and
// the apply walks the editor update endpoint per screen.
type AutoCloneImprove struct {
	// Enabled gates the whole section (default false — nothing improves
	// until the owner asks for it).
	Enabled bool `toml:"enabled"`
	// Fields lists the field ids to improve ("title", "about",
	// "key_skills", "experience_description", ...). Date fields
	// ("experience_dates") are NOT AI fields by default: improving one
	// requires an explicit owner prompt override for that field.
	Fields []string `toml:"fields"`
	// ModelProfile is the [ai.models] profile the improve task runs on.
	ModelProfile string `toml:"model_profile"`
	// AlwaysAsk lists field ids whose improved values never auto-apply:
	// they park for review with the proposed diff instead.
	AlwaysAsk []string `toml:"always_ask"`
	// Prompts maps a field id to its prompt override ([ai.tasks]-shaped:
	// inline | file, never both; version optional). Only the prompt fields
	// are read here — the model profile always comes from ModelProfile.
	Prompts map[string]AITask `toml:"prompts"`
	// Editor configures the browser (chromedp) apply of the planned values
	// (T30). When enabled and Chrome is available, the plan is applied
	// through the real resume editor pages (browser TLS passes the edge
	// that gates off-browser POSTs); without it the pipeline keeps the
	// T29 builder-update path and its degradation semantics.
	Editor AutoCloneImproveEditor `toml:"editor"`
}

// AutoCloneImproveEditor configures the T30 browser editor runner.
type AutoCloneImproveEditor struct {
	// Enabled gates the browser apply (default false — the builder-update
	// path stays the apply surface).
	Enabled bool `toml:"enabled"`
	// BaseURL is the hh.ru origin the wizard pages are opened on.
	BaseURL string `toml:"base_url"` // default https://hh.ru
	// ScreenURLs maps a wizard screen id to a URL template overriding the
	// default {base}/applicant/resume/{resume}/{screen}. Placeholders:
	// {resume} and {screen} are required everywhere; {item} is required on
	// screens hosting per-item fields (experience).
	ScreenURLs map[string]string `toml:"screen_urls"`
	// CookieDomain is the domain the account session cookies are injected
	// under (hh.ru and its subdomains; nothing outside it ever receives
	// them — the isolation invariant of the browser runner).
	CookieDomain string `toml:"cookie_domain"` // default .hh.ru
	// PageTimeoutSec bounds one whole browser run (all screens of a plan).
	PageTimeoutSec int `toml:"page_timeout_sec"` // default 120
	// SaveTimeoutSec bounds the post-save wait for the wizard to leave the
	// screen; the authoritative gate is the get_by_hash read-back either
	// way.
	SaveTimeoutSec int `toml:"save_timeout_sec"` // default 20
}

// RunOverride replaces inherited parent-run values for the clone run; zero
// fields inherit.
type RunOverride struct {
	MaxPerDay      int    `toml:"max_per_day"`
	LetterMode     string `toml:"letter_mode"`
	LetterTemplate string `toml:"letter_template"`
}

// AutoLift configures resume auto-raising (T36 server_timer): lifts are
// planned from the hh shard's nextTouchAt (falling back to
// updated_at + 4h per resume).
type AutoLift struct {
	Enabled bool   `toml:"enabled"`
	PerDay  int    `toml:"per_day"` // default 6
	Jitter  string `toml:"jitter"`  // e.g. "20-40m"
}

// MultiApply configures the apply pipeline.
type MultiApply struct {
	Enabled            bool `toml:"enabled"`
	AIFilter           bool `toml:"ai_filter"`
	MinMatchPercent    int  `toml:"min_match_percent"`
	ExcludeAgenciesDef bool `toml:"exclude_agencies_default"`
	PrioritizeOnlineHR bool `toml:"prioritize_online_hr"`
	DedupNativeResp    bool `toml:"dedup_native_autoresponse"`
}

// Browser release-channel and humanize-preset vocabularies for [browser].
// Channels mirror the cloakbrowser release layout: the free build is
// public on GitHub Releases, pro builds are marker/cache-resolved
// (licensed downloads are out of scope); auto prefers a cached newer
// marker version and falls back to the pinned free build.
const (
	BrowserPresetDefault = "default"
	BrowserPresetCareful = "careful"

	BrowserChannelAuto = "auto"
	BrowserChannelFree = "free"
	BrowserChannelPro  = "pro"
)

// Browser configures the shared browser subsystem (T32): executable
// discovery, the cloakbrowser chromium cache/auto-download and the
// humanize launch profile shared by the testsolve headless filler and
// the autoclone editor runner.
type Browser struct {
	// AutoDownload lets the provider download the cloakbrowser chromium
	// build when neither HHAUTO_CHROME_PATH nor PATH yield an executable
	// and the cache holds no compatible binary (nil → true).
	AutoDownload *bool `toml:"auto_download"`
	// Humanize enables human-like input in the fillers: typing cadence,
	// micro-delays and a smooth focus before text entry (nil → true).
	Humanize *bool `toml:"humanize"`
	// Preset selects the launch/input profile: careful (default; the
	// conservative stealth flag set plus slower, deliberate pacing) or
	// default (no extra launch flags, normal pacing).
	Preset string `toml:"preset"`
	// Channel selects the cloakbrowser release channel: auto (a cached
	// marker version wins, else the pinned free build), free (the pinned
	// free build only) or pro (pro marker/cache resolution only).
	Channel string `toml:"channel"`
}

// TestSolve configures screening-test detection and filling.
type TestSolve struct {
	Enabled       bool     `toml:"enabled"`
	SolveInternal bool     `toml:"solve_internal"`
	SolveExternal bool     `toml:"solve_external"` // requires Chrome
	FormDomains   []string `toml:"external_form_domains"`
	Mode          string   `toml:"mode"`          // draft_only | headless
	SubmitPolicy  string   `toml:"submit_policy"` // auto | human
}

// ChatOps configures chat reading, replies and auto-leave.
type ChatOps struct {
	Enabled           bool     `toml:"enabled"`
	AutoReply         *bool    `toml:"auto_reply_enabled"` // default true; explicit false respected (R1)
	RejectionMinConf  float64  `toml:"rejection_min_confidence"`
	AutoLeaveRejected bool     `toml:"auto_leave_rejected"` // default false
	LeaveFarewell     string   `toml:"leave_farewell_template"`
	DoNotLeave        []string `toml:"do_not_leave"`
	PollInterval      string   `toml:"poll_interval"`
	QABaseEnabled     bool     `toml:"qa_base"` // reserved: not implemented in v1; Validate rejects true
}

// InterviewDetect configures interview-link detection in chats.
type InterviewDetect struct {
	Enabled     bool     `toml:"enabled"`
	LinkDomains []string `toml:"link_domains"`
}

// Contacts configures employer-contact collection.
type Contacts struct {
	Enabled      bool `toml:"enabled"`
	FromCard     bool `toml:"from_card"`
	FromMessages bool `toml:"from_messages"`
	AutoEmailDup bool `toml:"auto_trigger_emaildup"`
}

// Warmup configures the recruiter follow-up ping ladder.
type Warmup struct {
	Enabled        bool     `toml:"enabled"`
	SilenceHours   int      `toml:"silence_hours"`
	LadderHours    []int    `toml:"ladder_hours"` // e.g. [24,48,96]
	MaxPings       int      `toml:"max_pings"`
	Source         string   `toml:"source"` // list | ai
	PingList       []string `toml:"ping_list"`
	ActiveHours    string   `toml:"active_hours"`
	RespectWeekend *bool    `toml:"respect_weekend"` // default true; explicit false respected
}

// Relevance carries the global relevance threshold.
type Relevance struct {
	MinScore int `toml:"min_match_score"`
}

// Pacing shapes per-account mutation timing (window, mode, jitter).
type Pacing struct {
	ActiveWindow string `toml:"active_window"` // "08:00-22:00"
	Mode         string `toml:"mode"`          // adaptive | uniform | burst
	NightPause   *bool  `toml:"night_pause"`   // default true; explicit false respected
	MinDelaySec  int    `toml:"min_delay_seconds"`
	Jitter       string `toml:"jitter"`
}

// Webhook is one notification endpoint (raw/telegram/discord).
type Webhook struct {
	URL       string   `toml:"url"`
	Preset    string   `toml:"preset"` // raw | telegram | discord
	SecretEnv string   `toml:"secret_env"`
	ProxyRef  string   `toml:"proxy_ref"`
	Events    []string `toml:"events"` // empty = all
	RetryMax  int      `toml:"retry_max"`
	ChatID    string   `toml:"chat_id"` // telegram preset target
}

// Run is one search-and-apply profile: query, windows, filters, letter.
type Run struct {
	Enabled           bool             `toml:"enabled"`
	Account           string           `toml:"account"`
	ResumeID          string           `toml:"resume_id"`
	Title             string           `toml:"title"`
	Query             string           `toml:"query"`
	SearchFields      []string         `toml:"search_fields"` // name | company_name | description; empty = all fields. Live counts for query "python backend" (remote+RU, 2026-09-25): name=28, description=319, all fields=375.
	MaxPerDay         int              `toml:"max_per_day"`
	MinMatchPct       int              `toml:"min_match_percent"`
	Windows           []Window         `toml:"windows"`
	Filters           RunFilters       `toml:"filters"`
	LetterTemplate    string           `toml:"letter_template"`
	LetterMode        string           `toml:"letter_mode"` // template | template_ai | ai_company (default ai_company, T26)
	AIFilterOverrides AIFilterOverride `toml:"ai_filter_overrides"`
}

// Window is one schedule time window inside a run profile.
type Window struct {
	TZ   string   `toml:"tz"`
	Time []string `toml:"time"`
	Days []string `toml:"days"`
}

// RunFilters is the native server-side vacancy filter set.
type RunFilters struct {
	SalaryFrom        int      `toml:"salary_from"`
	SalaryTo          int      `toml:"salary_to"`
	SalaryCurrency    string   `toml:"salary_currency"`
	OnlyWithSalary    bool     `toml:"only_with_salary"`
	Experience        []string `toml:"experience"`
	Schedule          []string `toml:"schedule"`
	TravelTime        string   `toml:"travel_time"`
	Employment        []string `toml:"employment"`
	Industries        []string `toml:"industries"`
	Areas             []string `toml:"areas"`
	SearchPeriodDays  int      `toml:"search_period_days"`
	ExcludeWords      []string `toml:"exclude_words"`
	ExcludeInDesc     bool     `toml:"exclude_in_description"`
	CompanyBlacklist  []string `toml:"company_blacklist"`
	CompanyWhitelist  []string `toml:"company_whitelist"`
	WhitelistBypass   []string `toml:"whitelist_bypass"`
	ExcludeAgencies   bool     `toml:"exclude_agencies"`
	MinEmployerRating float64  `toml:"min_employer_rating"`
	RatingPeriod      string   `toml:"rating_period"` // "all" | "N" (days)
	RatingCacheHours  int      `toml:"rating_cache_hours"`
	NoReviewsPolicy   string   `toml:"no_reviews_policy"` // allow | skip | human

	// --- T25: full hh filter coverage (values live-verified 2026-09-25) ---
	// ProfessionalRoles: hh professional-role IDs, param professional_role
	// (multi).
	ProfessionalRoles []string `toml:"professional_roles"`
	// Metros: hh metro-station IDs, param metro (multi).
	Metros []string `toml:"metros"`
	// WorkFormats: param work_format (multi); enum REMOTE|HYBRID|ON_SITE,
	// case-sensitive ("ONSITE" is silently ignored by hh — rejected here).
	WorkFormats []string `toml:"work_formats"`
	// Labels: param label (multi; merged with exclude_agencies). Free-form:
	// known live values are documented in docs/CONFIGURATION.md; hh ships new
	// labels without notice, so unknown strings pass validation.
	Labels []string `toml:"labels"`
	// Part: param part (multi); enum temp|evening|weekend. hh accepts the
	// values but no filtering effect could be verified live (2026-09-25) —
	// sent per the full-coverage decision.
	Part []string `toml:"part"`
	// OrderBy: param order_by; enum relevance|publication_time_desc|
	// salary_desc|salary_asc; "" = hh default.
	OrderBy string `toml:"order_by"`
}

// AIFilterOverride tweaks the AI relevance filter for one run.
type AIFilterOverride struct {
	MinScore          int    `toml:"min_score"`
	ExtraInstructions string `toml:"extra_instructions"`
}

// Review configures the operator-review SLA.
type Review struct {
	SLADefaultHours int `toml:"sla_hours_default"`
}

// Forms configures external test-form filling.
type Forms struct {
	AllowedDomains       []string `toml:"allowed_domains"`
	Mode                 string   `toml:"mode"` // draft_only | headless
	SubmitPolicy         string   `toml:"submit_policy"`
	AbortOnPasswordField bool     `toml:"abort_on_password_field"`
	PageTimeoutSec       int      `toml:"page_timeout_sec"`
}

// Email configures the follow-up email sender.
type Email struct {
	SMTPHost        string `toml:"smtp_host"`
	SMTPPort        int    `toml:"smtp_port"`
	Username        string `toml:"username"`
	PassEnv         string `toml:"pass_env"`
	From            string `toml:"from"`
	SubjectTemplate string `toml:"subject_template"`
	BodyTemplate    string `toml:"body_template"`
	AttachResume    *bool  `toml:"attach_resume"` // default true; explicit false respected
	RequireAttach   bool   `toml:"require_attachment"`
	PerHourLimit    int    `toml:"limits_per_hour"`
	PerDayLimit     int    `toml:"limits_per_day"`
	Retries         int    `toml:"retries"`
	PerEmployerOnce *bool  `toml:"per_employer_once"` // default true; explicit false respected
}

// Schedule configures engine-wide scheduling behavior.
type Schedule struct {
	JitterMinutes   int    `toml:"jitter_minutes"`
	MissedRunPolicy string `toml:"missed_run_policy"` // skip | run_once
	MaxCatchupHours int    `toml:"max_catchup_hours"`
}
