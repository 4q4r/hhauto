package hhapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

// Client implements HHAPI over the hh.ru web (cookie) surface. It is the
// only component allowed to talk HTTP to hh hosts. Concurrency-safe: one
// Client per account.
type Client struct {
	cfg        config.Account
	eps        endpointSet
	log        *slog.Logger
	httpClient *http.Client   // legacy stdlib transport ([api].transport = "http")
	tls        *tlsTransport  // tls-client transport (default; nil in http mode)
	jar        *cookiejar.Jar // single cookie authority for both transports
	lim        *limiter
	rnd        *rand.Rand
	rndMu      sync.Mutex

	mu        sync.Mutex
	xsrf      string
	persister CookiePersister
	accountID string

	book   cookieBook
	pinned atomic.Pointer[string] // effective site host after regional redirect

	// experienceReplace (T58): delete forked experience generations after
	// a verified apply; wired from [features.autoclone.improve]
	// experience_replace via WithExperienceReplace.
	experienceReplace bool
}

// NewClient builds the per-account web client. dialer comes from the proxy
// registry; nil means explicitly-direct (the fail-closed proxy decision
// belongs to the caller) and only applies to the legacy http transport —
// the default tls-client transport takes its proxy via WithProxyURL.
// cfg.UserAgent is mandatory and exclusive: a UA override via cfg.Headers
// would desynchronize the derived client hints (a classic bot marker), so
// it is rejected up front. The TLS profile is likewise derived from the
// User-Agent (T24: hh.ru fingerprintes the JA3/JA3S of crypto/tls).
func NewClient(cfg config.Account, endpoints config.APIEndpoints, dialer proxy.Dialer, log *slog.Logger, options ...ClientOption) (*Client, error) {
	if strings.TrimSpace(cfg.UserAgent) == "" {
		return nil, errors.New("hhapi: config.Account.UserAgent is required")
	}
	for k := range cfg.Headers {
		if http.CanonicalHeaderKey(k) == "User-Agent" {
			return nil, errors.New("hhapi: config.Account.Headers must not set User-Agent: use Account.UserAgent so derived client hints stay in sync")
		}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var opts clientOptions
	for _, opt := range options {
		opt(&opts)
	}
	jar, err := newCookieJar()
	if err != nil {
		return nil, fmt.Errorf("hhapi: cookie jar: %w", err)
	}
	c := &Client{
		cfg:               cfg,
		eps:               newEndpointSet(endpoints),
		log:               log,
		jar:               jar,
		lim:               newLimiter(cfg.RPS),
		rnd:               rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), rand.Uint64())),
		experienceReplace: opts.experienceReplace,
	}
	switch opts.transport {
	case "", config.TransportTLSClient:
		tp, terr := newTLSTransport(jar, cfg.UserAgent, opts.proxyURL, opts.browserSpec, log)
		if terr != nil {
			return nil, terr
		}
		c.tls = tp
	case config.TransportHTTP:
		if opts.proxyURL != "" {
			return nil, errors.New("hhapi: WithProxyURL needs the tls-client transport: the http transport proxies via its Dialer")
		}
		c.httpClient = &http.Client{
			Transport: newHTTPTransport(dialer),
			Jar:       jar,
			Timeout:   30 * time.Second,
		}
		c.httpClient.CheckRedirect = c.checkRedirect
	default:
		return nil, fmt.Errorf("hhapi: unknown transport %q (want %q or %q)", opts.transport, config.TransportTLSClient, config.TransportHTTP)
	}
	return c, nil
}

// --- Host pinning (M0: hh.ru 302 -> regional subdomain) ---

func (c *Client) pinHost(host string) {
	h := host
	c.pinned.Store(&h)
}

func (c *Client) effectiveHost() string {
	if p := c.pinned.Load(); p != nil {
		return *p
	}
	return ""
}

// endpointURL resolves an endpoint to its final URL, rewriting the host to
// the pinned regional host when the endpoint belongs to the site domain.
// Explicit overrides keep their own host (they were pinned by the operator).
func (c *Client) endpointURL(name, defaultURL string) string {
	raw := c.eps.resolve(name, defaultURL)
	if c.eps.hasOverride(name) {
		return raw // override: used verbatim
	}
	return c.withPinnedHost(raw)
}

// withPinnedHost swaps the host of a site-domain URL to the pinned one.
func (c *Client) withPinnedHost(raw string) string {
	pinned := c.effectiveHost()
	if pinned == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if !sameSite(u.Host, c.eps.siteHost()) || u.Host == pinned {
		return raw
	}
	u.Host = pinned
	return u.String()
}

// --- Request kinds and pipeline ---

type reqKind int

const (
	kindGet      reqKind = iota // reads: retry on network/5xx/429
	kindIdemPost                // chatik send: retryable (idempotencyKey), final 5xx => Ambiguous
	kindMutation                // apply/touch/leave: NEVER auto-retried
)

type requestSpec struct {
	kind      reqKind
	operation string
	method    string
	url       string
	header    http.Header
	body      []byte
}

func (s requestSpec) retryable() bool { return s.kind != kindMutation }

// response is the processed result of one logical request (redirects
// already followed).
type response struct {
	status     int
	header     http.Header
	body       []byte
	finalURL   string
	chain      []RedirectHop
	setCookies []*http.Cookie
	retryAfter time.Duration
}

