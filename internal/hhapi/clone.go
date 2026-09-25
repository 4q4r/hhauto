package hhapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
// T27 full-clone flow (CloneResume): shell clone → materialize → verify.
//
// The materialization rides the resume-profile builder surface (the web UI
// behind /profile/resume/...):
//
//   - GET  /profile/shards/resume/get_by_hash?resumeHash=<hash> — the builder
//     state: {nextIncompleteScreenId, screens:[{screenId}], resume:{...},
//     profile:{fields:{...}}, additionalProperties} (live-verified read).
//   - POST /profile/shards/resume/update — the per-screen save the editor
//     SPA issues: {resumeHash, currentScreenId, resume, profile,
//     questionToAnswerMap, additionalProperties, userSkillLevels, userPrint}
//     (contract decoded from the deployed resume_profile_front bundles:
//     thunk P in chunk 6069, hM split in module 64673, axios wrapper in
//     module 70147/21440). The response carries {nextIncompleteScreenId,
//     errors|error}; null nextIncompleteScreenId = the builder considers the
//     resume complete (the SPA then treats it as created/published — no
//     separate publish call exists on this surface; hh flips the visibility
//     itself, so we never touch it).
//   - The resume/profile sides are the editor's hM split: personal fields
//     (area, metro, name, birthday, citizenship, ..., education lists) plus
//     experience ride `profile`; the resume content (title, professionalRole,
//     key skills, about-text, work conditions) stays on `resume`. Values use
//     the conditional read format ([{"string":v}]); we transfer them verbatim.
//
// LIVE STATUS 2026-09-25: the update endpoint rejects every off-browser
// request shape with a bare 400 (and the hh.ru /profile/** POST class is
// edge-gated: non-browser TLS gets a spoofed 405 "allow: GET" while a real
// Chrome reaches the app). A freshly cloned shell therefore fails
// materialization with ErrCloneMaterialize, and any clone that still reads
// back incomplete fails the gate with ErrCloneIncomplete — callers (the
// multiapply autoclone) park for review instead of driving an empty shell.
// The endpoint defaults point at the site host; config may override
// "resume_update"/"resume_get_by_hash" (e.g. to the resume-profile-front
// origin) once hh accepts the surface from API clients.

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
	// title present, status left not_finished. Any shorter outcome is a
	// typed error (ErrResumeLimit / *CloneError) — callers must never drive
	// applications against an unverified shell.
	CloneResume(ctx context.Context, resumeID string) (newHash string, err error)
}

// CloneResume runs the full-clone pipeline: (а) shell clone, (б) content
// materialization through the resume-profile builder update endpoint, (в)
// completion walk until the builder reports no incomplete screen (hh
// publishes on completion; we never touch visibility), (г) verify read-back.
// The hash is returned only when (г) passes.
func (c *Client) CloneResume(ctx context.Context, resumeID string) (string, error) {
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
// from the deployed editor SPA; see the file doc-block).
type resumeScreenUpdate struct {
	ResumeHash      string                     `json:"resumeHash"`
	CurrentScreenID string                     `json:"currentScreenId"`
	Resume          map[string]json.RawMessage `json:"resume"`
	Profile         map[string]json.RawMessage `json:"profile"`
	QuestionMap     map[string]string          `json:"questionToAnswerMap"`
	Additional      json.RawMessage            `json:"additionalProperties"`
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
		ResumeHash:  cloneHash,
		Resume:      resumeSide,
		Profile:     profileFields,
		QuestionMap: map[string]string{},
		Additional:  additional,
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

// postResumeScreen issues one builder update. done reports hh's
// nextIncompleteScreenId == null (complete). Validation/business rejections
// surface as ErrCloneMaterialize with the field codes.
func (c *Client) postResumeScreen(ctx context.Context, payload *resumeScreenUpdate) (done bool, err error) {
	body, merr := json.Marshal(payload)
	if merr != nil {
		return false, fmt.Errorf("encode update: %w", merr)
	}
	resp, err := c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/profile/resume"))
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_builder_update",
			method:    http.MethodPost,
			url:       c.endpointURL(epResumeUpdate, c.eps.siteDefault("/profile/shards/resume/update")),
			header:    h,
			body:      body,
		}
	})
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrCloneMaterialize, err)
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return false, fmt.Errorf("%w: %v", ErrCloneMaterialize, &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain})
	}
	var out resumeUpdateResp
	if uerr := json.Unmarshal(resp.body, &out); uerr != nil {
		return false, fmt.Errorf("%w: decode response: %v", ErrCloneMaterialize, uerr)
	}
	if len(out.Error) > 0 {
		keys := make([]string, 0, len(out.Error))
		for _, e := range out.Error {
			keys = append(keys, e.Key)
		}
		return false, fmt.Errorf("%w: builder error: %s", ErrCloneMaterialize, strings.Join(keys, ","))
	}
	if len(out.Errors) > 0 {
		codes := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			codes = append(codes, e.Field+":"+e.Code)
		}
		return false, fmt.Errorf("%w: validation: %s", ErrCloneMaterialize, strings.Join(codes, ","))
	}
	return out.NextIncomplete == nil, nil
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
