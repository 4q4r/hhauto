package hhapi

// T51 resume content mutation — the save contract the REAL editor UI
// speaks, captured live on 2026-09-29 (owner session, cloakbrowser on the
// resume_partial_edit flow, clone 4f48f357…):
//
//	POST {site}/applicant/resume/edit?resume={hash}&hhtmSource=resume_partial_edit
//	{"title":["…"]} | {"skills":["…"]} | {"keySkills":["a","…"]} |
//	{"experience":[{…item objects, description changed…}]}
//
// with the ordinary site XHR discipline (Accept/Content-Type JSON,
// X-Requested-With, X-Xsrftoken, X-hhtmSource: resume_partial_edit,
// X-hhtmFrom: ""). Live facts that shape the serializer:
//
//   - the former /profile/shards/resume/update BFF route is DEAD for
//     off-browser clients (bare 400 for every payload variant, T29 probe)
//     and is not used by the deployed editor at all — every UI save rides
//     the site-host /applicant/resume/edit form endpoint;
//   - values are FLAT arrays: builder-state conditional format
//     ([{"string":"x"}] / [{"string":40}]) flattens to ["x"] / [40];
//     empty stays [] (salary), experience item objects travel verbatim;
//   - hh accepts a single-field body (title-only verified live, 200) —
//     the UI posts per-segment field sets, a one-field save is the
//     faithful minimal subset;
//   - no fingerprint fields required off-browser (probe passed without);
//   - success = 200 builder-state envelope; rejection = 200 +
//     {"errors":[{field,code,…}]} ("title:REQUIRED" probed live).
//
// Live verification (2026-09-29, -tags live51): title-only no-op POST from
// this client returned 200 and the read-back carried the same title.
// Live probe: internal/hhapi/live_t51_edit_probe_test.go (never in the
// default gate).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// resumeEditHhtmSource is the analytics context the deployed editor sends
// on every resume_partial_edit save (query param AND X-hhtmSource header).
const resumeEditHhtmSource = "resume_partial_edit"

// Verify read-back pacing (var seams for tests): hh applies saves
// asynchronously, so the verify gate polls the builder state until the
// budget expires. Live pacing 2026-09-29: a single scalar edit was visible
// +4s after the POST, but a full experience-array save (eight long
// descriptions, hh re-forks the shared records) applied minutes later —
// the store queue handles record forks slowly. 600s covers the observed
// worst case; a successful verify exits on the first poll that matches.
var (
	verifyPollInterval = 10 * time.Second
	verifyWaitBudget   = 600 * time.Second
)

// hhSharedRecordLists are the builder-state arrays whose items are shared
// records across the account's resumes (experience, education). get_by_hash
// lists the inherited parent-resume records too, while the editor saves
// only THIS resume's own items (captured UI body 2026-09-29: items whose
// "resumes" usage list carries the resume's hash) — and posting inherited
// ids makes hh fork copies instead of updating (live: each improve run
// grew the item count until this filter landed).
var hhSharedRecordLists = []string{
	"experience", "primaryEducation", "elementaryEducation",
	"additionalEducation", "attestationEducation",
}

// hhVolatileKeys are builder-state fields hh updates on its own between
// reads (photoUrls regenerate, lastChangeTimeDetails moves on every
// server-side change — live 2026-09-29 diagnosis: they surfaced as
// phantom diffs and broke verify without any client mutation). They are
// not part of the UI save contract, so the split drops them.
var hhVolatileKeys = []string{"photoUrls", "lastChangeTimeDetails"}

// filterSharedRecords keeps only the shared-record items that belong to
// selfHash and strips the server-only fields (the unstable "resumes"
// usage metadata and "companyLogos" — neither rides the UI save body),
// in place.
func filterSharedRecords(side map[string]json.RawMessage, selfHash string) {
	for _, key := range hhVolatileKeys {
		delete(side, key)
	}
	for _, key := range hhSharedRecordLists {
		raw, ok := side[key]
		if !ok {
			continue
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			continue
		}
		kept := make([]map[string]json.RawMessage, 0, len(items))
		changed := false
		for _, item := range items {
			if !sharedRecordSelf(item, selfHash) {
				changed = true // inherited (parent-resume) record: not this resume's content
				continue
			}
			for _, meta := range []string{"resumes", "companyLogos"} {
				if _, has := item[meta]; has {
					delete(item, meta)
					changed = true
				}
			}
			kept = append(kept, item)
		}
		if !changed {
			continue
		}
		if b, err := json.Marshal(kept); err == nil {
			side[key] = b
		}
	}
}