// maxBodyBytes caps response reads: SSR pages run ~1.5MB; 16MB is generous.
const maxBodyBytes = 16 << 20

// retryAfterGiveUp is the longest Retry-After the client is willing to sleep
// inline. Larger values stop the retry loop and surface the duration on the
// StatusError so the scheduler can pace the account properly.
const retryAfterGiveUp = 30 * time.Second

// do runs the full pipeline: rate limit -> send (with redirect recording)
// -> classify -> retry per kind. It never returns a redirect-followed
// intermediate response, only the final one or a typed error.
func (c *Client) do(ctx context.Context, spec requestSpec) (*response, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := c.lim.wait(ctx); err != nil {
			return nil, fmt.Errorf("hhapi: %s: %w", spec.operation, err)
		}
		resp, err := c.attempt(ctx, spec)
		if err != nil {
			if r, done, ret := c.handleAttemptError(ctx, spec, attempt, err, &lastErr); done {
				return r, ret
			}
			continue
		}

		c.absorbAndPersist(ctx, resp)
		if derr := c.redirectFailure(resp); derr != nil {
			return resp, &StatusError{Kind: derr, URL: spec.url, Chain: resp.chain}
		}
		kind := classifyStatus(resp.status, string(resp.body), resp.header.Get("Location"))
		if kind == nil {
			return resp, nil
		}
		if errors.Is(kind, ErrRateLimited) && resp.retryAfter > retryAfterGiveUp {
			// The server demands a wait longer than we ever sleep inline:
			// surface the instruction to the scheduler instead of stalling
			// the caller or ignoring the header. Resolve state by reading.
			return resp, &StatusError{
				Kind:        kind,
				StatusCode:  resp.status,
				URL:         spec.url,
				BodySnippet: snippet(resp.body),
				Chain:       resp.chain,
				RetryAfter:  resp.retryAfter,
			}
		}
		lastErr = &StatusError{
			Kind:        kind,
			StatusCode:  resp.status,
			URL:         spec.url,
			BodySnippet: snippet(resp.body),
			Chain:       resp.chain,
			RetryAfter:  resp.retryAfter,
		}
		if !retryableStatus(resp.status) || attempt+1 >= maxAttempts || !spec.retryable() {
			break
		}
		if werr := sleepCtx(ctx, c.retryWait(attempt, lastErr.(*StatusError))); werr != nil {
			return nil, werr
		}
	}
	return nil, c.finalError(spec, lastErr)
}

// handleAttemptError applies the transport-failure rules for one failed
// attempt. It returns done=true with the terminal (resp, err) pair when the
// retry loop must exit immediately, and done=false when the caller should
// proceed to the next attempt (the backoff sleep already happened).
func (c *Client) handleAttemptError(ctx context.Context, spec requestSpec, attempt int, err error, lastErr *error) (*response, bool, error) {
	var off *offsiteRedirectError
	if errors.As(err, &off) {
		// The fence fired inside CheckRedirect: net/http closed the
		// response, retrying cannot change the answer, and the
		// redirect itself is the finding (soft block / SSO bounce).
		return nil, true, &StatusError{
			Kind:        ErrForbidden,
			URL:         spec.url,
			Chain:       off.chain,
			BodySnippet: "redirect left the site registrable domain",
		}
	}
	*lastErr = err
	if !spec.retryable() {
		// The request went on the wire; a transport failure leaves
		// the outcome unknown unless the dial never completed.
		if isDialError(err) {
			return nil, true, &StatusError{Kind: ErrNetwork, URL: spec.url, BodySnippet: redactSnippet(err.Error())}
		}
		return nil, true, &AmbiguousError{Err: err}
	}
	if attempt+1 < maxAttempts {
		if werr := sleepCtx(ctx, c.jitter(attempt)); werr != nil {
			return nil, true, werr
		}
		return nil, false, nil
	}
	return nil, true, &StatusError{Kind: ErrNetwork, URL: spec.url, BodySnippet: redactSnippet(err.Error())}
}

