package hhapi

// T56 fake tests for DeleteExperienceRecords: a stateful in-memory lineage
// pool served over the test env (no real PII — companies are "Компания
// Test", hashes are synthetic).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// expRecordFixture renders one experience record JSON.
func expRecordFixture(id int64, resumes []string) string {
	rs := make([]string, 0, len(resumes))
	for _, h := range resumes {
		rs = append(rs, `{"hash":"`+h+`"}`)
	}
	return `{"id":` + itoa64(id) + `,"startDate":"2024-01-01","endDate":null,"companyName":"Компания Test","position":"Инженер Test","resumes":[` + strings.Join(rs, ",") + `],"description":"Описание теста"}`
}

func itoa64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// serveExperienceSurface wires the builder-state GET (returning the CURRENT
// pool) and the profile/update POST (guarding the live header set, then
// replacing the pool with the request's array). Returns the recorded update
// bodies.
func (env *testEnv) serveExperienceSurface(t *testing.T, hash string, pool *[]string) (updates *[]string) {
	t.Helper()
	updates = &[]string{}
	env.serve(env.mux1, "1", "GET", "/profile/shards/resume/get_by_hash", func(w http.ResponseWriter, _ *http.Request) {
		items := make([]string, 0, len(*pool))
		items = append(items, *pool...)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"nextIncompleteScreenId":null,"screens":[],"resume":{"_attributes":{"hash":"` + hash + `","id":"285632195","status":"modified","isSearchable":true}},"profile":{"fields":{"experience":[` + strings.Join(items, ",") + `]}},"additionalProperties":{}}`))
	})
	env.serve(env.mux1, "1", "POST", "/profile/shards/profile/update", func(w http.ResponseWriter, r *http.Request) {
		// The SPA-exact header set captured live (T56): missing pieces must
		// fail the test, not pass silently.
		if r.Header.Get("X-hhtmSource") != "profile_experience" || r.Header.Get("X-hhtmFrom") != "resume" ||
			r.Header.Get("X-hhtmSourceLabel") != "resume" || r.Header.Get("X-hhtmFromLabel") != "resume" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Xsrftoken") == "" || r.Header.Get("X-Requested-With") != "XMLHttpRequest" ||
			r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") != "same-site" || r.Header.Get("Origin") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body := env.lastBody("1:POST /profile/shards/profile/update")
		var req struct {
			Profile struct {
				Experience []json.RawMessage `json:"experience"`
			} `json:"profile"`
			ResumeFromHash string `json:"resumeFromHash"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil || req.ResumeFromHash != hash {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		replaced := make([]string, 0, len(req.Profile.Experience))
		for _, raw := range req.Profile.Experience {
			replaced = append(replaced, string(raw))
		}
		*pool = replaced
		*updates = append(*updates, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"NEW"}`))
	})
	return updates
}

func TestDeleteExperienceRecordsReplacesPool(t *testing.T) {
	const hash = "h3sh0001"
	pool := &[]string{
		expRecordFixture(101, []string{hash}),
		expRecordFixture(102, []string{hash}),
		expRecordFixture(103, []string{hash}),
		expRecordFixture(900, []string{hash, "other0"}),
	}
	env := newTestEnv(t)
	updates := env.serveExperienceSurface(t, hash, pool)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	left, err := c.DeleteExperienceRecords(ctx, hash, []int64{102})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if left != 3 {
		t.Fatalf("left=%d, want 3", left)
	}
	if len(*updates) != 1 {
		t.Fatalf("updates=%d, want 1", len(*updates))
	}
	body := (*updates)[0]
	if strings.Contains(body, `"id":102`) {
		t.Fatalf("update body still carries the deleted record: %s", body)
	}
	for _, want := range []string{`"id":101`, `"id":103`, `"id":900`} {
		if !strings.Contains(body, want) {
			t.Fatalf("update body missing %s: %s", want, body)
		}
	}
	if !strings.Contains(body, `"resumeFromHash":"`+hash+`"`) {
		t.Fatalf("update body missing resumeFromHash: %s", body)
	}
}

func TestDeleteExperienceRecordsRefusesShared(t *testing.T) {
	const hash = "h3sh0002"
	pool := &[]string{
		expRecordFixture(101, []string{hash}),
		expRecordFixture(900, []string{hash, "other0"}),
	}
	env := newTestEnv(t)
	updates := env.serveExperienceSurface(t, hash, pool)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	_, err := c.DeleteExperienceRecords(ctx, hash, []int64{900})
	if !errors.Is(err, ErrExperienceShared) {
		t.Fatalf("err=%v, want ErrExperienceShared", err)
	}
	if len(*updates) != 0 {
		t.Fatalf("shared refusal must not POST, got %d updates", len(*updates))
	}
	if len(*pool) != 2 {
		t.Fatalf("pool mutated on refusal: %d records", len(*pool))
	}
}

func TestDeleteExperienceRecordsIdempotentAbsent(t *testing.T) {
	const hash = "h3sh0003"
	pool := &[]string{expRecordFixture(101, []string{hash})}
	env := newTestEnv(t)
	updates := env.serveExperienceSurface(t, hash, pool)
	c := env.client()
	c.setXSRFForTests("tok_abc123")
	ctx, cancel := ctxb(t)
	defer cancel()

	left, err := c.DeleteExperienceRecords(ctx, hash, []int64{999})
	if err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if left != 1 || len(*updates) != 0 {
		t.Fatalf("left=%d updates=%d, want 1/0", left, len(*updates))
	}
}
