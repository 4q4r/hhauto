package hhapi

// T57 key-skill levels tests: the captured PUT contract (golden body), the
// skillsLevels SSR parse (the only off-browser levels source) and the
// captured level enum. The fixtures mirror the live 2026-09-29 capture on
// clone 4f48f357… (ids/names synthetic where anonymized).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"hhauto/internal/config"
)

// skillsLevelsSSRFixture mirrors the captured page: the "skills" array is
// one of several HTML-escaped JSON blobs on the page; level null = «Уровень
// не указан» (HashiCorp Vault, CrowdSec — the live bug state), levels carry
// {id, internalId, name, rank}. A decoy "resumeApplicantSkills" array must
// never match the anchor.
const skillsLevelsSSRFixture = `<!DOCTYPE html><html><body>
<template style="display:none" id="HH-Lux-InitialState">{"resumeApplicantSkills":[{&#34;id&#34;:34,&#34;name&#34;:&#34;Decoy&#34;,&#34;category&#34;:&#34;SKILL&#34;,&#34;level&#34;:null}],&#34;skills&#34;:[{&#34;id&#34;:7521444,&#34;name&#34;:&#34;HashiCorp Vault&#34;,&#34;category&#34;:&#34;SKILL&#34;,&#34;level&#34;:null,&#34;verified&#34;:false,&#34;verifications&#34;:[]},{&#34;id&#34;:24418278,&#34;name&#34;:&#34;CrowdSec&#34;,&#34;category&#34;:&#34;SKILL&#34;,&#34;level&#34;:null,&#34;verified&#34;:false,&#34;verifications&#34;:[]},{&#34;id&#34;:260819,&#34;name&#34;:&#34;Go&#34;,&#34;category&#34;:&#34;SKILL&#34;,&#34;level&#34;:{&#34;id&#34;:9,&#34;internalId&#34;:&#34;middle&#34;,&#34;name&#34;:&#34;Средний&#34;,&#34;rank&#34;:2},&#34;verified&#34;:false,&#34;verifications&#34;:[{&#34;id&#34;:10479674,&#34;attempt&#34;:1}]},{&#34;id&#34;:110106,&#34;name&#34;:&#34;Ansible&#34;,&#34;category&#34;:&#34;SKILL&#34;,&#34;level&#34;:{&#34;id&#34;:8,&#34;internalId&#34;:&#34;base&#34;,&#34;name&#34;:&#34;Базовый&#34;,&#34;rank&#34;:1},&#34;verified&#34;:false,&#34;verifications&#34;:[]},{&#34;id&#34;:1114,&#34;name&#34;:&#34;Python&#34;,&#34;category&#34;:&#34;COMPETENCE&#34;,&#34;level&#34;:{&#34;id&#34;:10,&#34;internalId&#34;:&#34;advanced&#34;,&#34;name&#34;:&#34;Продвинутый&#34;,&#34;rank&#34;:3},&#34;verified&#34;:false,&#34;verifications&#34;:[]}],&#34;fieldStatuses&#34;:{&#34;redFields&#34;:[]}}</template>
</body></html>`

