package hhapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// T28 native full-clone fixtures: synthetic builder-state and shard
// payloads (no real PII — titles are "DevOps-инженер Test", personal
// fields are test strings).

// cloneParentFixture extends the T27 builderStateFixture with the fields
// the native clone needs: the numeric resume id, and the title conditions
// (not_in carries the titles hh already has on the account). The parent's
// professionalRole rides as the live conditional format [{"string":160}].
func cloneParentFixture(hash, id, title string, notIn []string) string {
	return cloneParentFixtureRole(hash, id, title, `[{"string":160}]`, notIn)
}

// cloneParentFixtureRoleless drops the professionalRole field entirely: the
// clone must ride the ["160"] default (T45).
func cloneParentFixtureRoleless(hash, id, title string, notIn []string) string {
	return cloneParentFixtureRole(hash, id, title, "", notIn)
}

func cloneParentFixtureRole(hash, id, title, roleField string, notIn []string) string {
	titleField := "[]"
	if title != "" {
		titleField = `[{"string":` + mustJSON(title) + `}]`
	}
	notInJSON, _ := json.Marshal(notIn)
	roleLine := ""
	if roleField != "" {
		roleLine = `"professionalRole": ` + roleField + `,`
	}
	return `{
	  "nextIncompleteScreenId": null,
	  "screens": [
	    {"screenType":"STATIC","screenId":"professional_role","title":"Роль","fieldsOptions":{},"screenContent":[],"messages":[]}
	  ],
	  "resume": {
	    "_attributes": {"hash":` + mustJSON(hash) + `,"id":` + mustJSON(id) + `,"status":"modified","isSearchable":true},
	    "title": ` + titleField + `,
	    ` + roleLine + `
	    "keySkills": [{"string":"Linux"},{"string":"Docker"}],
	    "_conditions": {
	      "title": {"status":"ok","weight":2,"mincount":1,"maxcount":1,"parts":[{"string":{"required":true,"type":"string","minlength":2,"maxlength":100,"not_in":` + string(notInJSON) + `}}]},
	      "professionalRole": {"status":"ok","weight":2,"mincount":1,"maxcount":5,"parts":[{"string":{"required":true,"type":"integer"}}]}
	    }
	  },
	  "profile": {"fields": {"area": [{"string":1550}]}},
	  "additionalProperties": {}
	}`
}

// cloneShardFixture renders a /shards/applicant/resumes body: one published
// resume plus `clones` not-yet-useful entries parented to parentID and one
// native clone (status "new", searchable, parented, created last).
func cloneShardFixture(parentID, nativeHash string, clones int) string {
	var entries []string
	for i := 0; i < clones; i++ {
		entries = append(entries, `{"_attributes":{"hash":"sh3ll`+string(rune('a'+i))+`","id":"90000`+string(rune('0'+i))+`","hhid":"acct1","status":"not_finished","isSearchable":false,"created":1000,"parentResumeId":`+mustJSON(parentID)+`}}`)
	}
	if nativeHash != "" {
		entries = append(entries, `{"_attributes":{"hash":`+mustJSON(nativeHash)+`,"id":"288888888","hhid":"acct1","status":"new","isSearchable":true,"created":5000,"parentResumeId":`+mustJSON(parentID)+`},"professionalRole":[{"string":160}]}`)
	}
	body := `{"resumes":[` + strings.Join(entries, ",") + `]}`
	return body
}

