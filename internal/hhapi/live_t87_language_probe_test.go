//go:build live87

package hhapi

// T87 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live87 -run TestLive87LanguageProbe -count=1 -v ./internal/hhapi/
//
// Read-only: dumps the builder state of all six owner resumes and surfaces
// the language node (editor key "language", personalBuilderFields) plus any
// language-related _conditions. Full raw state lands in
// /tmp/opencode/t87/state-<hash8>.json. No POSTs here.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

var live87Hashes = []string{
	"fe31e03eff110666c30039ed1f4f5a59327851", // DevOps base
	"c8f0d470ff11044ad40039ed1f4b4d72506548", // Python base
	"7ea102d2ff1107845b0039ed1f6f484e45476a", // Fullstack base
	"4f48f357ff112c2fcc0039ed1f6c7555414349", // DevOps clone
	"943358f3ff112c2ff40039ed1f325344524b36", // Python clone
	"79d39537ff112c2ff30039ed1f733465596835", // Fullstack clone
}

func live87Client(t *testing.T) *Client {
	t.Helper()
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acct config.Account
	for _, a := range cfg.Accounts {
		acct = a
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	if spec.UserAgent != "" {
		acct.UserAgent = spec.UserAgent
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	raw, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c
}

// postLive87 fires one edit POST with the site-host XHR discipline plus the
// partial-edit hhtm headers (the T51 captured UI set).
func (c *Client) postLive87(ctx context.Context, url string, body []byte) (*response, error) {
	return c.doMutation(ctx, func() requestSpec {
		h := c.xhrHeaders(c.withPinnedHost(c.eps.site + "/applicant/resumes"))
		h.Set("Content-Type", "application/json")
		h.Set("X-hhtmSource", "resume_partial_edit")
		h.Set("X-hhtmFrom", "")
		return requestSpec{
			kind:      kindMutation,
			operation: "resume_edit_probe_t87",
			method:    http.MethodPost,
			url:       url,
			header:    h,
			body:      body,
		}
	})
}

// live87Lang is one builder-state language entry: dictionary id (api.hh.ru
// /languages uid — live cross-check 2026-10-03: rus=34 present in every
// owner resume, eng=57) + degree from _conditions enum.
type live87Lang struct {
	Degree string `json:"degree"`
	ID     int64  `json:"id"`
}

func live87ParseLangs(raw json.RawMessage) []live87Lang {
	var out []live87Lang
	_ = json.Unmarshal(raw, &out)
	return out
}

func live87LangEqual(a, b []live87Lang) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[live87Lang]int, len(a))
	for _, l := range a {
		seen[l]++
	}
	for _, l := range b {
		seen[l]--
		if seen[l] < 0 {
			return false
		}
	}
	return true
}