func TestParseSkillsLevelsPage(t *testing.T) {
	got, err := parseSkillsLevels([]byte(skillsLevelsSSRFixture))
	if err != nil {
		t.Fatalf("parseSkillsLevels: %v", err)
	}
	want := []UserSkill{
		{ID: 7521444, Name: "HashiCorp Vault", LevelID: 0},
		{ID: 24418278, Name: "CrowdSec", LevelID: 0},
		{ID: 260819, Name: "Go", LevelID: 9},
		{ID: 110106, Name: "Ansible", LevelID: 8},
	}
	if len(got) != len(want) {
		t.Fatalf("skills = %+v, want %d entries (non-SKILL categories filtered)", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("skill[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseSkillsLevelsEmpty(t *testing.T) {
	page := `{"skills":[]}`
	got, err := parseSkillsLevels([]byte(page))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty skills page: got %+v, err %v", got, err)
	}
	if _, err := parseSkillsLevels([]byte("<html>no anchor at all</html>")); err == nil {
		t.Fatal("a page without the anchor and without an empty skills list must fail loudly")
	}
}

func TestUserSkillsReadsSSRPage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /resume/edit/cl0ne1/skillsLevels", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(skillsLevelsSSRFixture))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eps := epsCfg()
	eps.Site = srv.URL
	c, err := NewClient(config.Account{DisplayName: "acc1", UserAgent: "ua/1"}, eps, nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := c.UserSkills(context.Background(), "cl0ne1")
	if err != nil {
		t.Fatalf("UserSkills: %v", err)
	}
	if len(got) != 4 || got[2].Name != "Go" || got[2].LevelID != 9 {
		t.Fatalf("UserSkills = %+v", got)
	}
}

// TestSetKeySkillLevelsPostsCapturedContract pins the live 2026-09-29 wire
// capture: PUT /shards/applicant/key_skills/levels/update with the
// userSkillLevels object entries (category SKILL, dictionary skill_id,
// captured level_id) — exactly what the «Указать уровни» UI saves.
func TestSetKeySkillLevelsPostsCapturedContract(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	var gotMethod string
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotMethod, gotBody, gotCT = r.Method, string(b), r.Header.Get("Content-Type")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	eps := epsCfg()
	eps.Site = srv.URL
	c, err := NewClient(config.Account{DisplayName: "acc1", UserAgent: "ua/1"}, eps, nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = c.SetKeySkillLevels(context.Background(), "cl0ne1", []KeySkillLevel{
		{SkillID: 260819, LevelID: 9},
		{SkillID: 24418278, LevelID: 8},
	})
	if err != nil {
		t.Fatalf("SetKeySkillLevels: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("method = %s, want PUT (captured contract)", gotMethod)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}
	// The golden wire body: object entries with the captured field order-
	// independent shape — skill dictionary id + level id, category SKILL.
	var body struct {
		Levels []keySkillLevelWire `json:"userSkillLevels"`
	}
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("decode body %q: %v", gotBody, err)
	}
	if len(body.Levels) != 2 ||
		body.Levels[0] != (keySkillLevelWire{Category: "SKILL", SkillID: 260819, LevelID: 9}) ||
		body.Levels[1] != (keySkillLevelWire{Category: "SKILL", SkillID: 24418278, LevelID: 8}) {
		t.Fatalf("body = %s, want the captured userSkillLevels objects", gotBody)
	}

	// Empty sets ride no request at all.
	fired := false
	eps2 := epsCfg()
	eps2.Site = srv.URL
	muxEmpty := http.NewServeMux()
	muxEmpty.HandleFunc("PUT /", func(http.ResponseWriter, *http.Request) { fired = true })
	srv2 := httptest.NewServer(muxEmpty)
	defer srv2.Close()
	eps2.Site = srv2.URL
	c2, err := NewClient(config.Account{DisplayName: "acc1", UserAgent: "ua/1"}, eps2, nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c2.SetKeySkillLevels(context.Background(), "cl0ne1", nil); err != nil {
		t.Fatalf("empty sets: %v", err)
	}
	if fired {
		t.Fatal("empty level sets must not fire any request")
	}
}

func TestSetKeySkillLevelsValidationTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"field":"userSkillLevels","code":"REQUIRED"}]}`))
	}))
	defer srv.Close()
	eps := epsCfg()
	eps.Site = srv.URL
	c, err := NewClient(config.Account{DisplayName: "acc1", UserAgent: "ua/1"}, eps, nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = c.SetKeySkillLevels(context.Background(), "cl0ne1", []KeySkillLevel{{SkillID: 1, LevelID: 9}})
	var ve *ResumeUpdateValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ResumeUpdateValidationError", err)
	}
	if len(ve.Codes) != 1 || ve.Codes[0] != "userSkillLevels:REQUIRED" {
		t.Fatalf("codes = %v", ve.Codes)
	}
}

func TestKeySkillLevelIDMapping(t *testing.T) {
	// The captured enum (levels dictionary, skillsLevels SSR, 2026-09-29).
	for name, want := range map[string]int64{"Базовый": 8, "Средний": 9, "Продвинутый": 10} {
		got, ok := KeySkillLevelID(name)
		if !ok || got != want {
			t.Fatalf("KeySkillLevelID(%q) = %d,%v; want %d,true", name, got, ok, want)
		}
	}
	if _, ok := KeySkillLevelID("Гуру"); ok {
		t.Fatal("a name outside the captured enum must not resolve")
	}
}