// serveNativeCloneSurface wires the native clone_resumes endpoint (the BFF
// URL is overridden to the test server), the parent builder state, and the
// resume shard used for clone discovery. It records every clone_resumes
// body so tests can assert the exact SPA serialization. duplicateFirst
// makes the "(копия)" candidate come back with the live DUPLICATE business
// envelope and "(копия 2)" succeed.
func (env *testEnv) serveNativeCloneSurface(t *testing.T, parent string, duplicateFirst *bool) (cloneBodies *[]string) {
	t.Helper()
	cloneBodies = &[]string{}
	env.serve(env.mux1, "1", "POST", "/profile/shards/resume/clone_resumes", func(w http.ResponseWriter, r *http.Request) {
		body := env.lastBody("1:POST /profile/shards/resume/clone_resumes")
		*cloneBodies = append(*cloneBodies, body)
		// The SPA-exact header set that unlocks the app surface (live
		// 2026-09-25: missing pieces get a bare empty 400).
		if r.Header.Get("X-hhtmSource") != "resume_professional_role" || r.Header.Get("X-Requested-With") == "" || r.Header.Get("X-Xsrftoken") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// X-hhtmFrom must be PRESENT and empty (the interceptor sends the
		// key with an empty value; Header.Get cannot tell absence apart).
		if _, ok := r.Header[http.CanonicalHeaderKey("X-hhtmFrom")]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") != "same-site" || r.Header.Get("Origin") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if *duplicateFirst && strings.Contains(body, "(копия)") && !strings.Contains(body, "(копия 2)") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"errors":[{"field":"title","fieldNamePath":"/title","type":"BUSINESS","code":"DUPLICATE","value":"DevOps-инженер Test (копия)","message":"занято"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.URL.Query().Get("resumeHash") == "p4rent" {
			_, _ = w.Write([]byte(parent))
			return
		}
		// The verify read-back of the native clone: titled, finished,
		// searchable — the one-call duplicate.
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":null,"screens":[],"resume":{"_attributes":{"hash":"n4t1v3cl0ne","status":"new","isSearchable":true},"title":[{"string":"DevOps-инженер Test (копия 2)"}],"professionalRole":[{"string":160}]}}`))
	})
	env.serve(env.mux1, "1", "GET", "/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_abc123", Path: "/"})
		_, _ = w.Write([]byte(cloneShardFixture("285632195", "n4t1v3cl0ne", 2)))
	})
	return cloneBodies
}

func nativeEndpoints(env *testEnv) (c *Client) {
	env.t.Helper()
	eps := env.endpoints()
	if eps.Override == nil {
		eps.Override = map[string]string{}
	}
	eps.Override["resume_clone_resumes"] = env.srv1.URL + "/profile/shards/resume/clone_resumes"
	c, err := NewClient(env.account(), eps, nil, nil, WithBrowserTLSSpec(captureSpec(env.t)))
	if err != nil {
		env.t.Fatalf("NewClient: %v", err)
	}
	return c
}

