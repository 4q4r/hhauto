//go:build live

package hhapi

// T83 wire bisect: hh's real response form (name="vacancy_response") has NO
// action — it posts to the CURRENT document URL (/applicant/vacancy_response
// ?vacancyId=N&startedWithQuestion=false), while our T80 contract posts to
// /applicant/vacancy_response/popup. The solved-test apply answers 400
// {"error":"unknown"} on /popup (live-proven 2026-10-03). This probe tries
// the form-faithful variants in order and logs each raw answer:
//
//	V0a: document URL + static form fields + resume_hash/vacancy_id
//	V0b: document URL + strictly the static form fields
//	V1:  popup URL + minimal fields + _xsrf form field
//
// Each 400 is harmless (nothing is created); the first success IS the
// response (owner-approved: QA target role). Stops at the first success.
//
//	go test -tags live -run TestLiveT83WireBisect -v ./internal/hhapi/

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

const t83ResumeHash = "fe31e03eff110666c30039ed1f4f5a59327851" // DevOps base resume

func t83BisectPost(t *testing.T, c *Client, label, target string, form url.Values, hhtmFrom bool) {
	t.Helper()
	h := c.xhrHeaders(c.vacancyCardURL(t83VacancyID))
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	h.Set("X-Hhtmfrom", "vacancy")
	h.Set("X-Hhtmsource", "vacancy_response")
	resp, err := c.doMutation(t.Context(), func() requestSpec {
		return requestSpec{
			kind: kindMutation, operation: "apply_test_" + label,
			method: http.MethodPost, url: target, header: h, body: []byte(form.Encode()),
		}
	})
	if err != nil {
		t.Logf("[%s] -> err=%v", label, err)
		return
	}
	t.Logf("[%s] -> status=%d finalURL=%s body=%.300s", label, resp.status, resp.finalURL, string(resp.body))
}

func TestLiveT83WireBisect(t *testing.T) {
	c := t83NewClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	tCtx := t.Context()

	// Fresh test coordinates (a fresh GET stamps a fresh startTime).
	vt, err := c.VacancyTests(tCtx, t83VacancyID)
	if err != nil {
		t.Fatalf("VacancyTests: %v", err)
	}
	t.Logf("coords: uidPk=%q guid=%q startTime=%q", vt.UIDPk, vt.GUID, vt.StartTime)

	// Honest answers, carried verbatim from the owner's profile facts (the
	// same content the live AI solve produced in phase B).
	ans1 := "Есть опыт тестирования сетевого стека через безопасность и нагрузку: в anicli-go — 881 автотест (unit/load/regression), в traffic_mimicry — эмуляция TLS-трафика под TSPU, в ERT — split-tunnel и сетевые сценарии."
	ans2 := "TCP/IP, DNS, HTTP/1.1, HTTP/2, HTTPS; глубокая работа с TLS (uTLS-мимикрия под браузерные фингерпринты), UDP, SOCKS/HTTP-прокси, wireguard."

	docURL := c.withPinnedHost(c.endpointURL(epVacancyResponse, c.eps.siteDefault("/applicant/vacancy_response"))) +
		fmt.Sprintf("?vacancyId=%d&startedWithQuestion=false", t83VacancyID)
	popupURL := c.withPinnedHost(c.endpointURL(epApplyPopup, c.eps.siteDefault("/applicant/vacancy_response/popup"))) +
		fmt.Sprintf("?vacancyId=%d", t83VacancyID)

	// V0a: document URL, static form fields + our base apply fields.
	formA := url.Values{}
	formA.Set("_xsrf", c.xsrfToken())
	formA.Set("uidPk", vt.UIDPk)
	formA.Set("guid", vt.GUID)
	formA.Set("startTime", vt.StartTime)
	formA.Set("testRequired", strconv.FormatBool(vt.Required))
	formA.Set("task_"+vt.Tasks[0].ID+"_text", ans1)
	formA.Set("task_"+vt.Tasks[1].ID+"_text", ans2)
	formA.Set("resume_hash", t83ResumeHash)
	formA.Set("vacancy_id", strconv.FormatInt(t83VacancyID, 10))
	t83BisectPost(t, c, "V0a", docURL, formA, true)

	// Success check: does the negotiation topic exist now?
	if topics, nerr := c.Negotiations(ctx, 0); nerr == nil {
		for _, n := range topics {
			if n.VacancyID == t83VacancyID {
				t.Logf("SUCCESS via V0a: topic %+v", n)
				return
			}
		}
	}

	// V0b: document URL, strictly the static form fields.
	formB := url.Values{}
	for k, vs := range formA {
		if k == "resume_hash" || k == "vacancy_id" {
			continue
		}
		formB[k] = vs
	}
	t83BisectPost(t, c, "V0b", docURL, formB, true)
	if topics, nerr := c.Negotiations(ctx, 0); nerr == nil {
		for _, n := range topics {
			if n.VacancyID == t83VacancyID {
				t.Logf("SUCCESS via V0b: topic %+v", n)
				return
			}
		}
	}

	// V1: popup URL, minimal fields + _xsrf form field.
	formC := url.Values{}
	for k, vs := range formA {
		formC[k] = vs
	}
	t83BisectPost(t, c, "V1", popupURL, formC, true)
	if topics, nerr := c.Negotiations(ctx, 0); nerr == nil {
		for _, n := range topics {
			if n.VacancyID == t83VacancyID {
				t.Logf("SUCCESS via V1: topic %+v", n)
				return
			}
		}
	}
	t.Logf("no variant produced a topic; audit the answers above")
}
