package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// T26/T27 resume cloning (live-verified 2026-09-25):
//
//   - POST /applicant/resumes/clone, XHR headers + X-Xsrftoken, JSON body
//     {"resumeId":"<id>"} → 200 {"url":"/profile/resume/?resume=<NEW_HASH>"};
//     the new resume hash is parsed from that url.
//   - That call only creates a SHELL: the /shards/applicant/resumes entry has
//     status="not_finished", no title (percent 0) and hh sets no parent —
//     lineage is tracked locally (internal/store). The owner-visible
//     behaviour is "требует дозаполнить данные, даже титула нет".
//
// T28 NATIVE FULL CLONE (live-verified 2026-09-25, owner session): hh's
// own "похожее резюме" flow is ONE call to the resume-profile-front
// service:
//
//	POST https://resume-profile-front.hh.ru/profile/shards/resume/clone_resumes
//	{"resumeHash":"<hash>","resumesCreationProperties":[{"title":"<new unique
//	 title>","roleIds":["<professionalRole id>"]}],"userPrint":{}}
//
// with the SPA-exact header set (reverse-engineered from the deployed
// resume_profile_front bundle, chunk 6069 module 11154 + axios wiring in
// modules 70147/11981/21440 + the editor page's globalServiceVars):
//
//	Accept: application/json            (axios default — no */*)
//	Content-Type: application/json      (axios default)
//	X-Requested-With: XMLHttpRequest    (interceptor u)
//	X-Xsrftoken: <_xsrf>                (interceptor v)
//	X-hhtmSource: resume_professional_role, X-hhtmFrom/-Label: ""
//	                                    (interceptor 11981 <- analyticsParams)
//	Origin/Referer: the hh.ru page; Sec-Fetch-Site: same-site
//
// Missing any piece of that set earns a bare empty 400 from the service
// (the T27 wall). The live success envelope is HTTP 200 {} — NO hash in
// the body; the new resume is found through the resumes shard
// (_attributes.parentResumeId == parent numeric id). The clone arrives
// COMPLETE in one call: hh copies the whole parent content server-side,
// sets parentResumeId itself, status "new", isSearchable true. hh enforces
// unique resume titles (BUSINESS DUPLICATE), so the clone rides a fresh
// title derived from the parent's (hh's own modal clones under a NEW
// position name; a byte-identical title is impossible).
//
// Fallback: on a DEFINITIVE native rejection (hh answered; no entity was
// created) the T27 shell-clone + builder-update pipeline still runs. On a
// transport-level failure the native call is ambiguous (the clone may
// exist) — no fallback, the AmbiguousError propagates so callers resolve
// by reading.
//
// T27 FALLBACK SURFACE (kept for definitive native rejections): shell
// clone (above) then the resume-profile builder — GET get_by_hash (builder
// state) and POST update (per-screen save; chunk 6069 thunk P, hM split in
// module 64673: personal fields + experience ride `profile`, content stays
// on `resume`; values use the conditional format [{"string":v}]). LIVE
// STATUS: the update endpoint rejects every off-browser shape with a bare
// 400 (2026-09-25), so the fallback terminates in ErrCloneMaterialize
// unless hh reopens it.

// ErrCloneNative marks a failed native clone_resumes attempt where hh gave
// a definitive answer and no entity was created.
var ErrCloneNative = errors.New("hhapi: native clone_resumes failed")

// StageNative is the CloneError stage for a native clone_resumes attempt
// that never produced an entity (definitive hh rejection).
const StageNative = "native"

// cloneResumesURL is the resume-profile-front service endpoint (never
// pinned to a regional host; see endpointSet.resolveExternal).
const cloneResumesURL = "https://resume-profile-front.hh.ru/profile/shards/resume/clone_resumes"

// resumeUpdateURL is the builder save endpoint: the same service and
// header discipline as clone_resumes (T28: every editor POST rides the
// resume-profile-front BFF host; a site-host pinning earned the T27 400s).
const resumeUpdateURL = "https://resume-profile-front.hh.ru/profile/shards/resume/update"

// cloneHhtmSource is the X-hhtmSource the deployed editor sends from the
// screen the clone modal rides on (resume_professional_role route).
const cloneHhtmSource = "resume_professional_role"