// finalError applies the ambiguous-outcome rules after the attempt budget
// is spent or a non-retryable condition stopped the loop.
func (c *Client) finalError(spec requestSpec, lastErr error) error {
	var se *StatusError
	if errors.As(lastErr, &se) {
		switch {
		case spec.kind == kindIdemPost && se.StatusCode >= 500:
			// Donor-verified semantics: a 5xx after chatik send means the
			// message state is unknown even though the key makes a repeat
			// safe — report ambiguous so the caller reconciles by reading.
			return &AmbiguousError{Err: se}
		case spec.kind == kindMutation && (se.StatusCode == http.StatusBadGateway || se.StatusCode == http.StatusGatewayTimeout):
			return &AmbiguousError{Err: se}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("hhapi: request failed")
	}
	return lastErr
}

// redirectFailure maps followed-redirect markers to typed errors: the chain
// is the only reliable signal when the final page itself looks innocent.
func (c *Client) redirectFailure(resp *response) error {
	for _, hop := range resp.chain {
		if kind := redirectKind(hop.Location); kind != nil {
			return kind
		}
	}
	return nil
}

func (c *Client) jitter(attempt int) time.Duration {
	c.rndMu.Lock()
	defer c.rndMu.Unlock()
	return fullJitter(attempt, c.rnd)
}

// retryWait honors Retry-After for 429, falling back to exponential jitter.
// Waits are capped at retryAfterGiveUp; anything longer trips the give-up
// branch in do() instead (see Test429RetryAfterBeyondCapStopsRetrying).
func (c *Client) retryWait(attempt int, se *StatusError) time.Duration {
	if se != nil && se.RetryAfter > 0 {
		return min(se.RetryAfter, retryAfterGiveUp)
	}
	return c.jitter(attempt)
}

// attempt performs one physical round trip with redirect recording. The
// transport branch lives here so retry/backoff/limiter/typed-error handling
// stays shared: the default tls-client transport follows redirects manually
// (attemptTLS) because tls-client offers no per-hop control hooks, while the
// legacy http transport leans on CheckRedirect.
func (c *Client) attempt(ctx context.Context, spec requestSpec) (*response, error) {
	if c.tls != nil {
		return c.attemptTLS(ctx, spec)
	}
	chainCtx, chain := withRedirectChain(ctx)
	req, err := http.NewRequestWithContext(chainCtx, spec.method, spec.url, bytes.NewReader(spec.body))
	if err != nil {
		return nil, fmt.Errorf("hhapi: build request: %w", err)
	}
	for k, vv := range spec.header {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }() // body consumed below; close error is not actionable
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("hhapi: read body: %w", err)
	}
	return &response{
		status:     httpResp.StatusCode,
		header:     httpResp.Header,
		body:       body,
		finalURL:   httpResp.Request.URL.String(),
		chain:      chain.snapshot(),
		setCookies: httpResp.Cookies(),
		retryAfter: retryAfterFrom(httpResp.Header),
	}, nil
}

// retryAfterFrom parses the Retry-After response header verbatim (no cap):
// give-up decisions belong to the retry policy, not the parser.
func retryAfterFrom(h http.Header) time.Duration {
	if d, ok := parseRetryAfter(h.Get("Retry-After")); ok {
		return d
	}
	return 0
}

func snippet(b []byte) string {
	const n = 160
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n]
	}
	return redactSnippet(s)
}

// withXSRF stamps the X-XSRFToken header onto a mutation spec.
func (c *Client) withXSRF(spec requestSpec) requestSpec {
	if tok := c.xsrfToken(); tok != "" {
		spec.header.Set("X-XSRFToken", tok)
	}
	return spec
}

// doMutation runs a mutation with the single sanctioned XSRF-refresh retry:
// a 403 carrying an xsrf signature is a definitive server rejection (no
// side effect), so refreshing the token and trying once more is safe.
func (c *Client) doMutation(ctx context.Context, build func() requestSpec) (*response, error) {
	resp, err := c.do(ctx, c.withXSRF(build()))
	if !errors.Is(err, ErrXSRF) {
		return resp, err
	}
	c.log.Debug("hhapi: xsrf rejected, refreshing once", "op", build().operation)
	if rerr := c.refreshXSRF(ctx); rerr != nil {
		return nil, rerr
	}
	return c.do(ctx, c.withXSRF(build()))
}

// --- Headers ---

// addBaseHeaders applies UA + derived client hints + language, then the
// account's explicit header overrides win for any key they set.
func (c *Client) addBaseHeaders(h http.Header) {
	ua := c.cfg.UserAgent
	h.Set("User-Agent", ua)
	h.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
	if brands, mobile, platform, ok := parseClientHints(ua); ok {
		h.Set("Sec-Ch-Ua", brands)
		h.Set("Sec-Ch-Ua-Mobile", mobile)
		h.Set("Sec-Ch-Ua-Platform", platform)
	}
	for k, v := range c.cfg.Headers {
		h.Set(k, v)
	}
}

// docHeaders mimics a top-level navigation.
func (c *Client) docHeaders(referer string) http.Header {
	h := http.Header{}
	c.addBaseHeaders(h)
	// Browser navigation Accept, verified live (T22, 2026-09-25): hh.ru answers
	// application/json with 406 and this exact form with the SSR page.
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	h.Set("Upgrade-Insecure-Requests", "1")
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-User", "?1")
	if referer == "" {
		h.Set("Sec-Fetch-Site", "none")
	} else {
		h.Set("Sec-Fetch-Site", "same-origin")
		h.Set("Referer", referer)
	}
	return h
}

// xhrHeaders mimics a same-origin fetch() call (shards, popup, widgets).
func (c *Client) xhrHeaders(referer string) http.Header {
	h := http.Header{}
	c.addBaseHeaders(h)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	if referer != "" {
		h.Set("Referer", referer)
	}
	return h
}

// chatikHeaders carries the chatik origin triple verified in the wild:
// Origin: chatik base, Referer: chatik base/, X-XSRFToken: _xsrf value.
func (c *Client) chatikHeaders(method string) http.Header {
	h := http.Header{}
	c.addBaseHeaders(h)
	h.Set("Accept", "application/json, */*")
	h.Set("Origin", c.eps.chatik)
	h.Set("Referer", c.eps.chatik+"/")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	if tok := c.xsrfToken(); tok != "" {
		h.Set("X-XSRFToken", tok)
	}
	if method != http.MethodGet {
		h.Set("Content-Type", "application/json")
	}
	return h
}

// --- GET plumbing ---

