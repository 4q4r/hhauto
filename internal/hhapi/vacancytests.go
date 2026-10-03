// vacancytests.go implements the T80 vacancy-test surface: the obligatory
// employer test attached to a vacancy («Тестирование соискателей», gated by
// the apply-popup hasTests flag) has no developer-API methods — it is
// submitted through the site's own response flow. The contract below is
// donor-derived (hh-applicant-tool behavior, 2026) and is deliberately
// fail-closed: any deviation from the expected shape is
// ErrVacancyTestUnavailable, which callers translate into the T77
// test_required skip. Nothing here guesses answers — the caller does.

package hhapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strconv"
)

// ErrVacancyTestUnavailable reports that the vacancy's test could not be
// read from the response page (no vacancyTests state, wrong vacancy key,
// malformed envelope). Callers treat it as the test_required skip.
var ErrVacancyTestUnavailable = errors.New("hhapi: vacancy test unavailable")

// TestSolution is one selectable answer of a test task.
type TestSolution struct {
	ID   string
	Text string
}

// TestTask is one question of a vacancy test. Choice tasks carry
// Solutions; open (free-text) tasks carry none.
type TestTask struct {
	ID        string
	Question  string
	Solutions []TestSolution
}

// VacancyTest is the test attached to one vacancy, as read from the
// response page state. UIDPk/GUID/StartTime are hh's own submission
// coordinates and are echoed back verbatim on the apply.
type VacancyTest struct {
	UIDPk     string
	GUID      string
	StartTime string
	Required  bool
	Tasks     []TestTask
}

// TestAnswer is one solved task: SolutionID for choice tasks, Text for
// open ones. A task with no honest answer is simply absent — the submit
// sends only what the AI derived from trusted facts.
type TestAnswer struct {
	TaskID     string
	SolutionID string
	Text       string
}

// ApplyTest carries the solved test on an ApplyRequest.
type ApplyTest struct {
	UIDPk     string
	GUID      string
	StartTime string
	Required  bool
	Answers   []TestAnswer
}

// VacancyTests implements hhapi.HHAPI: fetches the response page (document,
// account session) and extracts the vacancy's test from the embedded
// vacancyTests state. A captcha interstitial surfaces as ErrCaptcha.
func (c *Client) VacancyTests(ctx context.Context, vacancyID int64) (*VacancyTest, error) {
	u, err := url.Parse(c.withPinnedHost(c.endpointURL(epVacancyResponse, c.eps.siteDefault("/applicant/vacancy_response"))))
	if err != nil {
		return nil, fmt.Errorf("hhapi: vacancy_tests %d: %w", vacancyID, err)
	}
	q := u.Query()
	q.Set("vacancyId", strconv.FormatInt(vacancyID, 10))
	q.Set("startedWithQuestion", "false")
	q.Set("hhtmFrom", "vacancy")
	u.RawQuery = q.Encode()
	resp, err := c.getDoc(ctx, "vacancy_tests", u.String(), c.vacancyCardURL(vacancyID))
	if err != nil {
		return nil, err
	}
	// A captcha challenge on the page is a typed refusal (the captcha has
	// no solver — the caller skips the vacancy).
	if bodyHasAny(string(resp.body), captchaMarkers) {
		return nil, &StatusError{Kind: ErrCaptcha, StatusCode: resp.status, URL: resp.finalURL, BodySnippet: snippet(resp.body), Chain: resp.chain}
	}
	raw, ok := extractVacancyTests(resp.body)
	if !ok {
		return nil, fmt.Errorf("%w: %d: no vacancyTests state", ErrVacancyTestUnavailable, vacancyID)
	}
	env, has := raw[strconv.FormatInt(vacancyID, 10)]
	if !has {
		return nil, fmt.Errorf("%w: %d: vacancy not keyed in vacancyTests", ErrVacancyTestUnavailable, vacancyID)
	}
	vt, perr := parseVacancyTest(env)
	if perr != nil {
		return nil, fmt.Errorf("%w: %d: %v", ErrVacancyTestUnavailable, vacancyID, perr)
	}
	return vt, nil
}