// T28: the native full clone — one clone_resumes call carrying
// {resumeHash, resumesCreationProperties:[{title, roleIds}], userPrint} —
// creates a complete searchable duplicate; the new hash is discovered from
// the resume shard (parentResumeId lineage) and verified by read-back. The
// shell-clone endpoint is never touched.
func TestCloneResumeNativeFlow(t *testing.T) {
	env := newTestEnv(t)
	bodies := env.serveNativeCloneSurface(t, cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", []string{"Другое резюме"}), new(bool))
	c := nativeEndpoints(env)
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "n4t1v3cl0ne" {
		t.Fatalf("hash = %q, want n4t1v3cl0ne", hash)
	}
	if len(*bodies) == 0 {
		t.Fatal("clone_resumes was never called")
	}
	if env.hits("1:POST /applicant/resumes/clone") != 0 {
		t.Fatal("native flow must not create a shell via /applicant/resumes/clone")
	}
	body := (*bodies)[0]
	for _, want := range []string{
		`"resumeHash":"p4rent"`,
		`"resumesCreationProperties":[{"title":"DevOps-инженер Test (копия)","roleIds":["160"]}]`,
		`"userPrint":{}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("clone body %s misses %s", body, want)
		}
	}
}

// The parent title is already taken (not_in carries it plus the "(копия)"
// variants from earlier clone runs): the first candidate must skip straight
// to the free title.
func TestCloneResumeNativeTitleSkipsTaken(t *testing.T) {
	env := newTestEnv(t)
	// The "(копия)" title is already taken (registered by an earlier clone
	// run): the very first candidate must be the free "(копия)"-less bump —
	// well, the next free suffix: "(копия)" taken -> "(копия 2)".
	dup := new(bool)
	parent := cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", []string{"Другое резюме", "DevOps-инженер Test (копия)"})
	bodies := env.serveNativeCloneSurface(t, parent, dup)
	c := nativeEndpoints(env)
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "n4t1v3cl0ne" {
		t.Fatalf("hash = %q, want n4t1v3cl0ne", hash)
	}
	if len(*bodies) == 0 || !strings.Contains((*bodies)[0], `"title":"DevOps-инженер Test (копия 2)"`) {
		t.Fatalf("first candidate must skip the taken title, got %v", *bodies)
	}
}

// A DUPLICATE business verdict (live shape: HTTP 200 + errors array) bumps
// the title suffix and retries inside the same CloneResume call.
func TestCloneResumeNativeDuplicateBumps(t *testing.T) {
	env := newTestEnv(t)
	// Duplicate rejects the "(копия)" candidate; "(копия 2)" succeeds.
	dup := true
	bodies := env.serveNativeCloneSurface(t, cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", []string{"Другое резюме"}), &dup)
	c := nativeEndpoints(env)
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "n4t1v3cl0ne" {
		t.Fatalf("hash = %q, want n4t1v3cl0ne", hash)
	}
	if len(*bodies) != 2 {
		t.Fatalf("clone attempts = %d, want 2", len(*bodies))
	}
	if !strings.Contains((*bodies)[1], `"title":"DevOps-инженер Test (копия 2)"`) {
		t.Fatalf("second attempt must bump the suffix: %s", (*bodies)[1])
	}
}

// A parent without a professionalRole field: the clone must ride the
// ["160"] default (T45) — hh still demands a role dictionary id.
func TestCloneResumeNativeDefaultRoleIDs(t *testing.T) {
	env := newTestEnv(t)
	parent := cloneParentFixtureRoleless("p4rent", "285632195", "DevOps-инженер Test", []string{"Другое резюме"})
	bodies := env.serveNativeCloneSurface(t, parent, new(bool))
	c := nativeEndpoints(env)
	c.setXSRFForTests("tok_test")

	hash, err := c.CloneResume(context.Background(), "p4rent")
	if err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if hash != "n4t1v3cl0ne" {
		t.Fatalf("hash = %q, want n4t1v3cl0ne", hash)
	}
	if len(*bodies) == 0 || !strings.Contains((*bodies)[0], `"roleIds":["160"]`) {
		t.Fatalf("clone body must carry the default roleIds, got %v", *bodies)
	}
}

// The parent's own role rides verbatim: professionalRole [{"string":160}]
// becomes "roleIds":["160"] in the SPA serialization (strings, not numbers).
func TestCloneResumeNativeRoleIDsFromParent(t *testing.T) {
	env := newTestEnv(t)
	bodies := env.serveNativeCloneSurface(t, cloneParentFixture("p4rent", "285632195", "DevOps-инженер Test", []string{"Другое резюме"}), new(bool))
	c := nativeEndpoints(env)
	c.setXSRFForTests("tok_test")

	if _, err := c.CloneResume(context.Background(), "p4rent"); err != nil {
		t.Fatalf("CloneResume: %v", err)
	}
	if len(*bodies) == 0 || !strings.Contains((*bodies)[0], `"roleIds":["160"]`) {
		t.Fatalf("parent professionalRole must map to string roleIds, got %v", *bodies)
	}
}

// The resume shard parser exposes the lineage fields the discovery needs:
// parentResumeId and created (ms epoch), additively to the T22 contract.
func TestParseResumesLineage(t *testing.T) {
	rs, err := parseResumes([]byte(cloneShardFixture("285632195", "n4t1v3cl0ne", 2)))
	if err != nil {
		t.Fatalf("parseResumes: %v", err)
	}
	if len(rs) != 3 {
		t.Fatalf("resumes = %d, want 3", len(rs))
	}
	var native *Resume
	shells := 0
	for i := range rs {
		r := rs[i]
		if r.ParentResumeID != "285632195" {
			t.Fatalf("resume %d parentResumeID = %q, want 285632195", i, r.ParentResumeID)
		}
		switch r.Status {
		case "new":
			native = &rs[i]
		case "not_finished":
			shells++
		}
	}
	if native == nil {
		t.Fatal("native clone entry not parsed")
	}
	if native.HHID != "n4t1v3cl0ne" || !native.Searchable {
		t.Fatalf("native entry = %+v", native)
	}
	if shells != 2 {
		t.Fatalf("shells = %d, want 2", shells)
	}
}

// The live shard capture (anonymized) drives the same lineage contract:
// parentResumeId + created parse out of the real _attributes shape — only
// the native clone carries the lineage, exactly like live hh.
func TestLiveResumesLineage(t *testing.T) {
	rs, err := parseResumes(readLive(t, "resume_lineage.json"))
	if err != nil {
		t.Fatalf("parseResumes: %v", err)
	}
	if len(rs) != 3 {
		t.Fatalf("resumes = %d, want 3", len(rs))
	}
	clones := 0
	for _, r := range rs {
		if r.ParentResumeID == "" {
			continue
		}
		clones++
		if r.ParentResumeID != "285600000" {
			t.Fatalf("resume %s parentResumeID = %q, want 285600000", r.HHID, r.ParentResumeID)
		}
		if r.Status != "new" || !r.Searchable {
			t.Fatalf("native clone entry = %+v (want status new + searchable)", r)
		}
		if r.CreatedAt.IsZero() {
			t.Fatalf("native clone entry has no created timestamp: %+v", r)
		}
	}
	if clones != 1 {
		t.Fatalf("lineage parsed for %d resumes, want exactly the 1 native clone", clones)
	}
}
