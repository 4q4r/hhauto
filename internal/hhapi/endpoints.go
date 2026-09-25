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
	epSettings       = "settings"        // GET  /applicant/settings (SSR: xsrfToken, session)
	epBootstrap      = "bootstrap"       // GET  /shards/applicant/resumes (JSON: light authed bootstrap)
	epResumes        = "resumes"         // GET  /shards/applicant/resumes (JSON)
	epTouch          = "touch"           // POST /applicant/resumes/touch
	epApplyPopup     = "apply_popup"     // GET  /applicant/vacancy_response/popup?vacancyId=N
	epApply          = "apply"           // POST /applicant/vacancy_response/popup
	epNegotiations   = "negotiations"    // GET  /applicant/negotiations (SSR topicList)
	epVacancyCard    = "vacancy_card"    // GET  /vacancy/{id} (SSR vacancyView)
	epEmployerCard   = "employer_card"   // GET  /employer/{id} (SSR employerInfo)
	epEmployerRating = "employer_rating" // GET  /employer_reviews/proxy_components/small_widget?employerId=N
	epChats          = "chats"           // GET  chatik /chatik/api/chats
	epChatData       = "chat_data"       // GET  chatik /chatik/api/chat_data?chatId=N
	epSend           = "send"            // POST chatik /chatik/api/send
	epChatLeave      = "chat_leave"      // POST chatik /chatik/api/participant_action
	epCounts         = "counts"          // GET  /shards/vacancy/counts (JSON {"counts":N}) — RULING R15
	epResumeClone    = "resume_clone"    // POST /applicant/resumes/clone (T26; live-verified 2026-09-25)
	// T27 full-clone surface (resume-profile builder; live reads verified,
	// update gated by hh for off-browser clients as of 2026-09-25 — see
	// internal/hhapi/clone.go). Overridable via config like every endpoint.
	epResumeGetByHash = "resume_get_by_hash" // GET  /profile/shards/resume/get_by_hash?resumeHash=
	epResumeUpdate    = "resume_update"      // POST /profile/shards/resume/update
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