// Title-suffix scheme for the fresh clone title ("(копия)", "(копия 2)",
// ...): hh enforces unique resume titles, so the clone cannot reuse the
// parent's verbatim.
const (
	cloneTitleSuffixA   = " (копия)"
	cloneTitleSuffixFmt = " (копия %d)"
	maxCloneTitleLength = 100 // hh title maxlength (_conditions)
	maxCloneAttempts    = 3   // at most three clone_resumes POSTs per CloneResume
)

// ErrResumeLimit is the hh resume-count quota rejection (T26).
// It extends the contract sentinels (same pattern as ErrXSRF in
// errors.go; contracts.go stays untouched). The live rejection shape is
// unknown — the marker scan below is deliberately narrow (validation or
// forbidden class plus an explicit limit word) so ordinary 4xx never
// masquerades as a limit.
var ErrResumeLimit = errors.New("hhapi: resume limit reached")

// ErrCloneMaterialize marks a failed clone materialization: the builder
// update endpoint rejected the content transfer (T27).
var ErrCloneMaterialize = errors.New("hhapi: clone materialization failed")

// ErrCloneIncomplete marks the verify-gate failure: the clone still reads
// back without a title or as not_finished, so it is not a usable duplicate.
var ErrCloneIncomplete = errors.New("hhapi: clone is not a complete published resume")

// Clone stages for CloneError.
const (
	StageMaterialize = "materialize"
	StageVerify      = "verify"
)

// CloneError carries the failed full-clone stage and the shell hash (empty
// when the shell itself never materialized a hash to report).
type CloneError struct {
	Stage string
	Hash  string
	Err   error
}

func (e *CloneError) Error() string {
	return fmt.Sprintf("hhapi: clone %s (hash %q): %v", e.Stage, e.Hash, e.Err)
}

func (e *CloneError) Unwrap() error { return e.Err }

// resumeLimitMarkers: candidate hh resume-quota rejection markers (RU + EN).
var resumeLimitMarkers = []string{"limit", "лимит", "превышен", "максимум"}

// maxBuilderScreens bounds the materialization walk: the editor serves a
// handful of static screens per resume; a server looping us past this bound
// is a contract violation, not progress.
const maxBuilderScreens = 16

// personalBuilderFields is the editor's hM split table (module 64673 of the
// deployed resume_profile_front bundle): these resume-state keys plus
// "experience" ride the profile side of the update body.
var personalBuilderFields = []string{
	"area", "metro", "gender", "lastName", "birthday", "language", "firstName",
	"workTicket", "middleName", "hasVehicle", "hasMedicalBook",
	"hasSelfEmployment", "relocation", "citizenship", "relocationArea",
	"primaryEducation", "driverLicenseTypes", "addressCoordinates",
	"preferredWorkAreas", "elementaryEducation", "additionalEducation",
	"attestationEducation",
}

// ResumeCloner is the optional autoclone capability of the site client.
// *Client implements it; feature packages consume it through a type
// assertion so fakes and alternate transports may omit it.
type ResumeCloner interface {
	// CloneResume clones the resume identified by its hash (the multiapply
	// pipeline passes run.ResumeID, which the web surface keys by hash) and
	// returns the new resume hash ONLY if the clone reads back complete:
	// title present, status not in_flight. Any shorter outcome is a typed
	// error (ErrResumeLimit / *CloneError) — callers must never drive
	// applications against an unverified shell.
	CloneResume(ctx context.Context, resumeID string) (newHash string, err error)
}

// CloneResume runs the native full clone (T28, live-verified): one
// clone_resumes call — hh copies the whole parent content server-side,
// parents the clone (parentResumeId) and publishes it searchable; the new
// hash is discovered from the resumes shard and verified by read-back. On
// a definitive native rejection (no entity created) it falls back to the
// T27 pipeline (shell clone + builder materialization + verify). The hash
// is returned only when the verify gate passes.
func (c *Client) CloneResume(ctx context.Context, resumeID string) (string, error) {
	newHash, nerr := c.cloneResumeNative(ctx, resumeID)
	if nerr == nil {
		return newHash, nil
	}
	var amb *AmbiguousError
	if errors.As(nerr, &amb) || !isDefinitiveCloneReject(nerr) {
		// The native call may or may not have landed: creating another
		// clone through the fallback could duplicate the resume. Resolve
		// by reading, never by repeating.
		return "", nerr
	}
	c.log.Debug("hhapi: native clone rejected definitively, falling back to shell+update", "err", nerr)
	return c.cloneResumeShellFlow(ctx, resumeID)
}

