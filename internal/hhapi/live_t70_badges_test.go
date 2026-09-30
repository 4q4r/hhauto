//go:build live

package hhapi

// T70 LIVE FACT-CHECK (manual, network + owner session required; never in
// the default gate):
//
//	go test -tags live -run TestLiveSearchBadgesT70 -v ./internal/hhapi/
//
// Answers one question: does the JSON payload the engine parses for search
// (HH-Lux-InitialState -> vacancySearchResult.vacancies[]) carry the
// employer badges the SSR markup renders next to the company name (trusted
// employer checkmark, hh rating, +N vacancies)? Dumps the full raw search
// body, the parsed state and full raw items into /tmp/opencode/t70/ and
// walks the state for badge-ish keys (badge/accredit/trust/premium/rating/
// verified) at any depth.
//
// Production inputs, like the T58 probe: /home/whoami/.config/hhauto/
// hhauto.toml, the captured TLS spec and the plain cookie store under the
// configured data_dir. No env vars required; nothing is printed from the
// cookies or the account.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const t70Dir = "/tmp/opencode/t70"

var t70BadgeKeyRe = regexp.MustCompile(`(?i)badge|accredit|trust|premium|rating|verified`)

// t70Walk collects "path = value" pairs for every object key matching the
// badge-ish pattern, at any depth of the decoded state.
func t70Walk(prefix string, v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			p := prefix + "." + k
			if t70BadgeKeyRe.MatchString(k) {
				b, _ := json.Marshal(sub)
				s := string(b)
				if len(s) > 240 {
					s = s[:240]
				}
				*out = append(*out, p+" = "+s)
			}
			t70Walk(p, sub, out)
		}
	case []any:
		for i, sub := range x {
			t70Walk(fmt.Sprintf("%s[%d]", prefix, i), sub, out)
		}
	}
}

func TestLiveSearchBadgesT70(t *testing.T) {
	// Production client, exactly like the T58 probe: owner config, captured
	// browser TLS spec, plain cookie store. No env vars needed.
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
	if err := os.MkdirAll(t70Dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", t70Dir, err)
	}

	// The owner's devops run shape: text + remote + RU + без агентств.
	q := SearchQuery{
		Text:            "devops",
		PerPage:         20,
		Areas:           []string{"113"},
		WorkFormats:     []string{"REMOTE"},
		LabelNoAgencies: true,
	}
	saved := 0
	for _, page := range []int{0, 1} {
		q.Page = page
		target := c.searchURL(q)
		resp, err := c.getDoc(ctx, "search_vacancies", target, c.withPinnedHost(c.eps.site+"/"))
		if err != nil {
			t.Fatalf("page %d request: %v", page, err)
		}
		if resp.status != 200 {
			t.Fatalf("page %d: status = %d (snippet: %.200s)", page, resp.status, snippet(resp.body))
		}
		body := filepath.Join(t70Dir, fmt.Sprintf("page%d.html", page))
		if werr := os.WriteFile(body, resp.body, 0o600); werr != nil {
			t.Fatalf("write %s: %v", body, werr)
		}
		state, ok := initialState(resp.body)
		if !ok {
			t.Fatalf("page %d: HH-Lux-InitialState MISSING (snippet: %.300s)", page, snippet(resp.body))
		}
		vsr := obj(state["vacancySearchResult"])
		if vsr == nil {
			t.Fatalf("page %d: vacancySearchResult MISSING", page)
		}
		items := arr(vsr["vacancies"])
		t.Logf("page %d: 200, %d bytes, %d vacancy items -> %s", page, len(resp.body), len(items), body)

		// SSR markup presence of the icon classes the owner saw.
		for _, cls := range []string{"trusted-employer-link", "employer-hh-rating", "badge"} {
			t.Logf("page %d SSR: %q present=%v count=%d", page, cls,
				strings.Contains(string(resp.body), cls), strings.Count(string(resp.body), cls))
		}

		// Recursive badge-ish key walk over the WHOLE parsed state (the
		// only JSON the engine holds for search).
		rawState, _ := json.Marshal(state)
		var full any
		if uerr := json.Unmarshal(rawState, &full); uerr != nil {
			t.Fatalf("page %d: unmarshal state: %v", page, uerr)
		}
		var hits []string
		t70Walk("state", full, &hits)
		seen := map[string]int{}
		for _, h := range hits {
			key := h[strings.LastIndex(h, ".")+1:]
			if i := strings.Index(key, " = "); i >= 0 {
				key = key[:i]
			}
			seen[key]++
			if seen[key] <= 3 {
				t.Logf("page %d KEY %s", page, h)
			}
		}
		for k, n := range seen {
			if n > 3 {
				t.Logf("page %d KEY .%s x%d (further hits deduped)", page, k, n)
			}
		}
		if len(hits) == 0 {
			t.Logf("page %d: NO badge-ish keys anywhere in the parsed state", page)
		}

		// Full raw items (2-3 total per the fact-check brief).
		want := 3
		if page == 1 {
			want = 2
		}
		for i, raw := range items {
			if i >= want {
				break
			}
			p := filepath.Join(t70Dir, fmt.Sprintf("item_p%d_%d.json", page, i))
			if werr := os.WriteFile(p, raw, 0o600); werr != nil {
				t.Fatalf("write %s: %v", p, werr)
			}
			saved++
			if i == 0 {
				it := obj(raw)
				keys := make([]string, 0, len(it))
				for k := range it {
					keys = append(keys, k)
				}
				t.Logf("page %d item0 top-level keys (%d): %s", page, len(keys), strings.Join(keys, ", "))
				emp := obj(it["employer"])
				ekeys := make([]string, 0, len(emp))
				for k := range emp {
					ekeys = append(ekeys, k)
				}
				t.Logf("page %d item0 employer keys (%d): %s", page, len(ekeys), strings.Join(ekeys, ", "))
			}
		}
	}
	t.Logf("saved %d full raw items to %s", saved, t70Dir)

	// T70 implementation proof: the typed Vacancy.Badges now fills from the
	// live search payload (company.badges.badge[]).
	q.Page = 0
	page, err := c.SearchVacancies(ctx, q)
	if err != nil {
		t.Fatalf("SearchVacancies: %v", err)
	}
	withBadges := 0
	for _, it := range page.Items {
		if len(it.Badges) > 0 {
			withBadges++
			t.Logf("vacancy %d %q employer %q badges %v", it.ID, it.Name, it.EmployerName, it.Badges)
		}
	}
	t.Logf("SearchVacancies: %d/%d items carry typed Badges", withBadges, len(page.Items))
}