// sharedRecordSelf reports whether one shared record belongs to selfHash:
// its "resumes" usage list carries the hash (an absent or undecodable list
// keeps the record — the content must not be lost on unknown shapes).
func sharedRecordSelf(item map[string]json.RawMessage, selfHash string) bool {
	raw, ok := item["resumes"]
	if !ok {
		return true
	}
	var list []struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
		return true
	}
	for _, r := range list {
		if r.Hash == selfHash {
			return true
		}
	}
	return false
}

// ResumeContent is the mutable content split of one resume: Resume holds
// the condition-format content fields (title, professionalRole, keySkills,
// skills/О себе, salary, workSchedule, …), Profile the personal fields and
// experience. Screens carries the editor screen ids of the resume;
// Conditions the raw hh validation conditions (_conditions) the feature
// validators read live limits from. The maps are plain JSON values ready
// to be mutated field by field and sent back through UpdateResumeContent.
type ResumeContent struct {
	Resume     map[string]json.RawMessage
	Profile    map[string]json.RawMessage
	Screens    []string
	Conditions map[string]json.RawMessage
}

// clone returns a deep-enough copy (top-level keys copied, raw values are
// immutable JSON) so callers can mutate one field without aliasing.
func (rc *ResumeContent) clone() *ResumeContent {
	out := &ResumeContent{
		Resume:     make(map[string]json.RawMessage, len(rc.Resume)),
		Profile:    make(map[string]json.RawMessage, len(rc.Profile)),
		Screens:    append([]string(nil), rc.Screens...),
		Conditions: map[string]json.RawMessage{},
	}
	for k, v := range rc.Conditions {
		out.Conditions[k] = v
	}
	for k, v := range rc.Resume {
		out.Resume[k] = v
	}
	for k, v := range rc.Profile {
		out.Profile[k] = v
	}
	return out
}

// ResumeUpdateValidationError is a hh validation/business rejection of one
// update POST: Codes carries field:code pairs ("title:DUPLICATE").
type ResumeUpdateValidationError struct {
	Codes []string
	Err   error
}

func (e *ResumeUpdateValidationError) Error() string {
	return fmt.Sprintf("hhapi: resume update validation: %v", e.Codes)
}

func (e *ResumeUpdateValidationError) Unwrap() error { return e.Err }

// ResumeContentVerifyError is the verify-gate failure: hh accepted the
// update POSTs but the read-back does not carry the sent values, so
// nothing may claim the change landed.
type ResumeContentVerifyError struct {
	Keys []string
	Err  error
}

func (e *ResumeContentVerifyError) Error() string {
	return fmt.Sprintf("hhapi: resume update verify failed for %v", e.Keys)
}

func (e *ResumeContentVerifyError) Unwrap() error { return e.Err }

// ResumeContentEditor is the T29 content-mutation seam: read the mutable
// split of one resume, post a modified split through the builder update
// walk with the verify gate. *Client implements it; feature fakes may
// substitute their own.
type ResumeContentEditor interface {
	ResumeContent(ctx context.Context, hash string) (*ResumeContent, error)
	UpdateResumeContent(ctx context.Context, hash string, content *ResumeContent, screens []string) error
}

var _ ResumeContentEditor = (*Client)(nil)

// Clone returns an independent copy of the split (mutation-safe).
func (rc *ResumeContent) Clone() *ResumeContent { return rc.clone() }

// ResumeContent reads the mutable content split of one resume (live
// get_by_hash shard, retryable like every read).
func (c *Client) ResumeContent(ctx context.Context, hash string) (*ResumeContent, error) {
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		return nil, err
	}
	resume, profile := splitBuilderResume(st)
	filterSharedRecords(resume, hash)
	filterSharedRecords(profile, hash)
	conds := map[string]json.RawMessage{}
	if raw, ok := st.Resume["_conditions"]; ok {
		_ = json.Unmarshal(raw, &conds)
	}
	return &ResumeContent{Resume: resume, Profile: profile, Screens: st.Screens, Conditions: conds}, nil
}