// cloneResumesRequest is the exact SPA body (chunk 6069 module 11154):
// roleIds are STRINGS in hh's dictionaries ({ids:["40"],...}) and
// userPrint rides as an object ({} when the fingerprint slice is empty —
// the SPA sends the key regardless).
type cloneResumesRequest struct {
	ResumeHash    string                    `json:"resumeHash"`
	CreationProps []cloneResumePropertySpec `json:"resumesCreationProperties"`
	UserPrint     json.RawMessage           `json:"userPrint"`
}

type cloneResumePropertySpec struct {
	Title   string   `json:"title"`
	RoleIDs []string `json:"roleIds"`
}

// cloneResumesResponse covers every live envelope: success {};
// business validation {"errors":[{field,type,code,...}]} (HTTP 200!);
// legacy rejects {"@code":"403","$":"..."}; builder errors
// {"error":[{key}]}.
type cloneResumesResponse struct {
	Errors []struct {
		Field string `json:"field"`
		Type  string `json:"type"`
		Code  string `json:"code"`
	} `json:"errors"`
	Error []struct {
		Key string `json:"key"`
	} `json:"error"`
	Code string `json:"@code"`
	Msg  string `json:"$"`
}

// cloneResumesHeaders is the SPA-exact header set (see the file doc):
// xhrHeaders for the UA/client-hints/X-Requested-With base, then the
// axios-instance defaults and hhtm interceptor values the bundle adds.
func (c *Client) cloneResumesHeaders() http.Header {
	h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/profile/resume"))
	h.Set("Accept", "application/json")
	h.Set("Origin", c.eps.site)
	h.Set("Sec-Fetch-Site", "same-site")
	h.Set("X-hhtmSource", cloneHhtmSource)
	h.Set("X-hhtmFrom", "")
	h.Set("X-hhtmSourceLabel", "")
	h.Set("X-hhtmFromLabel", "")
	return h
}

