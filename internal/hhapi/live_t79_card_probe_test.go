//go:build live

package hhapi

// T79 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live -run TestLiveVacancyCardT79 -v ./internal/hhapi/
//
// Answers one question: what does the production vacancy-card surface
// (/vacancy/{id} SSR -> HH-Lux-InitialState -> vacancyView) actually return
// for the vacancies whose cover letters died with the empty-record refusal
// ("Запись вакансии пуста"), and where the full payload lives
// (flat vacancyView vs nested vacancyFull.vacancy vs api.hh.ru JSON).
//
// Production inputs, like the T70 probe: /home/whoami/.config/hhauto/
// hhauto.toml, the captured TLS spec and the plain cookie store under the
// configured data_dir. No env vars required; nothing is printed from the
// cookies or the account. Dumps land in /tmp/opencode/t79/.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const t79Dir = "/tmp/opencode/t79"

func t79ParseFields(t *testing.T, label string, id int64, view map[string]json.RawMessage) {
	t.Helper()
	vb, err := json.Marshal(view)
	if err != nil {
		t.Logf("%s id=%d: marshal: %v", label, id, err)
		return
	}
	v := vacancyFromItem(vb)
	t.Logf("%s id=%d: name=%q employer=%d/%q area=%q published=%s descLen=%d altURL=%q",
		label, id, v.Name, v.EmployerID, v.EmployerName, v.Area,
		v.PublishedAt.Format(time.RFC3339), len(v.Description), v.AlternateURL)
}

func TestLiveVacancyCardT79(t *testing.T) {
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
	rawCk, err := os.ReadFile(filepath.Join(cfg.General.DataDir, "cookies", "main.bin"))
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(rawCk, &blob); err != nil {
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
	if err := os.MkdirAll(t79Dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", t79Dir, err)
	}

	// The known empty-record ids from the live review_items log.
	emptyIDs := []int64{136803802, 137622280}

	// One fresh control id from a live search (must not be one of the empties).
	if page, err := c.SearchVacancies(ctx, SearchQuery{
		Text: "devops", PerPage: 10, Areas: []string{"113"},
		WorkFormats: []string{"REMOTE"}, LabelNoAgencies: true,
	}); err != nil {
		t.Logf("control search failed (continuing with the empties only): %v", err)
	} else {
		known := map[int64]bool{136803802: true, 137622280: true}
		for _, it := range page.Items {
			if !known[it.ID] {
				emptyIDs = append(emptyIDs, it.ID)
				t.Logf("control id from search: %d (%s)", it.ID, it.Name)
				break
			}
		}
	}

	for _, id := range emptyIDs {
		target := c.endpointURL(epVacancyCard, c.eps.siteDefault("/vacancy")) + "/" + strconv.FormatInt(id, 10)
		referer := c.withPinnedHost(c.eps.searchPage)
		resp, err := c.getDoc(ctx, "vacancy_card_t79", target, referer)
		if err != nil {
			t.Logf("id=%d: GET failed: %v", id, err)
			continue
		}
		t.Logf("id=%d: status=%d bytes=%d", id, resp.status, len(resp.body))
		body := filepath.Join(t79Dir, fmt.Sprintf("card_%d.html", id))
		if werr := os.WriteFile(body, resp.body, 0o600); werr != nil {
			t.Fatalf("write %s: %v", body, werr)
		}
		state, ok := initialState(resp.body)
		if !ok {
			t.Logf("id=%d: NO HH-Lux-InitialState (snippet: %.300s)", id, snippet(resp.body))
			continue
		}
		view := obj(state["vacancyView"])
		if view == nil {
			t.Logf("id=%d: vacancyView MISSING; state keys:", id)
			for k := range state {
				t.Logf("  state.%s", k)
			}
			continue
		}
		keys := make([]string, 0, len(view))
		for k := range view {
			keys = append(keys, k)
		}
		t.Logf("id=%d: vacancyView keys=%v", id, keys)

		// Production parse (flat view) — what VacancyCard returns today.
		t79ParseFields(t, "flat", id, view)

		// Nested vacancyFull.vacancy candidate.
		if full := obj(obj(view["vacancyFull"])["vacancy"]); full != nil {
			fkeys := make([]string, 0, len(full))
			for k := range full {
				fkeys = append(fkeys, k)
			}
			t.Logf("id=%d: vacancyFull.vacancy keys=%v", id, fkeys)
			t79ParseFields(t, "nested", id, full)
		} else {
			t.Logf("id=%d: vacancyFull.vacancy ABSENT", id)
		}

		// Raw view JSON for offline audit.
		if vb, jerr := json.Marshal(view); jerr == nil {
			vj := filepath.Join(t79Dir, fmt.Sprintf("view_%d.json", id))
			_ = os.WriteFile(vj, vb, 0o600)
		}

		// What production VacancyCard reports right now.
		if v, verr := c.VacancyCard(ctx, id); verr != nil {
			t.Logf("id=%d: VacancyCard error: %v", id, verr)
		} else {
			t.Logf("id=%d: VacancyCard -> name=%q employer=%d/%q published=%s descLen=%d",
				id, v.Name, v.EmployerID, v.EmployerName, v.PublishedAt.Format(time.RFC3339), len(v.Description))
		}

		// Candidate second path: public vacancy JSON on api.hh.ru.
		apiTarget := "https://api.hh.ru/vacancies/" + strconv.FormatInt(id, 10)
		if apiResp, aerr := c.getXHR(ctx, "vacancy_api_t79", apiTarget, ""); aerr != nil {
			t.Logf("id=%d: api.hh.ru GET failed: %v", id, aerr)
		} else {
			head := string(apiResp.body)
			if len(head) > 300 {
				head = head[:300]
			}
			t.Logf("id=%d: api.hh.ru status=%d head=%.300s", id, apiResp.status, head)
		}
	}
}