func (c *Client) getDoc(ctx context.Context, op, target, referer string) (*response, error) {
	return c.do(ctx, requestSpec{
		kind: kindGet, operation: op, method: http.MethodGet,
		url: target, header: c.docHeaders(referer),
	})
}

func (c *Client) getXHR(ctx context.Context, op, target, referer string) (*response, error) {
	return c.do(ctx, requestSpec{
		kind: kindGet, operation: op, method: http.MethodGet,
		url: target, header: c.xhrHeaders(referer),
	})
}

// --- HHAPI: reads ---

// ListResumes implements hhapi.HHAPI: fetches and parses the resume list shard.
func (c *Client) ListResumes(ctx context.Context) ([]Resume, error) {
	resp, err := c.getXHR(ctx, "list_resumes", c.endpointURL(epResumes, c.eps.siteDefault("/shards/applicant/resumes")), c.withPinnedHost(c.eps.site+"/applicant/resumes"))
	if err != nil {
		return nil, err
	}
	return parseResumes(resp.body)
}

// SuitableVacancies reads the /applicant/resumes SSR state: per-resume
// suitable-vacancy blocks under applicantSuitableVacancyByResume. HH serves
// the whole list at once, so page slices it client-side (documented
// deviation from server-side pagination).
func (c *Client) SuitableVacancies(ctx context.Context, resumeHash string, page int) (VacancyPage, error) {
	target := c.withPinnedHost(c.eps.site + "/applicant/resumes")
	resp, err := c.getDoc(ctx, "suitable_vacancies", target, c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		return VacancyPage{}, err
	}
	state, ok := initialState(resp.body)
	if !ok {
		return VacancyPage{}, fmt.Errorf("hhapi: suitable_vacancies: %w", errNilState)
	}
	byResume := obj(state["applicantSuitableVacancyByResume"])
	if byResume == nil {
		return VacancyPage{}, fmt.Errorf("hhapi: suitable_vacancies: %w (applicantSuitableVacancyByResume)", errNilState)
	}
	entry := obj(byResume[resumeHash])
	if entry == nil {
		// Tolerate single-block payloads without hash keys.
		for _, raw := range byResume {
			e := obj(raw)
			if e != nil && (obj(e["searchCounts"]) != nil || arr(e["vacancies"]) != nil) {
				entry = e
				break
			}
		}
	}
	if entry == nil {
		return VacancyPage{}, fmt.Errorf("hhapi: suitable_vacancies: %w (resume %s)", errNilState, resumeHash)
	}
	items := arr(entry["vacancies"])
	found := int(num(obj(entry["searchCounts"]), "value", "totalResults", "found"))
	if found == 0 {
		found = len(items)
	}
	out := VacancyPage{Page: page, Found: found}
	perPage := 50
	start, end := page*perPage, (page+1)*perPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	for _, raw := range items[start:end] {
		out.Items = append(out.Items, vacancyFromItem(raw))
	}
	if perPage > 0 {
		out.Pages = (len(items) + perPage - 1) / perPage
	}
	return out, nil
}

// vacancyFromItem maps one hh vacancy block (search item or vacancyView)
// onto the contract type. Keys drift between surfaces; every lookup is
// tolerant and falls back across spellings.
func vacancyFromItem(raw json.RawMessage) Vacancy {
	m := obj(raw)
	if m == nil {
		return Vacancy{}
	}
	employer := obj(m["employer"])
	if employer == nil {
		// T71: the SSR search item names the employer block "company"
		// (live-probed 2026-09-30); the legacy "employer" shape still
		// surfaces on some pages. Without the fallback every search
		// vacancy parsed EmployerID=0 and collapsed the per-employer
		// daily quota into one shared bucket.
		employer = obj(m["company"])
	}
	area := obj(m["area"])
	comp := obj(m["compensation"])
	manager := obj(m["employerManager"])
	snippet := obj(m["snippet"])
	v := Vacancy{
		ID:                    int64(num(m, "vacancyId", "id")),
		Name:                  str(m, "name"),
		EmployerID:            int64(num(employer, "id")),
		EmployerName:          str(employer, "name"),
		Area:                  str(area, "name"),
		PublishedAt:           timeFrom(m, "publishedAt", "published_at", "createDateTime"),
		Description:           str(m, "description", "descriptionText"),
		SnippetResponsibility: snippetText(snippet, "resp"),
		SnippetRequirement:    snippetText(snippet, "req"),
		SnippetSkill:          snippetText(snippet, "skill"),
		AlternateURL:          firstNonEmpty(str(m, "alternate_url", "alternateURL"), str(obj(obj(m["links"])["desktop"]), "alternate_url")),
		AcceptTemp:            boolv(m, "acceptTemporary", "accept_temp"),
		HasTests:              boolv(m, "hasTest", "hasTests", "has_test"),
		Badges:                Badges(raw),
		Raw:                   raw,
	}
	if comp != nil {
		sal := &Salary{
			From:     int(num(comp, "from")),
			To:       int(num(comp, "to")),
			Currency: str(comp, "currencyCode", "currency"),
			Gross:    boolv(comp, "gross"),
		}
		if sal.From > 0 || sal.To > 0 || sal.Currency != "" {
			v.Salary = sal
		}
	}
	if la := str(manager, "latestActivity"); la == "online" {
		v.EmployerOnline = true
	}
	return v
}