// conditionInts renders a conditional-format field whose "string" parts
// are numbers (professionalRole: [{"string":160}]) as strings — the format
// hh's dictionaries use for roleIds.
func conditionInts(raw json.RawMessage) []string {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v, ok := p["string"]
		if !ok {
			continue
		}
		var n float64
		if err := json.Unmarshal(v, &n); err == nil {
			out = append(out, strconv.FormatInt(int64(n), 10))
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cloneTitleTaken reads the parent builder state's title conditions
// (_conditions.title.parts[].string.not_in): every title hh already has.
func cloneTitleTaken(st *builderState) map[string]bool {
	taken := map[string]bool{}
	raw, ok := st.Resume["_conditions"]
	if !ok {
		return taken
	}
	var conds struct {
		Title struct {
			Parts []struct {
				String struct {
					NotIn []string `json:"not_in"`
				} `json:"string"`
			} `json:"parts"`
		} `json:"title"`
	}
	if err := json.Unmarshal(raw, &conds); err != nil {
		return taken
	}
	for _, p := range conds.Title.Parts {
		for _, t := range p.String.NotIn {
			taken[t] = true
		}
	}
	return taken
}

// cloneTitleCandidate derives the fresh clone title: "<parent> (копия)",
// then "(копия N)", truncated to hh's maxlength.
func cloneTitleCandidate(parent string, attempt int) string {
	suffix := cloneTitleSuffixA
	if attempt > 1 {
		suffix = fmt.Sprintf(cloneTitleSuffixFmt, attempt)
	}
	base := parent
	if len(base)+len(suffix) > maxCloneTitleLength {
		base = strings.TrimSpace(base[:maxCloneTitleLength-len(suffix)])
	}
	return base + suffix
}

// cloneResumeNative fires the native full clone (live contract above).
// Definitive rejections wrap ErrCloneNative; transport loss surfaces as
// AmbiguousError.
func (c *Client) cloneResumeNative(ctx context.Context, parentHash string) (string, error) {
	parent, err := c.fetchBuilderState(ctx, parentHash)
	if err != nil {
		return "", fmt.Errorf("%w: parent state: %v", ErrCloneNative, err)
	}
	parentTitle := parent.Title()
	if parentTitle == "" {
		return "", fmt.Errorf("%w: parent has no title", ErrCloneNative)
	}
	var parentID string
	if attrs, ok := parent.Resume["_attributes"]; ok {
		var a struct {
			ID string `json:"id"`
		}
		if jerr := json.Unmarshal(attrs, &a); jerr == nil {
			parentID = a.ID
		}
	}
	if parentID == "" {
		return "", fmt.Errorf("%w: parent has no numeric id", ErrCloneNative)
	}
	roleIDs := conditionInts(parent.Resume["professionalRole"])
	if roleIDs == nil {
		roleIDs = []string{}
	}
	taken := cloneTitleTaken(parent)

	attempt := 1
	for {
		resp, err := c.doMutation(ctx, func() requestSpec {
			return c.buildCloneResumes(parentHash, parentTitle, roleIDs, taken, attempt)
		})
		if err != nil {
			// Transport loss after send or a non-2xx HTTP verdict: the
			// mutation path never auto-retries, so this is terminal and
			// typed by the pipeline (AmbiguousError stays ambiguous).
			return "", err
		}
		if bodyHasAny(string(resp.body), captchaMarkers) {
			return "", &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
		}
		if _, verr := parseCloneResumesVerdict(resp.body); verr == nil {
			break // created
		} else if !errors.Is(verr, errCloneTitleDuplicate) || attempt >= maxCloneAttempts {
			return "", verr
		}
		attempt++
	}

	// Discovery: the clone does not come back in the response body — read
	// the shard lineage (parentResumeId == parent numeric id), newest first.
	clone, derr := c.discoverNativeClone(ctx, parentID)
	if derr != nil {
		return "", fmt.Errorf("%w: %v", ErrCloneNative, derr)
	}

	// Verify read-back: titled, finished, searchable — the one-call
	// duplicate the T28 criterion demands.
	state, verr := c.fetchBuilderState(ctx, clone)
	if verr != nil {
		return "", &CloneError{Stage: StageVerify, Hash: clone, Err: verr}
	}
	if state.Title() == "" || state.Status() == "" || state.Status() == "not_finished" || !state.IsSearchable {
		return "", &CloneError{
			Stage: StageVerify,
			Hash:  clone,
			Err:   fmt.Errorf("%w: title=%q status=%q searchable=%t", ErrCloneIncomplete, state.Title(), state.Status(), state.IsSearchable),
		}
	}
	return clone, nil
}

// errCloneTitleDuplicate marks the live BUSINESS DUPLICATE verdict on the
// title field (unique resume titles).
var errCloneTitleDuplicate = errors.New("hhapi: clone title duplicate")

// buildCloneResumes renders one clone_resumes requestSpec with the
// attempt-th title candidate (the candidate is pre-registered in taken so
// two attempts never reuse one title within a call).
func (c *Client) buildCloneResumes(parentHash, parentTitle string, roleIDs []string, taken map[string]bool, attempt int) requestSpec {
	title := cloneTitleCandidate(parentTitle, attempt)
	for taken[title] {
		attempt++
		title = cloneTitleCandidate(parentTitle, attempt)
	}
	taken[title] = true
	body, _ := json.Marshal(cloneResumesRequest{
		ResumeHash:    parentHash,
		CreationProps: []cloneResumePropertySpec{{Title: title, RoleIDs: roleIDs}},
		UserPrint:     json.RawMessage(`{}`),
	})
	h := c.cloneResumesHeaders()
	h.Set("Content-Type", "application/json")
	return requestSpec{
		kind:      kindMutation,
		operation: "resume_clone_resumes",
		method:    http.MethodPost,
		url:       c.resolveCloneResumesURL(),
		header:    h,
		body:      body,
	}
}

func (c *Client) resolveCloneResumesURL() string {
	return c.eps.resolveExternal(epResumeCloneResumes, cloneResumesURL)
}

// parseCloneResumesVerdict classifies the live response envelopes. ok=true
// means the clone was created ({}).
func parseCloneResumesVerdict(body []byte) (ok bool, err error) {
	var out cloneResumesResponse
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		return false, fmt.Errorf("%w: decode response: %v", ErrCloneNative, uerr)
	}
	if len(out.Errors) > 0 {
		codes := make([]string, 0, len(out.Errors))
		dup := false
		for _, e := range out.Errors {
			codes = append(codes, e.Field+":"+e.Code)
			if e.Code == "DUPLICATE" {
				dup = true
			}
		}
		joined := strings.Join(codes, ",")
		if dup {
			return false, fmt.Errorf("%w: %s", errCloneTitleDuplicate, joined)
		}
		return false, fmt.Errorf("%w: validation: %s", ErrCloneNative, joined)
	}
	if len(out.Error) > 0 {
		keys := make([]string, 0, len(out.Error))
		for _, e := range out.Error {
			keys = append(keys, e.Key)
		}
		return false, fmt.Errorf("%w: builder error: %s", ErrCloneNative, strings.Join(keys, ","))
	}
	if out.Code != "" {
		return false, fmt.Errorf("%w: %s %s", ErrCloneNative, out.Code, out.Msg)
	}
	return true, nil
}

// discoverNativeClone reads the resumes shard and returns the hash of the
// newest resume parented to parentID — the clone hh just created.
func (c *Client) discoverNativeClone(ctx context.Context, parentID string) (string, error) {
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		return "", err
	}
	best := ""
	var bestCreated int64
	for _, r := range resumes {
		if r.ParentResumeID != parentID {
			continue
		}
		var created int64
		if !r.CreatedAt.IsZero() {
			created = r.CreatedAt.UnixMilli()
		}
		if best == "" || created >= bestCreated {
			best, bestCreated = r.HHID, created
		}
	}
	if best == "" {
		return "", errors.New("no shard entry carries the parent lineage")
	}
	return best, nil
}