// UpdateResumeContent posts every changed field through the site edit
// endpoint (one POST per field, the real UI save contract — see the file
// doc) and verifies by read-back that every changed key landed. Changed
// keys are computed against the LIVE state first: an unchanged content
// posts nothing. Validation rejections surface as
// *ResumeUpdateValidationError (the feature retries with a fixed value or
// skips the field); a verify mismatch is *ResumeContentVerifyError. The
// screens argument is part of the ResumeContentEditor contract only: the
// edit endpoint addresses fields directly, no screen walk exists anymore.
func (c *Client) UpdateResumeContent(ctx context.Context, hash string, content *ResumeContent, _ []string) error {
	live, err := c.ResumeContent(ctx, hash)
	if err != nil {
		return fmt.Errorf("live content: %w", err)
	}
	changed := diffContentKeys(live, content)
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	verifyStart := time.Now()
	for _, key := range changed {
		side, name := splitContentKey(key)
		if verr := c.postResumeEdit(ctx, hash, name, sideOf(content, side)[name]); verr != nil {
			return verr
		}
	}
	// Verify read-back: every changed key must read back equal to the
	// sent value. hh applies saves asynchronously (a marker edit was
	// invisible +2s and visible +4s after the POST — live 2026-09-29), so
	// the read-back polls until the budget expires; only the keys still
	// unreadable back then are reported missing.
	var missing []string
	for {
		verify, err := c.ResumeContent(ctx, hash)
		if err != nil {
			return fmt.Errorf("verify content: %w", err)
		}
		missing = nil
		for _, key := range changed {
			side, name := splitContentKey(key)
			if !appliedEqual(sideOf(verify, side)[name], sideOf(content, side)[name]) {
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Since(verifyStart) >= verifyWaitBudget {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("verify content: %w", ctx.Err())
		case <-time.After(verifyPollInterval):
		}
	}
	return &ResumeContentVerifyError{Keys: missing, Err: ErrCloneMaterialize}
}

// postResumeEdit issues one field save through the site edit endpoint with
// the captured UI header set and the flat value serialization (see the
// file doc). Validation rejections surface as *ResumeUpdateValidationError;
// transport losses keep the mutation-path semantics (never auto-retried).
func (c *Client) postResumeEdit(ctx context.Context, hash, name string, raw json.RawMessage) error {
	body, err := json.Marshal(map[string]json.RawMessage{name: editFlatValue(raw)})
	if err != nil {
		return fmt.Errorf("%w: encode %s: %v", ErrCloneMaterialize, name, err)
	}
	resp, err := c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Accept", "application/json") // the captured save POST value (no */*)
		h.Set("Content-Type", "application/json")
		h.Set("X-hhtmSource", resumeEditHhtmSource)
		h.Set("X-hhtmFrom", "")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_edit",
			method:    http.MethodPost,
			url:       c.editURL(hash),
			header:    h,
			body:      body,
		}
	})
	if err != nil {
		// Transport-level failure (non-2xx classification, session loss):
		// keep the T27 semantics — the caller sees a materialize failure
		// with the original StatusError in the chain.
		return fmt.Errorf("%w: %v", ErrCloneMaterialize, err)
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return fmt.Errorf("%w: %v", ErrCloneMaterialize, &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain})
	}
	var out resumeEditResp
	if uerr := json.Unmarshal(resp.body, &out); uerr != nil {
		return fmt.Errorf("%w: decode %s response: %v", ErrCloneMaterialize, name, uerr)
	}
	if len(out.Errors) > 0 {
		codes := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			codes = append(codes, e.Field+":"+e.Code)
		}
		return &ResumeUpdateValidationError{Codes: codes, Err: ErrCloneMaterialize}
	}
	// Success is the builder-state envelope ("resume":{…}) hh echoes after
	// an accepted save (live 200 shape); anything else that parses without
	// errors is an upstream anomaly — never claimed as success (the verify
	// read-back below is the authoritative gate either way).
	if _, perr := parseBuilderState(resp.body); perr != nil {
		return fmt.Errorf("%w: %s edit verdict: %s", ErrCloneMaterialize, name, snippet(resp.body))
	}
	return nil
}

// resumeEditResp is the error shape of the edit endpoint: hh answers
// validation/business rejections with HTTP 200 {"errors":[{field,code,…}]}
// ("title:REQUIRED" probed live 2026-09-29). Success bodies carry the
// builder-state envelope instead (parsed via parseBuilderState).
type resumeEditResp struct {
	Errors []struct {
		Field string `json:"field"`
		Code  string `json:"code"`
	} `json:"errors"`
}

// appliedEqual compares a changed key's sent value with the read-back
// value. hh stores texts verbatim but canonically normalizes dictionary
// strings — case plus dash/space variants («GitLab» stored as «Gitlab»,
// «Docker-compose» as «Docker Compose»; live 2026-09-29) and may
// reorder dictionary-driven string lists — so strings compare through
// normAppliedText, string arrays as normalized sets; everything else
// compares strictly (jsonEqual).
func appliedEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return jsonAppliedEqual(av, bv)
}

func jsonAppliedEqual(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && normAppliedText(av) == normAppliedText(bv)
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		switch {
		case allStrings(av) && allStrings(bv):
			// a = read-back, b = sent (see stringListCovers).
			return stringListCovers(av, bv)
		case allObjects(av) && allObjects(bv):
			// a = read-back, b = sent: every sent record must be covered
			// by some read record (see objectCovers).
			return objectListCovers(av, bv)
		}
		return positionalAppliedEqual(av, bv)
	default:
		return jsonEqual(a, b)
	}
}

