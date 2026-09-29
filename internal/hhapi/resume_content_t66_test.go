package hhapi

// T66 verify-regression tests, built from the LIVE clone 943358f3… dumps
// (/tmp/opencode/t66/, 2026-09-29): hh's own builder-state presentation of
// resume.specializationTree flickers between reads (the nested
// specialization[].translit case flips: "programmirovanie-razrabotka" ↔
// "Programmirovanie-Razrabotka" — captured live), and the verify gate must
// not treat sections the apply never targeted (nor hh-normalized nested
// dictionary strings inside targeted sections) as failures.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// f66Fixture renders the 943358f3… builder-state shape (anonymized): the
// dictionary spec tree with nested translit strings and a lineage-wide
// experience pool (3 own + inherited records with full server field sets).
// lowerSpec selects the flicker form of the nested translit.
func f66Fixture(selfHash, specTranslit string) string {
	exp, _ := json.Marshal(f66Experience(selfHash))
	return `{
	  "nextIncompleteScreenId": null,
	  "screens": [{"screenType":"STATIC","screenId":"title","title":"Должность"}],
	  "resume": {
	    "_attributes": {"hash":` + mustJSON(selfHash) + `,"id":"285600001","status":"modified","isSearchable":true},
	    "title": [{"string":"Backend-разработчик (Python, FastAPI, PostgreSQL)"}],
	    "specializationTree": [{"id":1,"name":"professional_area.1","title":null,"translit":"informacionnye-tehnologii-internet-telekom","specialization":[{"id":221,"isLabour":false,"name":"specialization.221","title":null,"translit":` + mustJSON(specTranslit) + `}]}],
	    "_conditions": {}
	  },
	  "profile": {"fields": {"experience": ` + string(exp) + `}},
	  "additionalProperties": {}
	}`
}

// f66Experience is the live experience-pool shape of 943358f3…: own
// records (resumes carries the clone hash) plus inherited ones (other
// hashes), every server field present (verification object, empty
// industry arrays, nulls).
func f66Experience(selfHash string) []map[string]any {
	record := func(id int64, position string, startDate, endDate any, resumes []string) map[string]any {
		rr := make([]map[string]any, 0, len(resumes))
		for _, h := range resumes {
			rr = append(rr, map[string]any{"hash": h})
		}
		return map[string]any{
			"id": id, "startDate": startDate, "endDate": endDate,
			"companyName": "ООО «Ромашка»", "companyIndustryId": nil,
			"companyIndustries": []any{}, "companyAreaId": nil,
			"industries": []any{}, "companyUrl": nil, "companyId": nil,
			"position": position, "description": "Коммерческая разработка и автоматизация.",
			"employerId": nil, "companyState": nil, "companyLogos": nil,
			"professionId": 44, "professionName": "Fullstack-разработчик",
			"verification": map[string]any{"status": "NOT_VERIFIED", "canModify": true},
			"resumes":      rr,
		}
	}
	return []map[string]any{
		record(2111053911, "Python Backend Developer", "2024-01-01", nil, []string{selfHash}),
		record(2111053912, "Full-stack разработчик", "2026-06-01", "2026-08-01", []string{selfHash}),
		record(2111053913, "Разработчик ПО", "2025-04-01", "2025-09-01", []string{selfHash}),
		record(2103588721, "Python Backend Developer", "2024-01-01", nil, []string{"base1", "othe1"}),
		record(2103588722, "Full-stack разработчик", "2026-06-01", "2026-08-01", []string{"base2", "othe2"}),
	}
}

const (
	f66TranslitLower = "programmirovanie-razrabotka"
	f66TranslitUpper = "Programmirovanie-Razrabotka"
)

// f66Surface is a stateful builder-surface fake with hh's presentation
// flicker: reads from flipAt on answer the canonicalized form (uppercase
// nested translit — the captured live flip), and a save flips the form for
// the following reads too.
type f66Surface struct {
	mu     sync.Mutex
	state  string
	reads  int
	flipAt int
	bodies []string
	apply  bool
}

func (s *f66Surface) canon(state string) string {
	return strings.Replace(state, `"translit":"`+f66TranslitLower+`"`, `"translit":"`+f66TranslitUpper+`"`, 1)
}

func (s *f66Surface) current() string {
	if s.reads >= s.flipAt {
		return s.canon(s.state)
	}
	return s.state
}

func (s *f66Surface) mount(env *testEnv) {
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reads++
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.current()))
	})
	env.serve(env.mux1, "1", "POST", "/applicant/resume/edit", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		body := env.lastBody("1:POST /applicant/resume/edit")
		s.bodies = append(s.bodies, body)
		if r.Header.Get("X-hhtmSource") != "resume_partial_edit" || r.Header.Get("X-Xsrftoken") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s.apply {
			s.state = applyEditField(s.state, body)
			s.reads = s.flipAt // a save flips hh's presentation form
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(s.current()))
	})
}