// isDefinitiveCloneReject reports whether the native error proves hh
// answered WITHOUT creating an entity (so the shell fallback is safe).
func isDefinitiveCloneReject(err error) bool {
	return errors.Is(err, ErrCloneNative) || errors.Is(err, errCloneTitleDuplicate)
}

// cloneResumeShellFlow is the T27 pipeline (fallback): shell clone,
// content materialization through the builder update endpoint, verify
// read-back. The hash is returned only when the verify gate passes.
func (c *Client) cloneResumeShellFlow(ctx context.Context, resumeID string) (string, error) {
	// (а) shell clone (T26 live contract).
	resp, err := c.doMutation(ctx, func() requestSpec {
		body, _ := json.Marshal(struct {
			ResumeID string `json:"resumeId"`
		}{ResumeID: resumeID})
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: "clone_resume",
			method:    http.MethodPost,
			url:       c.endpointURL(epResumeClone, c.eps.siteDefault("/applicant/resumes/clone")),
			header:    h,
			body:      body,
		}
	})
	if err != nil {
		if isResumeLimitResponse(err) {
			return "", &StatusError{Kind: ErrResumeLimit, URL: c.endpointURL(epResumeClone, c.eps.siteDefault("/applicant/resumes/clone"))}
		}
		return "", err
	}
	// JSON captcha verdicts ride a 200 (same as TouchResume).
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return "", &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	newHash, perr := parseCloneHash(resp.body)
	if perr != nil {
		return "", &StatusError{Kind: ErrUpstream, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}

	// (б)+(в) materialize the shell from the parent content.
	if err := c.materializeClone(ctx, resumeID, newHash); err != nil {
		return "", &CloneError{Stage: StageMaterialize, Hash: newHash, Err: err}
	}

	// (г) verify read-back: only a titled, finished clone is a duplicate.
	state, err := c.fetchBuilderState(ctx, newHash)
	if err != nil {
		return "", &CloneError{Stage: StageVerify, Hash: newHash, Err: err}
	}
	if state.Title() == "" || state.Status() == "" || state.Status() == "not_finished" {
		return "", &CloneError{
			Stage: StageVerify,
			Hash:  newHash,
			Err:   fmt.Errorf("%w: title=%q status=%q", ErrCloneIncomplete, state.Title(), state.Status()),
		}
	}
	return newHash, nil
}

// builderState is the parsed /profile/shards/resume/get_by_hash payload.
// Resume keeps the raw conditional-format fields so content can be
// transferred verbatim.
type builderState struct {
	Hash           string
	status         string
	IsSearchable   bool
	Screens        []string
	NextIncomplete *string
	Resume         map[string]json.RawMessage
	ProfileFields  map[string]json.RawMessage
	Additional     json.RawMessage
}

