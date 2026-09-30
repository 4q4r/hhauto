//go:build live71

package hhapi

// T71 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	HHAUTO_T71_CONFIG=~/.config/hhauto/hhauto.toml go test -tags live71 \
//	    -run TestLiveT71ORVerdict -v ./internal/hhapi/
//
// Establishes the verdict on how hh.ru parses the query TEXT operators
// (OR / AND / parentheses / quotes) by comparing found-counts across probe
// queries, plus the two incident facts: the SSR search item's employer
// block key ("company") and the Erlang vacancy's description tokens.
// Inputs (never printed): the owner's config and the plain cookie store.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/cookies"
	"hhauto/internal/tlsfp"
)

// Name-scoped union: search_field=name restricts the OR-union to vacancy
// NAMES — the verdict-driven config fix for the run queries (the union
// matched the Erlang vacancy only through its DESCRIPTION).
func TestLiveT71NameField(t *testing.T) {
	cfgPath := os.Getenv("HHAUTO_T71_CONFIG")
	if cfgPath == "" {
		cfgPath = os.Getenv("HOME") + "/.config/hhauto/hhauto.toml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acctID string
	var acct config.Account
	for id, a := range cfg.Accounts {
		acctID, acct = id, a
	}
	spec, serr := tlsfp.Load(filepath.Join(cfg.General.DataDir, "browser", "tls_spec.json"))
	if serr != nil {
		t.Fatalf("load tls spec: %v", serr)
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil,
		WithTransport(cfg.API.Transport), WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	store, err := cookies.NewPlainFile(cookies.StorageConfig{Path: filepath.Join(cfg.General.DataDir, "cookies")})
	if err != nil {
		t.Fatalf("plain store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	got, err := store.Load(ctx, acctID)
	if err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	if err := c.LoadCookies(ctx, got); err != nil {
		t.Fatalf("apply cookies: %v", err)
	}
	page, err := c.SearchVacancies(ctx, SearchQuery{Text: "devops OR sre", PerPage: 20, SearchFields: []string{"name"}})
	if err != nil {
		t.Fatalf("name-scoped search: %v", err)
	}
	names := make([]string, 0, 6)
	for i, v := range page.Items {
		if i >= 6 {
			break
		}
		names = append(names, v.Name)
	}
	t.Logf("NAME-SCOPED %q -> found=%d top=[%s]", "devops OR sre", page.Found, strings.Join(names, " | "))
	time.Sleep(2 * time.Second)
}

func TestLiveT71ORVerdict(t *testing.T) {
	cfgPath := os.Getenv("HHAUTO_T71_CONFIG")
	if cfgPath == "" {
		cfgPath = os.Getenv("HOME") + "/.config/hhauto/hhauto.toml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acctID string
	var acct config.Account
	for id, a := range cfg.Accounts {
		acctID, acct = id, a
	}
	spec, serr := tlsfp.Load(filepath.Join(cfg.General.DataDir, "browser", "tls_spec.json"))
	if serr != nil {
		t.Fatalf("load tls spec: %v", serr)
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil,
		WithTransport(cfg.API.Transport), WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient(tls spec): %v", err)
	}
	store, err := cookies.NewPlainFile(cookies.StorageConfig{Path: filepath.Join(cfg.General.DataDir, "cookies")})
	if err != nil {
		t.Fatalf("plain store: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	got, err := store.Load(ctx, acctID)
	if err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	if err := c.LoadCookies(ctx, got); err != nil {
		t.Fatalf("apply cookies: %v", err)
	}

	probes := []string{
		"devops OR sre",               // the owner's devops run query
		"devops sre",                  // plain AND pair
		"devops",                      // left operand alone
		"sre",                         // right operand alone
		"devops OR qzxkvwjmt",         // OR with a garbage token: union ≈ devops, AND ≈ 0
		"qzxkvwjmt",                   // the garbage alone (sanity ≈ 0)
		`fullstack OR "full stack"`,   // the owner's fullstack run query
		`"full stack"`,                // exact phrase
		"full stack",                  // the phrase as a plain AND pair
		"python AND (backend OR api)", // the owner's python run query
	}
	for _, text := range probes {
		page, err := c.SearchVacancies(ctx, SearchQuery{Text: text, PerPage: 20})
		if err != nil {
			t.Fatalf("search %q: %v", text, err)
		}
		names := make([]string, 0, 4)
		for i, v := range page.Items {
			if i >= 4 {
				break
			}
			names = append(names, v.Name)
		}
		t.Logf("QUERY %q -> found=%d items=%d top=[%s]", text, page.Found, len(page.Items),
			strings.Join(names, " | "))
		time.Sleep(2 * time.Second) // gentle on the SSR surface
	}

	// Incident fact 1: the search item's employer-ish keys.
	page, err := c.SearchVacancies(ctx, SearchQuery{Text: "devops OR sre", PerPage: 20})
	if err != nil {
		t.Fatalf("re-probe: %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("no items for the raw-shape probe")
	}
	var m map[string]any
	if err := json.Unmarshal(page.Items[0].Raw, &m); err != nil {
		t.Fatalf("raw item json: %v", err)
	}
	for _, k := range []string{"employer", "company"} {
		if v, ok := m[k]; ok {
			b, _ := json.Marshal(v)
			t.Logf("ITEM[%q] = %s", k, string(b))
		} else {
			t.Logf("ITEM[%q] ABSENT", k)
		}
	}
	it := page.Items[0]
	t.Logf("PARSED item: id=%d name=%q employer_id=%d employer_name=%q", it.ID, it.Name, it.EmployerID, it.EmployerName)

	// Incident fact 2: the Erlang vacancy's card description tokens.
	target := c.endpointURL(epVacancyCard, c.eps.siteDefault("/vacancy")) + "/90599106"
	resp, err := c.getDoc(ctx, "vacancy_card", target, c.withPinnedHost(c.eps.searchPage))
	if err != nil {
		t.Fatalf("card fetch: %v", err)
	}
	state, ok := initialState(resp.body)
	if !ok {
		t.Fatal("no initial state on the card page")
	}
	view := obj(state["vacancyView"])
	if view == nil {
		t.Fatal("vacancyView absent")
	}
	inner := obj(obj(view["vacancyFull"])["vacancy"])
	if inner == nil {
		inner = view // legacy flat shape
	}
	desc := strings.ToLower(str(inner, "description"))
	name := str(inner, "name")
	t.Logf("CARD 90599106: name=%q desc_len=%d", name, len(desc))
	for _, token := range []string{"erlang", "sre", "devops"} {
		i := strings.Index(desc, token)
		if i < 0 {
			t.Logf("CARD 90599106: token %q ABSENT", token)
			continue
		}
		lo, hi := i-70, i+len(token)+70
		if lo < 0 {
			lo = 0
		}
		if hi > len(desc) {
			hi = len(desc)
		}
		t.Logf("CARD 90599106: token %q at %d: ...%s...", token, i, strings.ReplaceAll(desc[lo:hi], "\n", " "))
	}
}