func allStrings(vs []any) bool {
	for _, v := range vs {
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return len(vs) > 0
}

// stringListCovers reports whether every sent dictionary value surfaces in
// the read-back list. hh canonizes dictionary names, sometimes expanding
// them («Vault» stored as «HashiCorp Vault», live 2026-09-29), so equality
// OR containment of the normalized sent string counts; a dropped (unknown)
// value does not. a = read-back, b = sent.
func stringListCovers(a, b []any) bool {
	for _, sent := range b {
		ns := normAppliedText(sent.(string))
		covered := false
		for _, got := range a {
			ng := normAppliedText(got.(string))
			if ng == ns || strings.Contains(ng, ns) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func allObjects(vs []any) bool {
	for _, v := range vs {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	return len(vs) > 0
}

// objectCovers reports whether the sent record (b) is carried by the
// read-back record (a). hh re-forks shared records with NEW server ids on
// edit (live 2026-09-29: 2110984924 → 2110991452, same content) and may
// reorder the array, so "id" is excluded from the comparison and any
// read-back record can carry a sent one; every other sent field must
// match — strings through normAppliedText (equality or containment),
// everything else strictly.
func objectCovers(a, b map[string]any) bool {
	for k, sv := range b {
		if k == "id" {
			continue
		}
		gv, ok := a[k]
		if !ok {
			return false
		}
		if ss, ok1 := sv.(string); ok1 {
			if gs, ok2 := gv.(string); ok2 {
				ns, ng := normAppliedText(ss), normAppliedText(gs)
				if ns != ng && !strings.Contains(ng, ns) {
					return false
				}
				continue
			}
		}
		if !jsonEqual(sv, gv) {
			return false
		}
	}
	return true
}

// objectListCovers: every sent record must be carried by some read-back
// record (see objectCovers). a = read-back, b = sent.
func objectListCovers(a, b []any) bool {
	for _, so := range b {
		covered := false
		for _, got := range a {
			if objectCovers(got.(map[string]any), so.(map[string]any)) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

// positionalAppliedEqual is the strict fall-back for arrays that are
// neither string lists nor object records.
func positionalAppliedEqual(a, b []any) bool {
	for i := range a {
		if !jsonAppliedEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// normAppliedText folds the hh canonicalization surface: case, dash
// variants and whitespace runs are equivalent.
func normAppliedText(s string) string {
	s = strings.ToLower(s)
	r := strings.NewReplacer("-", " ", "–", " ", "—", " ", "−", " ")
	s = r.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// editURL renders the site-host partial-edit save URL for one resume.
func (c *Client) editURL(hash string) string {
	return c.endpointURL(epResumeEdit, c.eps.siteDefault("/applicant/resume/edit")) +
		"?resume=" + url.QueryEscape(hash) + "&hhtmSource=" + resumeEditHhtmSource
}

// editFlatValue serializes one builder-state field into the edit
// contract's flat value: conditional-format arrays ([{"string":v},…])
// become plain value arrays (["x"], [40] — the part value rides with its
// JSON type); anything else (empty arrays, experience item objects)
// travels verbatim — all four live shapes, captured 2026-09-29.
func editFlatValue(raw json.RawMessage) json.RawMessage {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) == 0 {
		return raw
	}
	flat := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		if len(p) != 1 {
			return raw
		}
		v, ok := p["string"]
		if !ok {
			return raw
		}
		flat = append(flat, v)
	}
	out, err := json.Marshal(flat)
	if err != nil {
		return raw
	}
	return out
}

// diffContentKeys returns "side.name" keys whose values differ between the
// live and the new content.
func diffContentKeys(live, next *ResumeContent) []string {
	var keys []string
	for side, lm := range map[string]map[string]json.RawMessage{"resume": live.Resume, "profile": live.Profile} {
		nm := sideOf(next, side)
		for k, lv := range lm {
			if nv, ok := nm[k]; !ok || !rawEqual(lv, nv) {
				keys = append(keys, side+"."+k)
			}
		}
		for k := range nm {
			if _, ok := lm[k]; !ok {
				keys = append(keys, side+"."+k)
			}
		}
	}
	return keys
}

func sideOf(rc *ResumeContent, side string) map[string]json.RawMessage {
	if side == "profile" {
		return rc.Profile
	}
	return rc.Resume
}

func splitContentKey(key string) (side, name string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:]
		}
	}
	return "resume", key
}

func rawEqual(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return jsonEqual(av, bv)
}

// jsonEqual compares decoded JSON values (maps ignore key order).
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvi, ok := bv[k]
			if !ok || !jsonEqual(v, bvi) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}
