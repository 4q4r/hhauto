package hhapi

// T57 key-skill levels — the live contract of the hh.ru «Указать уровни»
// flow, captured 2026-09-29 (cloakbrowser on the owner session, clone
// 4f48f357…): the «Указать уровни» button opens the editor page
// /resume/edit/{hash}/skillsLevels and its save fires one account-scoped
// request per changed skill:
//
//	PUT {site}/shards/applicant/key_skills/levels/update
//	{"userSkillLevels":[{"category":"SKILL","level_id":9,"skill_id":260819}]}
//
// with the ordinary site XHR discipline (X-XSRFToken, XHR headers, JSON
// body). Levels are NOT part of the resume builder state — they attach to
// the account's userSkillLevels and are addressed by the key-skill
// DICTIONARY id (the same id the builder state carries in
// advancedKeySkills[].id and the resume page renders as
// data-qa="skill-tag-<id>"). level null = «Уровень не указан».
//
// There is no JSON GET for the levels: the only off-browser source is the
// skillsLevels SSR page, which embeds the resume's skill list as an
// HTML-escaped JSON array under the "skills" key —
// [{"id":7521444,"name":"HashiCorp Vault","category":"SKILL","level":null,…}]
// where level is null or {"id":9,"internalId":"middle","name":"Средний","rank":2}.
//
// Captured level enum (the same SSR page; live-verified: the PUT above
// landed and read back through this page on 2026-09-29):
//
//	8  = base     = Базовый     (rank 1)
//	9  = middle   = Средний     (rank 2)  — live-verified (Go → Средний)
//	10 = advanced = Продвинутый (rank 3)

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
)

// hhKeySkillLevelIDs maps the hh UI level names (RU) onto the captured
// level ids. Source: the levels dictionary embedded in the skillsLevels
// editor SSR page (see the file doc) — 8/9/10, captured live 2026-09-29.
var hhKeySkillLevelIDs = map[string]int64{
	"Базовый":     8,
	"Средний":     9,
	"Продвинутый": 10,
}

// KeySkillLevelID resolves a RU level name onto the captured id; ok=false
// for a name outside the captured enum (the caller applies its default).
func KeySkillLevelID(name string) (int64, bool) {
	id, ok := hhKeySkillLevelIDs[strings.TrimSpace(name)]
	return id, ok
}

// KeySkillLevel is one userSkillLevels entry of the captured PUT body.
type KeySkillLevel struct {
	SkillID int64 `json:"skill_id"`
	LevelID int64 `json:"level_id"`
}

// UserSkill is one resume key skill with its dictionary id and current
// level id (0 = «Уровень не указан»).
type UserSkill struct {
	ID      int64
	Name    string
	LevelID int64
}

// UserSkills reads the resume's key skills with their current levels by
// parsing the skillsLevels editor SSR (the only levels source off-browser;
// retryable like every read).
func (c *Client) UserSkills(ctx context.Context, hash string) ([]UserSkill, error) {
	target := c.withPinnedHost(c.eps.site + "/resume/edit/" + url.PathEscape(hash) + "/skillsLevels")
	resp, err := c.do(ctx, func() requestSpec {
		h := c.docHeaders(c.withPinnedHost(c.eps.site + "/resume/" + hash))
		return requestSpec{
			kind:      kindGet,
			operation: "resume_skills_levels",
			method:    http.MethodGet,
			url:       target,
			header:    h,
		}
	}())
	if err != nil {
		return nil, err
	}
	skills, perr := parseSkillsLevels(resp.body)
	if perr != nil {
		return nil, &StatusError{Kind: ErrUpstream, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	return skills, nil
}

// SetKeySkillLevels PUTs the captured levels contract (never auto-retried,
// like every mutation; validation rejections surface as
// *ResumeUpdateValidationError). The authoritative gate is the caller's
// UserSkills read-back, mirroring the resume-content verify pattern.
func (c *Client) SetKeySkillLevels(ctx context.Context, resumeHash string, sets []KeySkillLevel) error {
	if len(sets) == 0 {
		return nil
	}
	wire := make([]keySkillLevelWire, 0, len(sets))
	for _, s := range sets {
		wire = append(wire, keySkillLevelWire{Category: "SKILL", SkillID: s.SkillID, LevelID: s.LevelID})
	}
	body, err := json.Marshal(map[string][]keySkillLevelWire{"userSkillLevels": wire})
	if err != nil {
		return fmt.Errorf("%w: encode key skill levels: %v", ErrCloneMaterialize, err)
	}
	resp, merr := c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/resume/edit/" + url.PathEscape(resumeHash) + "/skillsLevels"))
		h.Set("Accept", "application/json")
		h.Set("Content-Type", "application/json")
		return requestSpec{
			kind:      kindMutation,
			operation: "key_skill_levels_update",
			method:    http.MethodPut,
			url:       c.withPinnedHost(c.eps.site + "/shards/applicant/key_skills/levels/update"),
			header:    h,
			body:      body,
		}
	})
	if merr != nil {
		return fmt.Errorf("%w: %v", ErrCloneMaterialize, merr)
	}
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return fmt.Errorf("%w: %v", ErrCloneMaterialize, &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain})
	}
	var out resumeEditResp
	if uerr := json.Unmarshal(resp.body, &out); uerr == nil && len(out.Errors) > 0 {
		codes := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			codes = append(codes, e.Field+":"+e.Code)
		}
		return &ResumeUpdateValidationError{Codes: codes, Err: ErrCloneMaterialize}
	}
	return nil
}

// keySkillLevelWire is the exact captured wire entry (2026-09-29):
// {"category":"SKILL","level_id":9,"skill_id":260819}.
type keySkillLevelWire struct {
	Category string `json:"category"`
	SkillID  int64  `json:"skill_id"`
	LevelID  int64  `json:"level_id"`
}

// skillsLevelsEntry is one decoded "skills" array item of the SSR page.
type skillsLevelsEntry struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Level    *struct {
		ID         int64  `json:"id"`
		InternalID string `json:"internalId"`
		Name       string `json:"name"`
		Rank       int    `json:"rank"`
	} `json:"level"`
}

// skillsLevelsAnchor is the escaped-JSON marker of the resume's skill list
// on the skillsLevels SSR page ("skills" is quoted, so longer keys such as
// resumeApplicantSkills never match).
const skillsLevelsAnchor = `"skills":[{"id":`

// parseSkillsLevels extracts the resume's key skills with their levels from
// the skillsLevels SSR HTML: unescape, anchor on the "skills" array,
// stream-decode it (the page carries several escaped JSON blobs; the
// decoder stops at the matching close bracket). A page without the anchor
// (resume without key skills) yields an empty list, not an error.
func parseSkillsLevels(body []byte) ([]UserSkill, error) {
	doc := html.UnescapeString(string(body))
	start := strings.Index(doc, skillsLevelsAnchor)
	if start < 0 {
		if strings.Contains(doc, `"skills":[]`) {
			return []UserSkill{}, nil
		}
		return nil, fmt.Errorf("skills levels page: anchor %q not found", skillsLevelsAnchor)
	}
	dec := json.NewDecoder(strings.NewReader(doc[start+len(`"skills":`):]))
	var entries []skillsLevelsEntry
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("skills levels page: decode array: %w", err)
	}
	out := make([]UserSkill, 0, len(entries))
	for _, e := range entries {
		if e.Category != "SKILL" {
			continue
		}
		s := UserSkill{ID: e.ID, Name: e.Name}
		if e.Level != nil {
			s.LevelID = e.Level.ID
		}
		out = append(out, s)
	}
	return out, nil
}