// SearchVacancies implements hhapi.HHAPI: runs one SSR search page and parses
// the embedded HH-Lux-InitialState payload.
func (c *Client) SearchVacancies(ctx context.Context, q SearchQuery) (VacancyPage, error) {
	target := c.searchURL(q)
	resp, err := c.getDoc(ctx, "search_vacancies", target, c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		return VacancyPage{}, err
	}
	state, ok := initialState(resp.body)
	if !ok {
		return VacancyPage{}, fmt.Errorf("hhapi: search_vacancies: %w", errNilState)
	}
	vsr := obj(state["vacancySearchResult"])
	if vsr == nil {
		return VacancyPage{}, fmt.Errorf("hhapi: search_vacancies: %w (vacancySearchResult)", errNilState)
	}
	items := arr(vsr["vacancies"])
	found := int(num(obj(state["searchCounts"]), "value", "totalResults"))
	if found == 0 {
		found = len(items)
	}
	perPage := q.PerPage
	if perPage <= 0 {
		perPage = 50
	}
	page := q.Page
	out := VacancyPage{Items: make([]Vacancy, 0, len(items)), Page: page, Found: found, Pages: (found + perPage - 1) / perPage}
	for _, raw := range items {
		out.Items = append(out.Items, vacancyFromItem(raw))
	}
	return out, nil
}

// searchURL maps SearchQuery onto the hh.ru search-page query parameters.
// SearchFields repeats the search_field param in order; an empty list sends
// no search_field (hh searches all fields). Quoting is the caller's query
// responsibility — the hh query syntax supports "..." inside text.
func (c *Client) searchURL(q SearchQuery) string {
	v := url.Values{}
	v.Set("text", q.Text)
	multi := func(key string, vals []string) {
		for _, val := range vals {
			if val != "" {
				v.Add(key, val)
			}
		}
	}
	multi("area", q.Areas)
	multi("experience", q.Experience)
	multi("schedule", q.Schedule)
	multi("employment", q.Employment)
	multi("industry", q.Industries)
	multi("search_field", q.SearchFields)
	if q.SalaryFrom > 0 {
		v.Set("salary", strconv.Itoa(q.SalaryFrom))
	}
	if q.Currency != "" {
		v.Set("currency", q.Currency)
	}
	// Labels: repeated label= params (hh applies OR-semantics inside a param
	// family). not_from_agency is the live name for "без агентств" — the
	// legacy no_agencies spelling is silently ignored (live-verified
	// 2026-09-25).
	if q.LabelNoAgencies {
		v.Add("label", "not_from_agency")
	}
	multi("label", q.Labels)
	multi("professional_role", q.ProfessionalRoles)
	multi("metro", q.Metros)
	multi("work_format", q.WorkFormats)
	multi("part", q.Part)
	if q.OnlyWithSalary {
		v.Set("only_with_salary", "true")
	}
	if q.OrderBy != "" {
		v.Set("order_by", q.OrderBy)
	}
	if q.PeriodDays > 0 {
		v.Set("search_period", strconv.Itoa(q.PeriodDays))
	}
	page := q.Page
	if page < 0 {
		page = 0
	}
	v.Set("page", strconv.Itoa(page))
	perPage := q.PerPage
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 100 {
		perPage = 100
	}
	v.Set("items_on_page", strconv.Itoa(perPage))
	return c.withPinnedHost(c.eps.searchPage) + "?" + v.Encode()
}

// VacancyCard implements hhapi.HHAPI: fetches one vacancy card by id.
func (c *Client) VacancyCard(ctx context.Context, id int64) (*Vacancy, error) {
	target := c.endpointURL(epVacancyCard, c.eps.siteDefault("/vacancy")) + "/" + strconv.FormatInt(id, 10)
	referer := c.withPinnedHost(c.eps.searchPage)
	resp, err := c.getDoc(ctx, "vacancy_card", target, referer)
	if err != nil {
		return nil, err
	}
	state, ok := initialState(resp.body)
	if !ok {
		return nil, fmt.Errorf("hhapi: vacancy_card %d: %w", id, errNilState)
	}
	view := obj(state["vacancyView"])
	if view == nil {
		return nil, fmt.Errorf("hhapi: vacancy_card %d: %w (vacancyView)", id, errNilState)
	}
	vb, jerr := json.Marshal(view)
	if jerr != nil {
		return nil, fmt.Errorf("hhapi: vacancy_card %d: %w", id, jerr)
	}
	v := vacancyFromItem(vb)
	v.ID = id
	return &v, nil
}

// EmployerCard implements hhapi.HHAPI: fetches one employer card by id.
func (c *Client) EmployerCard(ctx context.Context, id int64) (*Employer, error) {
	target := c.endpointURL(epEmployerCard, c.eps.siteDefault("/employer")) + "/" + strconv.FormatInt(id, 10)
	resp, err := c.getDoc(ctx, "employer_card", target, c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		return nil, err
	}
	state, ok := initialState(resp.body)
	if !ok {
		return nil, fmt.Errorf("hhapi: employer_card %d: %w", id, errNilState)
	}
	info := obj(state["employerInfo"])
	if info == nil {
		return nil, fmt.Errorf("hhapi: employer_card %d: %w (employerInfo)", id, errNilState)
	}
	return &Employer{
		ID:      int64(num(info, "employerId", "id")),
		Name:    str(info, "name", "employerName"),
		SiteURL: str(info, "site", "site_url", "url"),
		Raw:     state["employerInfo"],
	}, nil
}