// extractVacancyTests locates the vacancyTests key anywhere in the document
// (the state rides an inline script whose exact container is unstable) and
// decodes the value as {vacancyID: envelope}. Tolerant to number/string
// key drift via json.RawMessage. T83 live form: the whole state may be
// HTML-escaped (every quote is &#34;) — the escaped key is located and the
// remainder unescaped before decoding.
func extractVacancyTests(body []byte) (map[string]json.RawMessage, bool) {
	const key = `"vacancyTests"`
	i := bytes.Index(body, []byte(key))
	if i < 0 {
		const escKey = "&#34;vacancyTests&#34;"
		j := bytes.Index(body, []byte(escKey))
		if j < 0 {
			return nil, false
		}
		body = []byte(html.UnescapeString(string(body[j:])))
		i = bytes.Index(body, []byte(key))
		if i < 0 {
			return nil, false
		}
	}
	j := i + len(key)
	for j < len(body) && (body[j] == ' ' || body[j] == ':' || body[j] == '\n' || body[j] == '\r' || body[j] == '\t') {
		j++
	}
	dec := json.NewDecoder(bytes.NewReader(body[j:]))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}

// parseVacancyTest decodes one vacancy envelope. Missing submission
// coordinates are a parse failure (the apply would be rejected anyway).
func parseVacancyTest(raw json.RawMessage) (*VacancyTest, error) {
	var env struct {
		UIDPk     json.RawMessage `json:"uidPk"`
		GUID      json.RawMessage `json:"guid"`
		StartTime json.RawMessage `json:"startTime"`
		Required  bool            `json:"required"`
		Tasks     []struct {
			ID                 json.RawMessage `json:"id"`
			Description        string          `json:"description"`
			CandidateSolutions []struct {
				ID   json.RawMessage `json:"id"`
				Text string          `json:"text"`
			} `json:"candidateSolutions"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	vt := &VacancyTest{
		UIDPk:     rawLiteral(env.UIDPk),
		GUID:      rawLiteral(env.GUID),
		StartTime: rawLiteral(env.StartTime),
		Required:  env.Required,
	}
	if vt.UIDPk == "" || vt.GUID == "" {
		return nil, fmt.Errorf("envelope misses uidPk/guid")
	}
	for _, t := range env.Tasks {
		task := TestTask{ID: rawLiteral(t.ID), Question: t.Description}
		if task.ID == "" {
			return nil, fmt.Errorf("task without id")
		}
		for _, s := range t.CandidateSolutions {
			task.Solutions = append(task.Solutions, TestSolution{ID: rawLiteral(s.ID), Text: s.Text})
		}
		vt.Tasks = append(vt.Tasks, task)
	}
	return vt, nil
}

// rawLiteral renders a JSON scalar (string or number) as its wire value.
// Non-scalars and nulls render empty (fail-closed on the caller side).
func rawLiteral(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	if s == "null" {
		return ""
	}
	if s[0] == '"' {
		var out string
		if err := json.Unmarshal(raw, &out); err != nil {
			return ""
		}
		return out
	}
	return s
}

// applyTestForm adds the test submission fields to the apply form (the
// donor-captured shape: hh's own coordinates, honest constants and the
// per-task answers). Mutates form in place.
func applyTestForm(form url.Values, t *ApplyTest) {
	form.Set("uidPk", t.UIDPk)
	form.Set("guid", t.GUID)
	form.Set("startTime", t.StartTime)
	form.Set("testRequired", strconv.FormatBool(t.Required))
	form.Set("ignore_postponed", "true")
	form.Set("incomplete", "false")
	form.Set("mark_applicant_visible_in_vacancy_country", "false")
	form.Set("country_ids", "[]")
	form.Set("lux", "true")
	form.Set("withoutTest", "no")
	for _, a := range t.Answers {
		if a.SolutionID != "" {
			form.Set("task_"+a.TaskID, a.SolutionID)
		}
		if a.Text != "" {
			form.Set("task_"+a.TaskID+"_text", a.Text)
		}
	}
}
