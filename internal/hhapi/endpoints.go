package hhapi

import (
	"net"
	"net/url"
	"strings"

	"hhauto/internal/config"
)

// Endpoint names — the keys of config.APIEndpoints.Override. Defaults live
// here; overrides replace the full URL of a named endpoint.
const (
	epSettings   = "settings"    // GET  /applicant/settings (SSR: xsrfToken, session)
	epBootstrap  = "bootstrap"   // GET  /shards/applicant/resumes (JSON: light authed bootstrap)
	epResumes    = "resumes"     // GET  /shards/applicant/resumes (JSON)
	epTouch      = "touch"       // POST /applicant/resumes/touch
	epApplyPopup = "apply_popup" // GET  /applicant/vacancy_response/popup?vacancyId=N
	epApply      = "apply"       // POST /applicant/vacancy_response/popup
	// T80 vacancy-test surface: the response page whose embedded state
	// carries the vacancyTests block (donor-derived contract, fail-closed).
	epVacancyResponse = "vacancy_response" // GET /applicant/vacancy_response?vacancyId=N
	epNegotiations    = "negotiations"     // GET  /applicant/negotiations (SSR topicList)
	epVacancyCard     = "vacancy_card"     // GET  /vacancy/{id} (SSR vacancyView)
	epEmployerCard    = "employer_card"    // GET  /employer/{id} (SSR employerInfo)
	epEmployerRating  = "employer_rating"  // GET  /employer_reviews/proxy_components/small_widget?employerId=N
	epChats           = "chats"            // GET  chatik /chatik/api/chats
	epChatData        = "chat_data"        // GET  chatik /chatik/api/chat_data?chatId=N
	epSend            = "send"             // POST chatik /chatik/api/send
	epChatLeave       = "chat_leave"       // POST chatik /chatik/api/participant_action
	epCounts          = "counts"           // GET  /shards/vacancy/counts (JSON {"counts":N}) — RULING R15
	// T27 builder surface (resume-profile; live reads verified). Overridable
	// via config like every endpoint.
	epResumeGetByHash = "resume_get_by_hash" // GET  /profile/shards/resume/get_by_hash?resumeHash=
	// T51 content mutation — the save contract the real editor UI speaks
	// (captured live 2026-09-29 from the resume_partial_edit flow): one
	// site-host POST per saved field with a flat JSON body. The former
	// resume_update BFF route (/profile/shards/resume/update) is dead for
	// off-browser clients (bare 400 for every payload variant, T29) and is
	// not used by the deployed editor at all. Overridable via config like
	// every endpoint.
	epResumeEdit = "resume_edit" // POST /applicant/resume/edit?resume={hash}&hhtmSource=resume_partial_edit
	// T28 native full clone (live-verified 2026-09-25 — one call creates a
	// complete searchable duplicate; T45 removed the old shell-clone
	// endpoint, this is the only cloning path). The default lives on the
	// resume-profile-front service host and is used verbatim (never
	// regional-host pinned); overridable via config like every endpoint.
	epResumeCloneResumes = "resume_clone_resumes" // POST /profile/shards/resume/clone_resumes
	// T29 resume management surface (visibility + delete; bundle-reversed
	// from the applicant_profile SPA module 91062). Defaults live on the
	// hh.ru site host; overridable via config like every endpoint.
	epResumeVisibilityHide = "resume_visibility_hide" // POST /profile/shards/resume/visibility/hide
	epResumeVisibilityShow = "resume_visibility_show" // POST /profile/shards/resume/visibility/show
	epResumeDelete         = "resume_delete"          // POST resume-profile-front /profile/shards/resume/delete (T47: the site-host twin is edge-faked)
	// T47 resume creation: POST resume-profile-front /profile/shards/resume/create
	// (live-verified 2026-09-28 — the ONLY live create route; see
	// internal/hhapi/resume_create.go).
	epResumeCreate = "resume_create"
	// T56 experience delete (UI-captured live 2026-09-29): the editor's
	// "Удалить" on /profile/edit/experience/{id} replaces the lineage
	// experience pool — ids missing from the array are deleted. Default
	// lives on the resume-profile-front service host; overridable via
	// config like every endpoint.
	epProfileExperienceUpdate = "profile_experience_update" // POST /profile/shards/profile/update
	// T100 account photo album (live-verified 2026-10-09): the artifact
	// list the resume page's photo icon loads ({"images":[…]}); a resume
	// renders its avatar from the per-resume photo field pointing at an
	// album artifact id. Overridable via config like every endpoint.
	epAccountArtifacts = "account_artifacts" // GET /shards/applicant/artifacts/all
	// T118 «Ищу работу» status flip (DONOR-DERIVED — not live-verified;
	// the donor's only wire evidence posts to the resume-profile-front
	// BFF host). Overridable via config like every endpoint.
	epJobSearchStatus = "job_search_status" // POST /profile/shards/user_statuses/job_search_status?status=looking_for_offers
)