// EmployerRating implements hhapi.HHAPI: reads the employer's rating widget
// (score, recommendation share, review counts). See parseEmployerRating for
// the live envelope and the ScoreRecent window heuristic.
func (c *Client) EmployerRating(ctx context.Context, id int64) (*EmployerRating, error) {
	base := c.endpointURL(epEmployerRating, c.eps.siteDefault("/employer_reviews/proxy_components/small_widget"))
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("hhapi: employer_rating: %w", err)
	}
	q := u.Query()
	q.Set("employerId", strconv.FormatInt(id, 10))
	u.RawQuery = q.Encode()
	resp, err := c.getXHR(ctx, "employer_rating", c.withPinnedHost(u.String()), c.withPinnedHost(c.eps.site+"/employer/"+strconv.FormatInt(id, 10)))
	if err != nil {
		return nil, err
	}
	return parseEmployerRating(resp.body, id, time.Now())
}

// Negotiations implements hhapi.HHAPI: lists the applicant negotiations.
func (c *Client) Negotiations(ctx context.Context, page int) ([]Negotiation, error) {
	u, err := url.Parse(c.withPinnedHost(c.endpointURL(epNegotiations, c.eps.siteDefault("/applicant/negotiations"))))
	if err != nil {
		return nil, fmt.Errorf("hhapi: negotiations: %w", err)
	}
	if page > 0 {
		q := u.Query()
		q.Set("page", strconv.Itoa(page))
		u.RawQuery = q.Encode()
	}
	resp, err := c.getDoc(ctx, "negotiations", u.String(), c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		return nil, err
	}
	state, ok := initialState(resp.body)
	if !ok {
		return nil, fmt.Errorf("hhapi: negotiations: %w", errNilState)
	}
	an := obj(state["applicantNegotiations"])
	if an == nil {
		return nil, fmt.Errorf("hhapi: negotiations: %w (applicantNegotiations)", errNilState)
	}
	topics := arr(an["topicList"])
	out := make([]Negotiation, 0, len(topics))
	for _, raw := range topics {
		m := obj(raw)
		employer := obj(m["employer"])
		employerID := int64(0)
		if employer != nil {
			employerID = int64(num(employer, "id"))
		}
		out = append(out, Negotiation{
			ID:           str(m, "id", "topicId"),
			VacancyID:    int64(num(m, "vacancyId", "vacancy_id")),
			VacancyName:  firstNonEmpty(str(m, "vacancyName", "vacancy_name"), str(obj(m["vacancy"]), "name")),
			EmployerID:   employerID,
			EmployerName: firstNonEmpty(str(m, "employerName"), str(employer, "name")),
			State:        firstNonEmpty(str(m, "lastState", "state"), str(obj(m["state"]), "id")),
			UpdatedAt:    timeFrom(m, "updatedAt", "updated_at", "lastEventDate"),
		})
	}
	return out, nil
}

// --- HHAPI: mutations ---

// TouchResume implements hhapi.HHAPI: bumps resume activity so hh ranks the
// resume as recently updated.
func (c *Client) TouchResume(ctx context.Context, resumeHash string) error {
	resp, err := c.doMutation(ctx, func() requestSpec {
		form := url.Values{}
		if resumeHash != "" {
			form.Set("resume", resumeHash)
		}
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/x-www-form-urlencoded")
		return requestSpec{
			kind: kindMutation, operation: "touch_resume",
			method: http.MethodPost,
			url:    c.endpointURL(epTouch, c.eps.siteDefault("/applicant/resumes/touch")),
			header: h,
			body:   []byte(form.Encode()),
		}
	})
	if err != nil {
		return err
	}
	// JSON captcha verdicts ride a 200 (donor-verified hhcaptcha.isBot).
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	return nil
}

// ApplyPopup implements hhapi.HHAPI: fetches the response-popup metadata
// (form hints and already-applied state) before an apply. The live payload
// is a JSON envelope — see parseApplyPopup.
func (c *Client) ApplyPopup(ctx context.Context, vacancyID int64) (*ApplyMeta, error) {
	base := c.endpointURL(epApplyPopup, c.eps.siteDefault("/applicant/vacancy_response/popup"))
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("hhapi: apply_popup: %w", err)
	}
	q := u.Query()
	q.Set("vacancyId", strconv.FormatInt(vacancyID, 10))
	u.RawQuery = q.Encode()
	resp, err := c.getXHR(ctx, "apply_popup", c.withPinnedHost(u.String()), c.vacancyCardURL(vacancyID))
	if err != nil {
		return nil, err
	}
	meta, perr := parseApplyPopup(resp.body, vacancyID)
	if perr != nil {
		var se *StatusError
		if errors.As(perr, &se) && errors.Is(se.Kind, ErrRateLimited) {
			// Enrich the transport context the pure parser cannot know.
			se.StatusCode = resp.status
			se.URL = resp.finalURL
			se.BodySnippet = snippet(resp.body)
			se.Chain = resp.chain
		}
		return nil, perr
	}
	return meta, nil
}