func live87ExpIDs(raw json.RawMessage) []int64 {
	var entries []struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(raw, &entries)
	out := make([]int64, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestLive87LanguageEdit(t *testing.T) {
	c := live87Client(t)
	if err := os.MkdirAll("/tmp/opencode/t87", 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	target := []live87Lang{{Degree: "l1", ID: 34}, {Degree: "b2", ID: 57}}
	targetRaw, _ := json.Marshal(target)
	body, _ := json.Marshal(map[string]json.RawMessage{"language": json.RawMessage(targetRaw)})

	// Baseline invariants captured BEFORE any edit (pool/rendered exp/titles).
	baseline := map[string]struct {
		title   string
		rendExp []int64
		pool    []int64
	}{}
	snapshot := func(hash string) (*builderState, error) {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			return nil, err
		}
		dump := map[string]any{"resume": st.Resume}
		if st.ProfileFields != nil {
			dump["profile"] = st.ProfileFields
		}
		blob, _ := json.MarshalIndent(dump, "", " ")
		p := "/tmp/opencode/t87/t87-" + hash[:8] + ".json"
		if werr := os.WriteFile(p, blob, 0o644); werr != nil {
			return nil, werr
		}
		return st, nil
	}
	invariants := func(hash string, st *builderState) (string, []int64, []int64) {
		var pool []int64
		if raw, ok := st.ProfileFields["experience"]; ok {
			pool = live87ExpIDs(raw)
		}
		rend := live87ExpIDs(st.Resume["experience"])
		return st.Title(), rend, pool
	}
	for _, hash := range live87Hashes {
		st, err := snapshot(hash)
		if err != nil {
			t.Fatalf("pre-read %s: %v", hash[:8], err)
		}
		title, rend, pool := invariants(hash, st)
		baseline[hash] = struct {
			title   string
			rendExp []int64
			pool    []int64
		}{title, rend, pool}
		t.Logf("PRE %s: title=%q lang=%s rend_exp=%v pool=%d", hash[:8], title, string(st.Resume["language"]), rend, len(pool))
	}

	editURL := func(hash string) string {
		return c.eps.siteDefault("/applicant/resume/edit") + "?resume=" + hash + "&hhtmSource=resume_partial_edit"
	}

	for _, hash := range live87Hashes {
		base := baseline[hash]
		resp, err := c.postLive87(ctx, editURL(hash), body)
		if err != nil {
			t.Fatalf("edit %s: transport: %v", hash[:8], err)
		}
		rp := "/tmp/opencode/t87/edit-resp-" + hash[:8] + ".json"
		_ = os.WriteFile(rp, resp.body, 0o644)
		var envelope struct {
			Errors []struct {
				Field string `json:"field"`
				Code  string `json:"code"`
			} `json:"errors"`
		}
		if json.Unmarshal(resp.body, &envelope) == nil && len(envelope.Errors) > 0 {
			t.Fatalf("edit %s: hh rejected: %+v (body %s)", hash[:8], envelope.Errors, snippet(resp.body))
		}
		if st, perr := parseBuilderState(resp.body); perr == nil {
			t.Logf("edit %s: resp state lang=%s", hash[:8], string(st.Resume["language"]))
		}

		// Re-read with a small poll budget (T55 silent no-op trap).
		var after *builderState
		landed := false
		for attempt := 0; attempt < 4 && !landed; attempt++ {
			if attempt > 0 {
				time.Sleep(2 * time.Second)
			}
			after, err = c.fetchBuilderState(ctx, hash)
			if err != nil {
				t.Fatalf("re-read %s: %v", hash[:8], err)
			}
			landed = live87LangEqual(live87ParseLangs(after.Resume["language"]), target)
		}
		if !landed {
			t.Fatalf("edit %s: language did not land: %s (silent no-op? payload %s)", hash[:8], string(after.Resume["language"]), string(body))
		}
		if after.Title() != base.title {
			t.Fatalf("edit %s: TITLE CHANGED %q -> %q", hash[:8], base.title, after.Title())
		}
		_, rend, pool := invariants(hash, after)
		if !equalInt64(rend, base.rendExp) {
			t.Fatalf("edit %s: rendered experience changed %v -> %v", hash[:8], base.rendExp, rend)
		}
		if !equalInt64(pool, base.pool) {
			t.Fatalf("edit %s: EXPERIENCE POOL CHANGED (fork?) %v -> %v", hash[:8], base.pool, pool)
		}
		t.Logf("POST %s: lang=%s ✓ title ✓ rendered_exp=%v ✓ pool=%d ✓", hash[:8], string(after.Resume["language"]), rend, len(pool))
		time.Sleep(3 * time.Second)
	}

	// Final sweep: all six must carry the target pair.
	for _, hash := range live87Hashes {
		st, err := snapshot(hash)
		if err != nil {
			t.Fatalf("final read %s: %v", hash[:8], err)
		}
		if !live87LangEqual(live87ParseLangs(st.Resume["language"]), target) {
			t.Errorf("final %s: language=%s != target %s", hash[:8], string(st.Resume["language"]), string(targetRaw))
		}
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLive87PopupLanguageAcceptance is the MAIN acceptance: find a live
// vacancy requiring English b1/b2, open the apply popup (read-only
// pre-flight), and prove resumeInconsistencies carries no LANGUAGE block —
// the T68-era warnings must be gone for every owner resume.
// TestLive87PopupLanguageControl is the non-vacuity control: a vacancy
// requiring English C1 must still produce LANGUAGE blocks — with
// "actual b2" now that the resumes carry English B2 (pre-T87 they had no
// English entry at all). This proves the matcher reads the new section.
// TestLive87StructuredLanguageHunt pops several live vacancies and reports
// each one's STRUCTURED language requirement (responseStatus.shortVacancy.
// languages) next to the per-resume inconsistency types — the goal is a
// vacancy that structurally requires English so the control is conclusive.
func TestLive87StructuredLanguageHunt(t *testing.T) {
	c := live87Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()

	queries := []string{"технический английский", "английский язык разработчик", "английский b2 devops"}
	seen := map[int64]bool{}
	structured := 0
	for _, qtext := range queries {
		page, err := c.SearchVacancies(ctx, SearchQuery{
			Text:            qtext,
			SearchFields:    []string{"name", "description"},
			LabelNoAgencies: true,
			OrderBy:         "publication_time_desc",
			PerPage:         15,
		})
		if err != nil {
			t.Logf("search %q: %v", qtext, err)
			continue
		}
		for i := range page.Items {
			v := &page.Items[i]
			if seen[v.ID] || len(seen) >= 9 {
				continue
			}
			seen[v.ID] = true
			pu, _ := url.Parse(c.endpointURL(epApplyPopup, c.eps.siteDefault("/applicant/vacancy_response/popup")))
			q := pu.Query()
			q.Set("vacancyId", strconv.FormatInt(v.ID, 10))
			pu.RawQuery = q.Encode()
			raw, err := c.getXHR(ctx, "apply_popup", c.withPinnedHost(pu.String()), c.vacancyCardURL(v.ID))
			if err != nil {
				t.Logf("popup %d: %v", v.ID, err)
				continue
			}
			_ = os.WriteFile("/tmp/opencode/t87/hunt-"+strconv.FormatInt(v.ID, 10)+".json", raw.body, 0o644)
			var env struct {
				ResponseStatus struct {
					ShortVacancy struct {
						Languages []struct {
							ID    string `json:"id"`
							Name  string `json:"name"`
							Level struct {
								ID   string `json:"id"`
								Name string `json:"name"`
							} `json:"level"`
						} `json:"languages"`
					} `json:"shortVacancy"`
					ResumeInconsistencies struct {
						Resume []struct {
							ID  int64 `json:"id"`
							Inc struct {
								Inconsistency []struct {
									Type     string `json:"type"`
									Required string `json:"required"`
									Actual   string `json:"actual"`
								} `json:"inconsistency"`
							} `json:"inconsistencies"`
						} `json:"resume"`
					} `json:"resumeInconsistencies"`
				} `json:"responseStatus"`
			}
			if jerr := json.Unmarshal(raw.body, &env); jerr != nil {
				t.Logf("popup %d parse: %v", v.ID, jerr)
				continue
			}
			langs := env.ResponseStatus.ShortVacancy.Languages
			langStr := ""
			for _, l := range langs {
				langStr += l.Name + "/" + l.Level.ID + " "
			}
			types := map[string]string{}
			for _, r := range env.ResponseStatus.ResumeInconsistencies.Resume {
				for _, inc := range r.Inc.Inconsistency {
					types[inc.Type] = "req=" + inc.Required + " act=" + inc.Actual
				}
			}
			tag := "no-structured-lang"
			if langStr != "" {
				tag = "STRUCTURED: " + langStr
				structured++
			}
			t.Logf("cand %d %q: %s | inconsistencies=%v", v.ID, v.Name, tag, types)
			time.Sleep(2 * time.Second)
		}
	}
	t.Logf("hunt done: %d candidates popped, %d with structured language requirements", len(seen), structured)
}

func TestLive87PopupLanguageControl(t *testing.T) {
	c := live87Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	page, err := c.SearchVacancies(ctx, SearchQuery{
		Text:            "английский c1",
		SearchFields:    []string{"name", "description"},
		LabelNoAgencies: true,
		OrderBy:         "publication_time_desc",
		PerPage:         30,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var pick *Vacancy
	for i := range page.Items {
		v := &page.Items[i]
		card, cerr := c.VacancyCard(ctx, v.ID)
		if cerr != nil {
			continue
		}
		text := strings.ToLower(card.SnippetRequirement + " " + card.Description)
		needsC1 := regexp.MustCompile(`английск[а-яё]*[^.]{0,120}c1`)
		if needsC1.MatchString(text) {
			pick = card
			t.Logf("CONTROL PICK %d %q emp=%q", card.ID, card.Name, card.EmployerName)
			break
		}
	}
	if pick == nil {
		t.Fatalf("no live c1 vacancy found in top %d", len(page.Items))
	}
	pu, _ := url.Parse(c.endpointURL(epApplyPopup, c.eps.siteDefault("/applicant/vacancy_response/popup")))
	q := pu.Query()
	q.Set("vacancyId", strconv.FormatInt(pick.ID, 10))
	pu.RawQuery = q.Encode()
	raw, err := c.getXHR(ctx, "apply_popup", c.withPinnedHost(pu.String()), c.vacancyCardURL(pick.ID))
	if err != nil {
		t.Fatalf("popup %d: %v", pick.ID, err)
	}
	_ = os.WriteFile("/tmp/opencode/t87/popup-control-"+strconv.FormatInt(pick.ID, 10)+".json", raw.body, 0o644)
	var env struct {
		ResponseStatus struct {
			ResumeInconsistencies struct {
				Resume []struct {
					ID  int64 `json:"id"`
					Inc struct {
						Inconsistency []struct {
							Type     string `json:"type"`
							Required string `json:"required"`
							Actual   string `json:"actual"`
						} `json:"inconsistency"`
					} `json:"inconsistencies"`
				} `json:"resume"`
			} `json:"resumeInconsistencies"`
		} `json:"responseStatus"`
	}
	if err := json.Unmarshal(raw.body, &env); err != nil {
		t.Fatalf("control parse: %v", err)
	}
	found := false
	for _, r := range env.ResponseStatus.ResumeInconsistencies.Resume {
		for _, inc := range r.Inc.Inconsistency {
			if strings.EqualFold(inc.Type, "LANGUAGE") {
				found = true
				t.Logf("CONTROL resume %d: LANGUAGE required=%q actual=%q", r.ID, inc.Required, inc.Actual)
			}
		}
	}
	if !found {
		t.Logf("CONTROL CONCERN: c1 vacancy produced no LANGUAGE block — matcher did not parse this vacancy's requirement (control inconclusive)")
		return
	}
	t.Logf("control conclusive: LANGUAGE engine active, actual=b2 (English B2 visible to hh)")
}

func TestLive87PopupLanguageAcceptance(t *testing.T) {
	c := live87Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()

	page, err := c.SearchVacancies(ctx, SearchQuery{
		Text:              "английский b2",
		SearchFields:      []string{"name", "description"},
		LabelNoAgencies:   true,
		OrderBy:           "publication_time_desc",
		PerPage:           30,
		ProfessionalRoles: nil,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	t.Logf("search candidates: %d", len(page.Items))
	var pick *Vacancy
	for i := range page.Items {
		v := &page.Items[i]
		card, cerr := c.VacancyCard(ctx, v.ID)
		if cerr != nil {
			t.Logf("card %d: %v", v.ID, cerr)
			continue
		}
		text := strings.ToLower(card.SnippetRequirement + " " + card.Description)
		hasEN := strings.Contains(text, "английск") || strings.Contains(text, "english")
		needsB := regexp.MustCompile(`английск[а-яё]*[^.]{0,120}(b1|b2)`)
		hasC1 := regexp.MustCompile(`английск[а-яё]*[^.]{0,120}c1`)
		if hasEN && needsB.MatchString(text) && !hasC1.MatchString(text) {
			pick = card
			t.Logf("PICK %d %q emp=%q", card.ID, card.Name, card.EmployerName)
			t.Logf("  requirement snippet: %.200s", card.SnippetRequirement)
			break
		}
	}
	if pick == nil {
		t.Fatalf("no live vacancy with clean english b1/b2 requirement found in top %d", len(page.Items))
	}

	// Raw popup (read-only pre-flight) — dump the full envelope for the
	// per-resume blocks, then parse with the production parser.
	pu, _ := url.Parse(c.endpointURL(epApplyPopup, c.eps.siteDefault("/applicant/vacancy_response/popup")))
	q := pu.Query()
	q.Set("vacancyId", strconv.FormatInt(pick.ID, 10))
	pu.RawQuery = q.Encode()
	raw, err := c.getXHR(ctx, "apply_popup", c.withPinnedHost(pu.String()), c.vacancyCardURL(pick.ID))
	if err != nil {
		t.Fatalf("popup %d: %v", pick.ID, err)
	}
	_ = os.WriteFile("/tmp/opencode/t87/popup-"+strconv.FormatInt(pick.ID, 10)+".json", raw.body, 0o644)
	meta, perr := parseApplyPopup(raw.body, pick.ID)
	if perr != nil {
		t.Fatalf("popup parse %d: %v", pick.ID, perr)
	}
	t.Logf("popup meta: alreadyApplied=%v responseImpossible=%v testRequired=%v hasTests=%v",
		meta.AlreadyApplied, meta.ResponseImpossible, meta.TestRequired, meta.HasTests)
	for _, inc := range meta.ResumeInconsistencies {
		t.Logf("popup inconsistency: %s", inc)
		if strings.Contains(strings.ToUpper(inc), "LANGUAGE") {
			t.Errorf("LANGUAGE inconsistency still present: %s", inc)
		}
	}
	resumes, err := c.ListResumes(ctx)
	if err != nil {
		t.Fatalf("list resumes: %v", err)
	}
	hashes := map[string]bool{}
	for _, r := range resumes {
		hashes[r.HHID] = true
	}
	for _, h := range live87Hashes {
		if !hashes[h] {
			t.Errorf("resume %s missing from account shard", h[:8])
		}
	}
	t.Logf("acceptance: vacancy %d checked against %d account resumes; LANGUAGE blocks: 0", pick.ID, len(resumes))
}

func TestLive87LanguageProbe(t *testing.T) {
	c := live87Client(t)
	if err := os.MkdirAll("/tmp/opencode/t87", 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	for _, hash := range live87Hashes {
		st, err := c.fetchBuilderState(ctx, hash)
		if err != nil {
			t.Fatalf("state %s: %v", hash[:8], err)
		}
		dump := map[string]any{"resume": st.Resume}
		if st.ProfileFields != nil {
			dump["profile"] = st.ProfileFields
		}
		blob, _ := json.MarshalIndent(dump, "", " ")
		path := "/tmp/opencode/t87/state-" + hash[:8] + ".json"
		if err := os.WriteFile(path, blob, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		keys := make([]string, 0, len(st.Resume))
		for k := range st.Resume {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Logf("%s: status=%q searchable=%v title=%q", hash[:8], st.Status(), st.IsSearchable, st.Title())
		t.Logf("%s: keys=%v", hash[:8], keys)
		if raw, ok := st.Resume["language"]; ok {
			t.Logf("%s: language node: %s", hash[:8], string(raw))
		} else {
			t.Logf("%s: language node: <absent>", hash[:8])
		}
		if raw, ok := st.Resume["_conditions"]; ok {
			var conds map[string]json.RawMessage
			_ = json.Unmarshal(raw, &conds)
			for _, k := range []string{"language", "languageIlevel", "languages"} {
				if v, ok := conds[k]; ok {
					s := string(v)
					if len(s) > 500 {
						s = s[:500] + "…"
					}
					t.Logf("%s: _conditions[%q]: %s", hash[:8], k, s)
				}
			}
			ck := make([]string, 0, len(conds))
			for k := range conds {
				ck = append(ck, k)
			}
			sort.Strings(ck)
			t.Logf("%s: _conditions keys=%v", hash[:8], ck)
		}
	}
}