// Title returns the resume title from the conditional-format field.
func (s *builderState) Title() string {
	raw, ok := s.Resume["title"]
	if !ok {
		return ""
	}
	ss := conditionStrings(raw)
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// Status returns the clone/publish state from _attributes ("not_finished"
// for shells, "modified" for published resumes — live 2026-09-25).
func (s *builderState) Status() string { return s.status }

// parseBuilderState decodes the builder-state envelope. Unknown resume/
// profile fields are preserved verbatim (content transfer must not drop
// what hh adds over time).
func parseBuilderState(body []byte) (*builderState, error) {
	var aux struct {
		NextIncomplete *string `json:"nextIncompleteScreenId"`
		Screens        []struct {
			ScreenID string `json:"screenId"`
		} `json:"screens"`
		Resume     map[string]json.RawMessage `json:"resume"`
		Profile    json.RawMessage            `json:"profile"`
		Additional json.RawMessage            `json:"additionalProperties"`
	}
	if err := json.Unmarshal(body, &aux); err != nil {
		return nil, fmt.Errorf("hhapi: builder state: decode: %w", err)
	}
	if aux.Resume == nil {
		return nil, errors.New("hhapi: builder state: no resume object")
	}
	st := &builderState{
		NextIncomplete: aux.NextIncomplete,
		Screens:        make([]string, 0, len(aux.Screens)),
		Resume:         aux.Resume,
		Additional:     aux.Additional,
	}
	for _, s := range aux.Screens {
		if s.ScreenID != "" {
			st.Screens = append(st.Screens, s.ScreenID)
		}
	}
	if attrs, ok := aux.Resume["_attributes"]; ok {
		var a struct {
			Hash         string `json:"hash"`
			Status       string `json:"status"`
			IsSearchable bool   `json:"isSearchable"`
		}
		if err := json.Unmarshal(attrs, &a); err == nil {
			st.Hash, st.status, st.IsSearchable = a.Hash, a.Status, a.IsSearchable
		}
	}
	// profile.{fields} — the personal-data container (live envelope). A flat
	// map (older shape) is accepted as-is.
	if len(aux.Profile) > 0 {
		var withFields struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		if err := json.Unmarshal(aux.Profile, &withFields); err == nil && withFields.Fields != nil {
			st.ProfileFields = withFields.Fields
		} else {
			var flat map[string]json.RawMessage
			if err := json.Unmarshal(aux.Profile, &flat); err == nil {
				st.ProfileFields = flat
			}
		}
	}
	return st, nil
}

// conditionStrings extracts the string values of a conditional-format field
// ([{"string":...}, ...]). Non-string parts are skipped.
func conditionStrings(raw json.RawMessage) []string {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		var v string
		if err := json.Unmarshal(p["string"], &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// splitBuilderResume is the editor's hM split: personal fields plus
// experience ride the profile side, everything else stays on the resume
// side. The returned maps alias the source raw messages (no deep copies —
// the state is read-only after parse).
func splitBuilderResume(st *builderState) (resumeSide, profileFields map[string]json.RawMessage) {
	resumeSide = make(map[string]json.RawMessage, len(st.Resume))
	profileFields = make(map[string]json.RawMessage, len(st.ProfileFields)+1)
	personal := make(map[string]bool, len(personalBuilderFields)+1)
	for _, k := range personalBuilderFields {
		personal[k] = true
	}
	personal["experience"] = true
	for k, raw := range st.Resume {
		if personal[k] {
			profileFields[k] = raw
			continue
		}
		resumeSide[k] = raw
	}
	for k, raw := range st.ProfileFields {
		profileFields[k] = raw
	}
	// Identity metadata never travels with the content.
	delete(resumeSide, "_attributes")
	delete(resumeSide, "_conditions")
	return resumeSide, profileFields
}

// fetchBuilderState reads the builder state of one resume (live-verified
// GET JSON shard; retryable like every read).
func (c *Client) fetchBuilderState(ctx context.Context, hash string) (*builderState, error) {
	target := c.endpointURL(epResumeGetByHash, c.eps.siteDefault("/profile/shards/resume/get_by_hash")) + "?resumeHash=" + url.QueryEscape(hash)
	resp, err := c.do(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		return requestSpec{
			kind:      kindGet,
			operation: "resume_builder_state",
			method:    http.MethodGet,
			url:       target,
			header:    h,
		}
	}())
	if err != nil {
		return nil, err
	}
	st, perr := parseBuilderState(resp.body)
	if perr != nil {
		return nil, &StatusError{Kind: ErrUpstream, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	return st, nil
}

// resumeScreenUpdate is the resume-profile builder save envelope (decoded
// from the deployed editor SPA; chunk 6069 module 11154 thunk P — every
// key of the thunk body rides: the T27 400s were the missing
// additionalProperties/userSkillLevels/userPrint plus the missing SPA
// header set).
type resumeScreenUpdate struct {
	ResumeHash      string                     `json:"resumeHash"`
	CurrentScreenID string                     `json:"currentScreenId"`
	Resume          map[string]json.RawMessage `json:"resume"`
	Profile         map[string]json.RawMessage `json:"profile"`
	QuestionMap     map[string]string          `json:"questionToAnswerMap"`
	Additional      json.RawMessage            `json:"additionalProperties"`
	UserSkillLevels json.RawMessage            `json:"userSkillLevels,omitempty"`
	UserPrint       json.RawMessage            `json:"userPrint,omitempty"`
}

// resumeUpdateResp is the builder save response: null NextIncomplete means
// the builder considers the resume complete.
type resumeUpdateResp struct {
	NextIncomplete *string `json:"nextIncompleteScreenId"`
	Errors         []struct {
		Field string `json:"field"`
		Code  string `json:"code"`
	} `json:"errors"`
	Error []struct {
		Key string `json:"key"`
	} `json:"error"`
}

// materializeClone transfers the parent resume content onto the shell via
// the builder update endpoint, walking the editor screens until the builder
// reports no incomplete screen left (completion is hh's publish: visibility
// is never touched from our side).
func (c *Client) materializeClone(ctx context.Context, parentHash, cloneHash string) error {
	parent, err := c.fetchBuilderState(ctx, parentHash)
	if err != nil {
		return fmt.Errorf("parent builder state: %w", err)
	}
	clone, err := c.fetchBuilderState(ctx, cloneHash)
	if err != nil {
		return fmt.Errorf("clone builder state: %w", err)
	}
	resumeSide, profileFields := splitBuilderResume(parent)
	additional := parent.Additional
	if len(additional) == 0 {
		additional = json.RawMessage(`{}`)
	}
	payload := resumeScreenUpdate{
		ResumeHash:      cloneHash,
		Resume:          resumeSide,
		Profile:         profileFields,
		QuestionMap:     map[string]string{},
		Additional:      additional,
		UserSkillLevels: json.RawMessage(`{}`),
		UserPrint:       json.RawMessage(`{}`),
	}
	// Walk the clone's screens; the SPA re-sends the accumulated form per
	// screen, so the same split rides every call until hh reports completion.
	screens := clone.Screens
	if len(screens) == 0 {
		screens = []string{"professional_role"}
	}
	next := clone.NextIncomplete
	for i := 0; i < maxBuilderScreens; i++ {
		screen := ""
		if next != nil && *next != "" {
			screen = *next
		} else if i < len(screens) {
			screen = screens[i]
		} else {
			break
		}
		payload.CurrentScreenID = screen
		done, err := c.postResumeScreen(ctx, &payload)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return errors.New("builder still reports incomplete screens after the screen walk")
}

// isResumeLimitResponse re-tags the transport's 400/422/403 classification as
// ErrResumeLimit when the body carries a limit marker. The StatusError body
// snippet is the masked fragment the pipeline kept — enough for the marker
// scan without re-reading the body.
func isResumeLimitResponse(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	if !errors.Is(se.Kind, ErrValidation) && !errors.Is(se.Kind, ErrForbidden) {
		return false
	}
	return bodyHasAny(se.BodySnippet, resumeLimitMarkers)
}

// parseCloneHash extracts the new resume hash from the live envelope:
// {"url":"/profile/resume/?resume=<hash>"} (absolute urls accepted). Any
// deviation is an error — callers must never proceed without the hash.
func parseCloneHash(body []byte) (string, error) {
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("hhapi: clone_resume: decode envelope: %w", err)
	}
	raw := strings.TrimSpace(payload.URL)
	if raw == "" {
		return "", errors.New("hhapi: clone_resume: envelope has no url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("hhapi: clone_resume: parse url: %w", err)
	}
	hash := u.Query().Get("resume")
	if hash == "" {
		return "", errors.New("hhapi: clone_resume: url carries no resume hash")
	}
	return hash, nil
}