// Apply implements hhapi.HHAPI: submits the vacancy response with the
// idempotency key; AmbiguousError on transport loss after send.
func (c *Client) Apply(ctx context.Context, req ApplyRequest) (NegotiationRef, error) {
	resp, err := c.doMutation(ctx, func() requestSpec {
		form := url.Values{}
		form.Set("resume_hash", req.ResumeHash)
		form.Set("vacancy_id", strconv.FormatInt(req.VacancyID, 10))
		if req.Letter != "" {
			form.Set("letterRequired", "true")
			form.Set("letter", req.Letter)
		}
		h := c.xhrHeaders(c.popupReferer(req.VacancyID))
		h.Set("Content-Type", "application/x-www-form-urlencoded")
		return requestSpec{
			kind: kindMutation, operation: "apply",
			method: http.MethodPost,
			url:    c.withPinnedHost(c.endpointURL(epApply, c.eps.siteDefault("/applicant/vacancy_response/popup"))),
			header: h,
			body:   []byte(form.Encode()),
		}
	})
	if err != nil {
		return NegotiationRef{}, err
	}
	if bodyHasAny(string(resp.body), limitMarkers) || chainHas(resp.chain, limitMarkers) {
		return NegotiationRef{}, &StatusError{Kind: ErrRateLimited, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return NegotiationRef{}, &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	ref := NegotiationRef{Vacancy: req.VacancyID}
	var payload struct {
		Success       json.RawMessage `json:"success"`
		TopicID       json.RawMessage `json:"topic_id"`
		NegotiationID json.RawMessage `json:"negotiationId"`
		Status        string          `json:"status"`
		Response      struct {
			Responded    json.RawMessage `json:"responded"`
			Success      json.RawMessage `json:"success"`
			ShortVacancy struct {
				NegotiationID int64 `json:"negotiationId"`
			} `json:"shortVacancy"`
		} `json:"responseStatus"`
	}
	if jerr := json.Unmarshal(resp.body, &payload); jerr != nil {
		return NegotiationRef{}, &StatusError{
			Kind: ErrUpstream, StatusCode: resp.status, URL: resp.finalURL,
			BodySnippet: snippet(resp.body), Chain: resp.chain,
		}
	}
	id := firstNonEmpty(jsonString(payload.TopicID), jsonString(payload.NegotiationID))
	if id == "" && payload.Response.ShortVacancy.NegotiationID != 0 {
		id = strconv.FormatInt(payload.Response.ShortVacancy.NegotiationID, 10)
	}
	sent := jsonBool(payload.Success) || jsonBool(payload.Response.Responded) || jsonBool(payload.Response.Success) || id != "" || payload.Status == "ok"
	if !sent {
		// A 200 that is neither success nor a known failure marker: treat
		// as a soft block (anti-bot interstitial served with 200).
		return NegotiationRef{}, &StatusError{
			Kind: ErrForbidden, StatusCode: resp.status, URL: resp.finalURL,
			BodySnippet: snippet(resp.body), Chain: resp.chain,
		}
	}
	ref.ID = id
	return ref, nil
}

// limitMarkers: hh's own daily-negotiations quota (not our pacing).
var limitMarkers = []string{"negotiations-limit-exceeded", "negotiationslimitexceeded"}

func chainHas(chain []RedirectHop, markers []string) bool {
	for _, hop := range chain {
		if bodyHasAny(hop.Location, markers) {
			return true
		}
	}
	return false
}

func jsonBool(raw json.RawMessage) bool {
	if raw == nil {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(s, "true")
	}
	return false
}

func jsonString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return ""
}

// --- Chatik methods ---

func (c *Client) chatikURL(name, defaultPath string) string {
	return c.eps.resolve(name, c.eps.chatikDefault(defaultPath))
}

// ListChats implements hhapi.HHAPI: lists chatik conversations (one page).
// The live payload carries per-chat applicant state and resource refs — see
// parseChatsList.
func (c *Client) ListChats(ctx context.Context, page int) ([]Chat, error) {
	u, err := url.Parse(c.chatikURL(epChats, "/chatik/api/chats"))
	if err != nil {
		return nil, fmt.Errorf("hhapi: chats: %w", err)
	}
	q := u.Query()
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	u.RawQuery = q.Encode()
	resp, err := c.do(ctx, requestSpec{
		kind: kindGet, operation: "chats", method: http.MethodGet,
		url: u.String(), header: c.chatikHeaders(http.MethodGet),
	})
	if err != nil {
		return nil, err
	}
	return parseChatsList(resp.body)
}

// Counts implements hhapi.HHAPI: reads the vacancy-search counter shard.
// query is the raw URL query string (e.g. `text=golang&area=1`); empty means
// the global counter.
func (c *Client) Counts(ctx context.Context, query string) (int, error) {
	target := c.endpointURL(epCounts, c.eps.siteDefault("/shards/vacancy/counts"))
	if q := strings.TrimPrefix(strings.TrimSpace(query), "?"); q != "" {
		target += "?" + q
	}
	resp, err := c.getXHR(ctx, "counts", c.withPinnedHost(target), c.withPinnedHost(c.eps.searchPage))
	if err != nil {
		return 0, err
	}
	return parseCounts(resp.body)
}

// ChatHistory implements hhapi.HHAPI: fetches one chat's message history.
func (c *Client) ChatHistory(ctx context.Context, chatID string) ([]ChatMessage, error) {
	u, err := url.Parse(c.chatikURL(epChatData, "/chatik/api/chat_data"))
	if err != nil {
		return nil, fmt.Errorf("hhapi: chat_data: %w", err)
	}
	q := u.Query()
	q.Set("chatId", chatID)
	u.RawQuery = q.Encode()
	resp, err := c.do(ctx, requestSpec{
		kind: kindGet, operation: "chat_history", method: http.MethodGet,
		url: u.String(), header: c.chatikHeaders(http.MethodGet),
	})
	if err != nil {
		return nil, err
	}
	return parseChatHistory(resp.body, chatID)
}

// SendMessage posts {chatId, idempotencyKey, text}. The idempotency key is
// caller-owned and constant across retries, so network/5xx retries are safe.
func (c *Client) SendMessage(ctx context.Context, chatID, idempotencyKey, text string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(chatID), 10, 64)
	if err != nil {
		return fmt.Errorf("hhapi: send_message: chat id %q is not numeric: %w", chatID, err)
	}
	body, err := json.Marshal(struct {
		ChatID         int64  `json:"chatId"`
		IdempotencyKey string `json:"idempotencyKey"`
		Text           string `json:"text"`
	}{ChatID: id, IdempotencyKey: idempotencyKey, Text: text})
	if err != nil {
		return fmt.Errorf("hhapi: send_message: %w", err)
	}
	resp, err := c.do(ctx, requestSpec{
		kind: kindIdemPost, operation: "send_message", method: http.MethodPost,
		url: c.chatikURL(epSend, "/chatik/api/send"), header: c.chatikHeaders(http.MethodPost), body: body,
	})
	if err != nil {
		return err
	}
	if resp.status == http.StatusConflict || bodyHasAny(string(resp.body), []string{"chat_not_found", "CHAT_DOES_NOT_EXIST"}) {
		return &StatusError{Kind: ErrNotFound, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body)}
	}
	return nil
}

