//go:build live

package hhapi

// T83 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live -run TestLiveT83TestHole -v ./internal/hhapi/
//
// Vacancy 137339427 slipped past BOTH the T77 popup gate (HasTests) and the
// T80 solver, then died with 400 {"error":"test-required"} on the apply
// POST. This probe answers, with raw dumps, which surface lied:
//   - the apply popup (does hasTests appear at all? in which shape?);
//   - the response page (is there a vacancyTests node? which shape?);
//   - the production VacancyTests parse of that same page.
//
// Read-only GETs only. Dumps land in /tmp/opencode/t83/ (never committed).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const (
	t83Dir        = "/tmp/opencode/t83"
	t83VacancyID  = int64(137339427)
	t83ConfigPath = "/home/whoami/.config/hhauto/hhauto.toml"
)

func t83NewClient(t *testing.T) *Client {
	t.Helper()
	cfg, err := config.Load(t83ConfigPath)
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
	return c
}

// t83TestKeys lists every JSON path whose key mentions "test" (any case),
// recursing into objects and arrays, so alternative flag shapes surface.
func t83TestKeys(t *testing.T, label string, v any, path string, out *[]string) {
	t.Helper()
	switch val := v.(type) {
	case map[string]any:
		for k, sub := range val {
			p := path + "." + k
			if strings.Contains(strings.ToLower(k), "test") {
				raw, _ := json.Marshal(sub)
				*out = append(*out, fmt.Sprintf("%s = %.200s", p, raw))
			}
			t83TestKeys(t, label, sub, p, out)
		}
	case []any:
		for i, sub := range val {
			t83TestKeys(t, label, sub, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

func TestLiveT83TestHole(t *testing.T) {
	c := t83NewClient(t)
	if err := os.MkdirAll(t83Dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// (1) The apply popup — the T77 gate surface.
	popupURL := c.withPinnedHost(c.endpointURL(epApplyPopup, c.eps.siteDefault("/applicant/vacancy_response/popup"))) +
		fmt.Sprintf("?vacancyId=%d", t83VacancyID)
	resp, err := c.getXHR(ctx, "apply_popup_t83", popupURL, c.vacancyCardURL(t83VacancyID))
	if err != nil {
		t.Fatalf("popup GET: %v", err)
	}
	t.Logf("popup: status=%d bytes=%d", resp.status, len(resp.body))
	popupPath := filepath.Join(t83Dir, fmt.Sprintf("popup_%d.json", t83VacancyID))
	if err := os.WriteFile(popupPath, resp.body, 0o600); err != nil {
		t.Fatalf("write popup dump: %v", err)
	}
	var popupAny any
	if err := json.Unmarshal(resp.body, &popupAny); err == nil {
		var keys []string
		t83TestKeys(t, "popup", popupAny, "$", &keys)
		sort.Strings(keys)
		for _, k := range keys {
			t.Logf("popup test-key: %s", k)
		}
	}
	if meta, perr := parseApplyPopup(resp.body, t83VacancyID); perr != nil {
		t.Logf("parseApplyPopup ERROR: %v", perr)
	} else {
		t.Logf("parseApplyPopup: HasTests=%v LetterRequired=%v Chat=%q AlreadyApplied=%v Inconsistencies=%d",
			meta.HasTests, meta.LetterRequired, meta.ChatWritePossibility, meta.AlreadyApplied, len(meta.ResumeInconsistencies))
	}

	// (2) The response page — the T80 solver surface.
	docURL := c.withPinnedHost(c.endpointURL(epVacancyResponse, c.eps.siteDefault("/applicant/vacancy_response"))) +
		fmt.Sprintf("?vacancyId=%d&startedWithQuestion=false&hhtmFrom=vacancy", t83VacancyID)
	doc, err := c.getDoc(ctx, "vacancy_tests_t83", docURL, c.vacancyCardURL(t83VacancyID))
	if err != nil {
		t.Fatalf("response page GET: %v", err)
	}
	t.Logf("response page: status=%d bytes=%d", doc.status, len(doc.body))
	docPath := filepath.Join(t83Dir, fmt.Sprintf("response_%d.html", t83VacancyID))
	if err := os.WriteFile(docPath, doc.body, 0o600); err != nil {
		t.Fatalf("write response dump: %v", err)
	}
	raw, ok := extractVacancyTests(doc.body)
	if !ok {
		t.Logf("extractVacancyTests: NO vacancyTests state in the page")
	} else {
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Logf("extractVacancyTests: keys=%v", keys)
		key := fmt.Sprintf("%d", t83VacancyID)
		if env, has := raw[key]; has {
			t.Logf("vacancyTests[%s] raw: %.2000s", key, env)
			envPath := filepath.Join(t83Dir, fmt.Sprintf("vacancytests_%d.json", t83VacancyID))
			_ = os.WriteFile(envPath, env, 0o600)
		}
	}

	// (3) What production VacancyTests makes of it.
	vt, verr := c.VacancyTests(ctx, t83VacancyID)
	switch {
	case verr != nil:
		t.Logf("VacancyTests ERROR: %v", verr)
	case vt == nil:
		t.Logf("VacancyTests: nil, nil (unexpected)")
	default:
		t.Logf("VacancyTests: uidPk=%q guid=%q startTime=%q required=%v tasks=%d",
			vt.UIDPk, vt.GUID, vt.StartTime, vt.Required, len(vt.Tasks))
		for i, task := range vt.Tasks {
			t.Logf("  task[%d]: id=%q q=%.120q solutions=%d", i, task.ID, task.Question, len(task.Solutions))
		}
	}
}