// endpointSet resolves endpoint URLs from config with defaults and
// per-name overrides. Immutable after construction.
type endpointSet struct {
	site       string
	chatik     string
	searchPage string // full URL of the vacancy search path
	overrides  map[string]string
}

func newEndpointSet(eps config.APIEndpoints) endpointSet {
	e := endpointSet{
		site:       strings.TrimRight(eps.Site, "/"),
		chatik:     strings.TrimRight(eps.Chatik, "/"),
		searchPage: strings.TrimRight(eps.SearchPage, "/"),
		overrides:  eps.Override,
	}
	if e.site == "" {
		e.site = "https://hh.ru"
	}
	if e.chatik == "" {
		e.chatik = "https://chatik.hh.ru"
	}
	if e.searchPage == "" {
		e.searchPage = e.site + "/search/vacancy"
	}
	if e.overrides == nil {
		e.overrides = map[string]string{}
	}
	return e
}

// siteHost returns the host of the configured site URL for sameSite checks.
func (e endpointSet) siteHost() string {
	if u, err := url.Parse(e.site); err == nil && u.Host != "" {
		return u.Host
	}
	return e.site
}

// hasOverride reports whether the named endpoint was explicitly overridden
// (such URLs are used verbatim, without regional-host rewriting).
func (e endpointSet) hasOverride(name string) bool {
	return strings.TrimSpace(e.overrides[name]) != ""
}

// resolve returns the override URL for name or defaultURL. Overrides keep
// their own scheme+host (pinning never rewrites an explicit override).
func (e endpointSet) resolve(name, defaultURL string) string {
	if o := strings.TrimSpace(e.overrides[name]); o != "" {
		return strings.TrimRight(o, "/")
	}
	return defaultURL
}

// siteDefault is site + path used as the resolve() fallback.
func (e endpointSet) siteDefault(path string) string { return e.site + path }

// resolveExternal returns the override URL for name or defaultURL, used
// verbatim in BOTH cases: the default belongs to a distinct hh service
// host (resume-profile-front.hh.ru), which regional-host pinning must
// never rewrite — pinning exists for hh.ru regional redirects only.
func (e endpointSet) resolveExternal(name, defaultURL string) string {
	if o := strings.TrimSpace(e.overrides[name]); o != "" {
		return strings.TrimRight(o, "/")
	}
	return defaultURL
}

func (e endpointSet) chatikDefault(path string) string { return e.chatik + path }

// sameSite reports whether two hosts belong to the same registrable domain
// (e.g. hh.ru and taganrog.hh.ru). Hosts may carry ports; the port is
// stripped before comparison. Hosts that publicsuffix cannot classify
// (IP literals) are compared by hostname equality.
func sameSite(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, oka := registrable(a)
	rb, okb := registrable(b)
	if !oka || !okb {
		return false
	}
	return ra != "" && ra == rb
}

// registrable returns the eTLD+1 of host (port stripped). ok is false when
// the host has no registrable domain (IP literal, empty).
func registrable(hostPort string) (string, bool) {
	host := hostWithoutPort(hostPort)
	if host == "" {
		return "", false
	}
	et1, err := publicSuffixETLD1(host)
	if err != nil || et1 == "" {
		return host, host != ""
	}
	return et1, true
}

// hostWithoutPort strips the port part of host:port (brackets-aware for IPv6).
func hostWithoutPort(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return hostPort
}