// LeaveChat removes us from a chat via the chatik participant_action
// endpoint with actionType LEAVE. NOTE: the donor map documents only
// TYPING|NONE values; LEAVE must be confirmed against a live session and
// can be corrected via the `chat_leave` endpoint override without a rebuild.
func (c *Client) LeaveChat(ctx context.Context, chatID string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(chatID), 10, 64)
	if err != nil {
		return fmt.Errorf("hhapi: leave_chat: chat id %q is not numeric: %w", chatID, err)
	}
	body, err := json.Marshal(struct {
		ChatID     int64  `json:"chatId"`
		ActionType string `json:"actionType"`
	}{ChatID: id, ActionType: "LEAVE"})
	if err != nil {
		return fmt.Errorf("hhapi: leave_chat: %w", err)
	}
	_, err = c.doMutation(ctx, func() requestSpec {
		return requestSpec{
			kind: kindMutation, operation: "leave_chat", method: http.MethodPost,
			url: c.chatikURL(epChatLeave, "/chatik/api/participant_action"), header: c.chatikHeaders(http.MethodPost), body: body,
		}
	})
	return err
}

// --- Contacts ---

// EmployerContacts extracts contacts from the employer SSR page
// (employerInfo.contacts). Unknown shapes degrade to an empty list, never
// an error: contacts are best-effort enrichment.
func (c *Client) EmployerContacts(ctx context.Context, employerID int64) ([]Contact, error) {
	target := c.endpointURL(epEmployerCard, c.eps.siteDefault("/employer")) + "/" + strconv.FormatInt(employerID, 10)
	resp, err := c.getDoc(ctx, "employer_contacts", target, c.withPinnedHost(c.eps.site+"/"))
	if err != nil {
		return nil, err
	}
	state, ok := initialState(resp.body)
	if !ok {
		return nil, fmt.Errorf("hhapi: employer_contacts %d: %w", employerID, errNilState)
	}
	info := obj(state["employerInfo"])
	if info == nil {
		return nil, nil
	}
	out := []Contact{}
	for _, raw := range arr(info["contacts"]) {
		m := obj(raw)
		typ := firstNonEmpty(str(m, "type", "contactType"))
		val := firstNonEmpty(str(m, "value", "contactValue"))
		if val == "" {
			continue
		}
		if typ == "" {
			typ = inferContactType(val)
		}
		out = append(out, Contact{Type: typ, Value: val})
	}
	return out, nil
}

func inferContactType(v string) string {
	switch {
	case strings.Contains(v, "@"):
		return "email"
	case strings.HasPrefix(v, "+7"), strings.HasPrefix(v, "8"), strings.HasPrefix(v, "+"):
		return "phone"
	case strings.HasPrefix(v, "https://"), strings.HasPrefix(v, "http://"):
		return "url"
	default:
		return "name"
	}
}

// --- small URL helpers ---

func (c *Client) vacancyCardURL(id int64) string {
	return c.endpointURL(epVacancyCard, c.eps.siteDefault("/vacancy")) + "/" + strconv.FormatInt(id, 10)
}

func (c *Client) popupReferer(id int64) string {
	u, _ := url.Parse(c.withPinnedHost(c.endpointURL(epApplyPopup, c.eps.siteDefault("/applicant/vacancy_response/popup"))))
	q := u.Query()
	q.Set("vacancyId", strconv.FormatInt(id, 10))
	u.RawQuery = q.Encode()
	return u.String()
}