func (s *f66Surface) postedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, b := range s.bodies {
		var p map[string]json.RawMessage
		if err := json.Unmarshal([]byte(b), &p); err == nil {
			for k := range p {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

func f66Endpoints(env *testEnv) *Client { return contentEndpoints(env) }

func f66FastVerify(t *testing.T) {
	t.Helper()
	origBudget, origPoll := verifyWaitBudget, verifyPollInterval
	verifyWaitBudget = 100 * time.Millisecond
	verifyPollInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		verifyWaitBudget, verifyPollInterval = origBudget, origPoll
	})
}

// T66: a section the plan never targeted must not ride the apply — not
// posted, not verified — even when hh's presentation of it changes between
// the plan-time read and the apply-time read (live: the spec-tree
// translit flip). The only target here is resume.title.
func TestUpdateResumeContentSkipsUntargetedSections(t *testing.T) {
	f66FastVerify(t)
	env := newTestEnv(t)
	surface := &f66Surface{state: f66Fixture("cl0ne66", f66TranslitLower), flipAt: 2, apply: true}
	surface.mount(env)
	c := f66Endpoints(env)

	base, err := c.ResumeContent(context.Background(), "cl0ne66")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	mutated := mutatedContent(base, "Backend-разработчик (Python, FastAPI, PostgreSQL, Docker)")

	err = c.UpdateResumeContent(context.Background(), "cl0ne66", mutated, []string{"resume.title"})
	if err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}
	keys := surface.postedKeys()
	if len(keys) != 1 || keys[0] != "title" {
		t.Fatalf("posted keys = %v, want exactly [title] — untargeted sections must not ride the apply", keys)
	}
}

// T66: when a TARGETED section's read-back carries hh's canonical form of
// a NESTED dictionary string (the live spec-tree translit flip), the
// verify gate must accept it — canonization applies at every depth, not
// only on top-level strings. apply=false: the fake stores the section
// verbatim like live hh (object arrays travel verbatim), and the read-back
// answers the canonicalized form.
func TestUpdateResumeContentVerifiesNestedCanonicalForm(t *testing.T) {
	f66FastVerify(t)
	env := newTestEnv(t)
	surface := &f66Surface{state: f66Fixture("cl0ne66", f66TranslitLower), apply: false}
	surface.mount(env)
	c := f66Endpoints(env)

	base, err := c.ResumeContent(context.Background(), "cl0ne66")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	mutated := base.Clone()
	mutated.Resume["specializationTree"], _ = json.Marshal([]map[string]any{{
		"id": 1, "name": "professional_area.1", "title": nil,
		"translit": "informacionnye-tehnologii-internet-telekom",
		"specialization": []map[string]any{{
			"id": 221, "isLabour": false, "name": "specialization.221",
			"title": nil, "translit": f66TranslitLower,
		}},
	}})

	// hh answers the save with the canonical (uppercase) nested translit —
	// the captured live flip — and the gate must accept that form.
	if err := c.UpdateResumeContent(context.Background(), "cl0ne66", mutated, []string{"resume.specializationTree"}); err != nil {
		t.Fatalf("UpdateResumeContent: %v", err)
	}
	var verr *ResumeContentVerifyError
	if errors.As(err, &verr) {
		t.Fatalf("canonical nested form must verify: %v", verr)
	}
}

// T66: a real mismatch of a targeted field must still fail the gate — the
// scoping fix does not open a silent-success path.
func TestUpdateResumeContentStillFailsRealTargetMismatch(t *testing.T) {
	f66FastVerify(t)
	env := newTestEnv(t)
	surface := &f66Surface{state: f66Fixture("cl0ne66", f66TranslitLower), apply: false}
	surface.mount(env)
	c := f66Endpoints(env)

	base, err := c.ResumeContent(context.Background(), "cl0ne66")
	if err != nil {
		t.Fatalf("ResumeContent: %v", err)
	}
	mutated := mutatedContent(base, "Совершенно новый заголовок T66")

	err = c.UpdateResumeContent(context.Background(), "cl0ne66", mutated, []string{"resume.title"})
	if err == nil {
		t.Fatal("verify mismatch must fail the update")
	}
	var ve *ResumeContentVerifyError
	if !errors.As(err, &ve) || len(ve.Keys) == 0 || ve.Keys[0] != "resume.title" {
		t.Fatalf("err = %v, want verify error for resume.title", err)
	}
}
