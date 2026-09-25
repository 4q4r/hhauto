package autoclone

// T30 editor runner tests: the browser (chromedp) apply of an improve plan.
// The suite is Chrome-free: the browser is a seam (browserRunner) faked
// here, and the mock editor page (httptest, data-qa fixture) pins the
// selector contract the in-page fill JS relies on. The real-Chrome live
// flow lives under the live30 tag.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/html"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/review"
	"hhauto/internal/store"
)

// --- fakes ------------------------------------------------------------------

// fakeCookieSource hands out scripted per-account cookies.
type fakeCookieSource struct {
	byAccount map[string][]*http.Cookie
	err       error
}

func (f *fakeCookieSource) EditorCookies(_ context.Context, accountID string) ([]*http.Cookie, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byAccount[accountID], nil
}

// fakeBrowser records the screens it visited and the ops it applied.
type fakeBrowser struct {
	mu      sync.Mutex
	visited []string
	ops     []editorFillOp
	// failLocators are reported as not_found outcomes.
	failLocators map[string]bool
	// apply mutates the editor state so the runner's read-back verify sees
	// the filled values (nil = no mutation: verify must then fail).
	apply func(url string, ops []editorFillOp)
	// err fails the whole screen run (transport-level).
	err error
}

func (b *fakeBrowser) runScreen(_ context.Context, url string, ops []editorFillOp) ([]editorOpOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.visited = append(b.visited, url)
	b.ops = append(b.ops, ops...)
	if b.err != nil {
		return nil, b.err
	}
	if b.apply != nil {
		b.apply(url, ops)
	}
	out := make([]editorOpOutcome, 0, len(ops))
	for _, op := range ops {
		if b.failLocators[op.Field] {
			out = append(out, editorOpOutcome{Field: op.Field, OK: false, Err: "not_found"})
			continue
		}
		out = append(out, editorOpOutcome{Field: op.Field, OK: true, Selector: op.Selectors[0]})
	}
	return out, nil
}

// scriptedBrowserFactory wires the fake browser into the runner and keeps
// the cleanup flag for the isolation assertions. A pre-set browser is
// reused by start (scripted scenarios); otherwise start creates one.
type fakeBrowserFactory struct {
	browser   *fakeBrowser
	closed    bool
	startErr  error
	cookiesIn []*http.Cookie
}

func (f *fakeBrowserFactory) start(_ context.Context, cookies []*http.Cookie) (browserRunner, func(), error) {
	if f.startErr != nil {
		return nil, func() {}, f.startErr
	}
	f.cookiesIn = cookies
	if f.browser == nil {
		f.browser = &fakeBrowser{}
	}
	return f.browser, func() { f.closed = true }, nil
}

// planByLocator indexes a plan by its unique locator.
func planByLocator(plan []FieldChange) map[string]FieldChange {
	m := make(map[string]FieldChange, len(plan))
	for _, fc := range plan {
		m[fc.Locator] = fc
	}
	return m
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func testSessionCookie() []*http.Cookie {
	return []*http.Cookie{{Name: "_xsrf", Value: "tok", Domain: ".hh.ru", Path: "/", Secure: true}}
}

// editorTestRunner assembles a ChromeEditorRunner over the fakes.
func editorTestRunner(t *testing.T, cfg config.AutoCloneImproveEditor, db *store.DB,
	verify ContentReader, factory *fakeBrowserFactory, cookies EditorCookieSource,
) *ChromeEditorRunner {
	t.Helper()
	if cookies == nil {
		cookies = &fakeCookieSource{byAccount: map[string][]*http.Cookie{"acc1": testSessionCookie()}}
	}
	r, err := NewChromeEditorRunner(EditorOptions{
		Cfg: cfg, DB: db, Cookies: cookies, Verify: verify,
		Browser: factory.start, Log: testLoggerT(),
	})
	if err != nil {
		t.Fatalf("NewChromeEditorRunner: %v", err)
	}
	return r
}

func editorCfg(baseURL string) config.AutoCloneImproveEditor {
	return config.AutoCloneImproveEditor{
		Enabled: true, BaseURL: baseURL,
		PageTimeoutSec: 30, SaveTimeoutSec: 5,
	}
}

// testPlan covers every fillable kind on the T29 fixture state.
func testPlan() []FieldChange {
	return []FieldChange{
		{Field: "title", Locator: "resume.title", Screen: "title", Kind: KindText,
			Label: "Заголовок резюме", Item: -1, Value: json.RawMessage(`"DevOps-платформенный инженер"`)},
		{Field: "key_skills", Locator: "resume.keySkills", Screen: "key_skills", Kind: KindTextList,
			Label: "Ключевые навыки", Item: -1, Value: json.RawMessage(`["Linux","Docker","Go"]`)},
		{Field: "experience_description", Locator: "profile.experience.0.description", Screen: "experience",
			Kind: KindText, Label: "Описание опыта", Item: 0,
			Value: json.RawMessage(`"Построил CI с нуля: 40 сборок в день."`)},
		{Field: "experience_dates", Locator: "profile.experience.1.dates", Screen: "experience",
			Kind: KindDate, Label: "Даты опыта", Item: 1,
			Value: json.RawMessage(`{"startDate":"2022-06-01","endDate":null}`)},
	}
}

// applyOpsToState is the fake "hh accepted the save": natural DOM values
// ride the real feature validators/writers into the editor state.
func applyOpsToState(t *testing.T, st *contentState, planByLocator map[string]FieldChange) func(string, []editorFillOp) {
	t.Helper()
	return func(_ string, ops []editorFillOp) {
		rc := st.toResumeContent()
		// Date pairs need both halves; collect them before writing.
		pairs := map[string]map[string]string{}
		for _, op := range ops {
			fc, ok := planByLocator[op.Field]
			if !ok {
				t.Fatalf("op for unknown locator %q", op.Field)
			}
			spec, ok := Spec(fc.Field)
			if !ok {
				t.Fatalf("unknown spec %q", fc.Field)
			}
			switch spec.Kind {
			case KindText:
				raw, err := ValidateValue(spec, rc, op.Value)
				if err != nil {
					t.Fatalf("validate %s: %v", fc.Field, err)
				}
				if werr := writeValue(spec, rc, fc.Item, raw); werr != nil {
					t.Fatalf("write %s: %v", fc.Field, werr)
				}
			case KindTextList:
				raw, err := ValidateValue(spec, rc, strings.Split(op.Value, ", "))
				if err != nil {
					t.Fatalf("validate %s: %v", fc.Field, err)
				}
				if werr := writeValue(spec, rc, fc.Item, raw); werr != nil {
					t.Fatalf("write %s: %v", fc.Field, werr)
				}
			case KindSalary:
				n, err := strconv.ParseFloat(op.Value, 64)
				if err != nil {
					t.Fatalf("salary value %q: %v", op.Value, err)
				}
				raw, err := ValidateValue(spec, rc, n)
				if err != nil {
					t.Fatalf("validate salary: %v", err)
				}
				if werr := writeValue(spec, rc, fc.Item, raw); werr != nil {
					t.Fatalf("write salary: %v", werr)
				}
			case KindDate:
				if spec.Key == "birthday" {
					raw, err := ValidateValue(spec, rc, op.Value)
					if err != nil {
						t.Fatalf("validate birthday: %v", err)
					}
					if werr := writeValue(spec, rc, fc.Item, raw); werr != nil {
						t.Fatalf("write birthday: %v", werr)
					}
					continue
				}
				m, ok := pairs[op.Field]
				if !ok {
					m = map[string]string{}
					pairs[op.Field] = m
				}
				m[op.Part] = op.Value
			default:
				t.Fatalf("unexpected op kind %q for %s", spec.Kind, fc.Field)
			}
		}
		for locator, m := range pairs {
			fc := planByLocator[locator]
			spec, _ := Spec(fc.Field)
			end := m["end"]
			probe := map[string]*string{"startDate": strPtr(m["start"])}
			if end != "" {
				probe["endDate"] = strPtr(end)
			}
			raw, err := ValidateValueAt(spec, rc, fc.Item, probe)
			if err != nil {
				t.Fatalf("validate dates: %v", err)
			}
			if werr := writeValue(spec, rc, fc.Item, raw); werr != nil {
				t.Fatalf("write dates: %v", werr)
			}
		}
		// Persist back into the shared fake state (the verify read path).
		st.Resume = rc.Resume
		st.Profile = rc.Profile
	}
}

func strPtr(s string) *string { return &s }

// --- pure planning ----------------------------------------------------------

func TestEditorScreenURLDefault(t *testing.T) {
	// Live-verified 2026-09-26: the editor route is /resume/edit/{resume}/{segment}.
	got, err := editorScreenURL("https://hh.ru", nil, "abc123", FieldChange{
		Field: "title", Screen: "title", Item: -1,
	})
	if err != nil {
		t.Fatalf("editorScreenURL: %v", err)
	}
	want := "https://hh.ru/resume/edit/abc123/position"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
	// Salary rides the same page; about and per-item experience map to
	// their own verified segments.
	for _, tc := range []struct {
		fc   FieldChange
		want string
	}{
		{FieldChange{Field: "salary", Screen: "position_and_salary", Item: -1}, "https://hh.ru/resume/edit/abc123/position"},
		{FieldChange{Field: "about", Screen: "about", Item: -1}, "https://hh.ru/resume/edit/abc123/about"},
		{FieldChange{Field: "experience_description", Screen: "experience", Item: 0}, "https://hh.ru/resume/edit/abc123/experience/0"},
		{FieldChange{Field: "key_skills", Screen: "key_skills", Item: -1}, "https://hh.ru/resume/edit/abc123/key_skills"},
	} {
		got, err = editorScreenURL("https://hh.ru", nil, "abc123", tc.fc)
		if err != nil {
			t.Fatalf("%s: %v", tc.fc.Field, err)
		}
		if got != tc.want {
			t.Fatalf("%s url = %q, want %q", tc.fc.Field, got, tc.want)
		}
	}
}

func TestEditorScreenURLOverride(t *testing.T) {
	overrides := map[string]string{
		"title":      "https://edit.hh.ru/wizard/{resume}?screen={screen}",
		"experience": "https://hh.ru/applicant/resume/{resume}/experience/item/{item}",
	}
	got, err := editorScreenURL("https://hh.ru", overrides, "abc123", FieldChange{Field: "title", Screen: "title", Item: -1})
	if err != nil {
		t.Fatalf("override title: %v", err)
	}
	if want := "https://edit.hh.ru/wizard/abc123?screen=title"; got != want {
		t.Fatalf("override url = %q, want %q", got, want)
	}
	got, err = editorScreenURL("https://hh.ru", overrides, "abc123", FieldChange{Field: "experience_description", Screen: "experience", Item: 1})
	if err != nil {
		t.Fatalf("override experience: %v", err)
	}
	if want := "https://hh.ru/applicant/resume/abc123/experience/item/1"; got != want {
		t.Fatalf("item url = %q, want %q", got, want)
	}
}

func TestEditorScreenURLTemplateValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		overrides map[string]string
		fc        FieldChange
	}{
		"missing resume": {overrides: map[string]string{"title": "https://hh.ru/wizard?screen={screen}"}, fc: FieldChange{Screen: "title", Field: "title"}},
		"missing item":   {overrides: map[string]string{"experience": "https://hh.ru/wizard/{resume}/{screen}"}, fc: FieldChange{Screen: "experience", Field: "experience_description", Item: 0}},
		"stray item":     {overrides: map[string]string{"experience": "https://hh.ru/w/{resume}/{screen}/{item}"}, fc: FieldChange{Screen: "experience", Field: "experience_description", Item: -1}},
	} {
		if _, err := editorScreenURL("https://hh.ru", tc.overrides, "h", tc.fc); err == nil {
			t.Fatalf("%s: template without required placeholders must be rejected", name)
		}
	}
	// {screen} inside a per-screen override is optional: the map key
	// already pins the screen.
	if _, err := editorScreenURL("https://hh.ru",
		map[string]string{"title": "https://edit.hh.ru/wizard/{resume}/title"}, "h", FieldChange{Screen: "title", Field: "title"}); err != nil {
		t.Fatalf("override without {screen} is legitimate: %v", err)
	}
	// The default pattern needs no {item}: item-bearing changes ride the
	// verified /experience/{item} segments without any override.
	if _, err := editorScreenURL("https://hh.ru", nil, "h", FieldChange{Screen: "experience", Field: "experience_description", Item: 0}); err != nil {
		t.Fatalf("default experience url with an item change: %v", err)
	}
}

func TestGroupPlanByURLKeepsFirstSeenOrder(t *testing.T) {
	plan := testPlan() // position, key_skills, experience/0, experience/1
	groups, errs := groupPlanByURL("https://hh.ru", nil, "clone1", plan)
	if len(errs) != 0 {
		t.Fatalf("plan errors: %+v", errs)
	}
	if len(groups) != 4 {
		t.Fatalf("groups = %d, want 4: %+v", len(groups), groups)
	}
	wantOrder := []string{
		"https://hh.ru/resume/edit/clone1/position",
		"https://hh.ru/resume/edit/clone1/key_skills",
		"https://hh.ru/resume/edit/clone1/experience/0",
		"https://hh.ru/resume/edit/clone1/experience/1",
	}
	for i, g := range groups {
		if g.URL != wantOrder[i] {
			t.Fatalf("group %d url = %q, want %q", i, g.URL, wantOrder[i])
		}
	}
}

func TestBuildScreenOpsByKind(t *testing.T) {
	plan := testPlan()
	ops, errs := buildScreenOps(plan)
	if len(errs) != 0 {
		t.Fatalf("buildable plan produced errors: %+v", errs)
	}
	if len(ops) != 5 { // title + key_skills + description + date pair
		t.Fatalf("ops = %d, want 5: %+v", len(ops), ops)
	}
	byField := map[string][]editorFillOp{}
	for _, op := range ops {
		byField[op.Field] = append(byField[op.Field], op)
	}
	if ops := byField["resume.title"]; len(ops) != 1 || ops[0].Selectors[0] != `[data-qa="resume-edit-title-suggest"]` || ops[0].Value != "DevOps-платформенный инженер" {
		t.Fatalf("title op wrong: %+v", ops)
	}
	if ops := byField["resume.keySkills"]; len(ops) != 1 || ops[0].Value != "Linux, Docker, Go" {
		t.Fatalf("key_skills op wrong: %+v", ops)
	}
	if ops := byField["profile.experience.0.description"]; len(ops) != 1 || ops[0].Selectors[0] != `[data-qa="resume-editor-experience-description-input"]` {
		t.Fatalf("description op wrong: %+v", ops)
	}
	dates := byField["profile.experience.1.dates"]
	if len(dates) != 2 || dates[0].Part != "start" || dates[1].Part != "end" {
		t.Fatalf("date pair ops wrong: %+v", dates)
	}
	if dates[0].Selectors[0] != `[data-qa="resume-editor-experience-start-year-input"]` || dates[1].Selectors[0] != `[data-qa="resume-editor-experience-end-year-input"]` {
		t.Fatalf("date pair selectors wrong: %+v", dates)
	}
	if dates[0].Value != "2022-06-01" || dates[1].Value != "" {
		t.Fatalf("date pair values wrong: %+v", dates)
	}
}

func TestBuildScreenOpsRejectsStructured(t *testing.T) {
	plan := []FieldChange{{
		Field: "professional_roles", Locator: "resume.professionalRole", Screen: "professional_role",
		Kind: KindStructured, Label: "Профессиональные роли", Item: -1,
		Value: json.RawMessage(`[1]`),
	}}
	ops, errs := buildScreenOps(plan)
	if len(ops) != 0 {
		t.Fatalf("structured field must not produce ops: %+v", ops)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Err, "structured") {
		t.Fatalf("structured field must fail with an explicit reason: %+v", errs)
	}
}

// --- runner flow ------------------------------------------------------------

// TestApplyFieldPlanEndToEnd runs the whole runner against the mock editor
// page: URLs point at the httptest fixture, the fixture carries the
// data-qa selectors the ops target, the fake save mutates the editor state
// and the read-back verify gates the result.
func TestApplyFieldPlanEndToEnd(t *testing.T) {
	st := newFakeEditor(t)
	srv := mockEditorServer(t, st)
	defer srv.Close()

	factory := &fakeBrowserFactory{browser: &fakeBrowser{
		apply: applyOpsToState(t, &st.state, planByLocator(testPlan())),
	}}
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	r := editorTestRunner(t, editorCfg(srv.URL), db, st, factory, nil)

	res, err := r.ApplyFieldPlan(context.Background(), "clone1", testPlan())
	if err != nil {
		t.Fatalf("ApplyFieldPlan: %v", err)
	}
	wantApplied := []string{"resume.title", "resume.keySkills", "profile.experience.0.description", "profile.experience.1.dates"}
	if len(res.Applied) != len(wantApplied) {
		t.Fatalf("applied = %v (failed %+v), want %v", res.Applied, res.Failed, wantApplied)
	}
	for _, loc := range wantApplied {
		if !contains(res.Applied, loc) {
			t.Fatalf("locator %q missing from applied: %v", loc, res.Applied)
		}
	}
	if len(res.Failed) != 0 {
		t.Fatalf("unexpected failures: %+v", res.Failed)
	}
	// Screens visited in first-seen order; dates and description share the
	// experience screen.
	wantVisited := []string{
		srv.URL + "/resume/edit/clone1/position",
		srv.URL + "/resume/edit/clone1/key_skills",
		srv.URL + "/resume/edit/clone1/experience/0",
		srv.URL + "/resume/edit/clone1/experience/1",
	}
	if len(factory.browser.visited) != len(wantVisited) {
		t.Fatalf("visited = %v, want %v", factory.browser.visited, wantVisited)
	}
	for i, u := range wantVisited {
		if factory.browser.visited[i] != u {
			t.Fatalf("visited[%d] = %q, want %q", i, factory.browser.visited[i], u)
		}
	}
	// The account session cookies reached the browser and nothing else.
	if len(factory.cookiesIn) != 1 || factory.cookiesIn[0].Name != "_xsrf" {
		t.Fatalf("browser cookies = %v, want the _xsrf session", factory.cookiesIn)
	}
	if !factory.closed {
		t.Fatal("browser cleanup was not called")
	}
}

// TestMockEditorPageMatchesSelectors pins the fixture↔selector contract:
// every op's primary data-qa selector must resolve on the mock page, the
// same way the live editor's pages carry them.
func TestMockEditorPageMatchesSelectors(t *testing.T) {
	srv := mockEditorServer(t, newFakeEditor(t))
	defer srv.Close()
	groups, errs := groupPlanByURL(srv.URL, nil, "clone1", testPlan())
	if len(errs) != 0 {
		t.Fatalf("plan errors: %+v", errs)
	}
	for _, g := range groups {
		ops, oerrs := buildScreenOps(g.Changes)
		if len(oerrs) != 0 {
			t.Fatalf("%s: plan errors: %+v", g.URL, oerrs)
		}
		page := fetchPage(t, g.URL)
		for _, op := range ops {
			if !htmlHasDataQA(t, page, dataQAOf(op.Selectors[0])) {
				t.Fatalf("%s: fixture has no %s for op %+v", g.URL, op.Selectors[0], op)
			}
		}
	}
}

// TestApplyFieldPlanBrowserFailureIsolatesFields: a not_found op fails only
// its own change; the rest still apply and verify.
func TestApplyFieldPlanBrowserFailureIsolatesFields(t *testing.T) {
	st := newFakeEditor(t)
	srv := mockEditorServer(t, st)
	defer srv.Close()

	factory := &fakeBrowserFactory{}
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	r := editorTestRunner(t, editorCfg(srv.URL), db, st, factory, nil)
	factory.browser = &fakeBrowser{
		failLocators: map[string]bool{"resume.keySkills": true},
		apply:        applyOpsToState(t, &st.state, planByLocator(testPlan())),
	}

	res, err := r.ApplyFieldPlan(context.Background(), "clone1", testPlan())
	if err != nil {
		t.Fatalf("ApplyFieldPlan: %v", err)
	}
	if !contains(res.Applied, "resume.title") || contains(res.Applied, "resume.keySkills") {
		t.Fatalf("applied = %v, want title in, keySkills out", res.Applied)
	}
	if len(res.Failed) != 1 || res.Failed[0].Field != "resume.keySkills" {
		t.Fatalf("failed = %+v, want keySkills only", res.Failed)
	}
}

// TestApplyFieldPlanVerifyGate: a browser that reports success but does not
// actually mutate the resume must fail verification, not claim applied.
// Every planned value differs from the live fixture (the dates change
// shifts the start date) so a trivial live-equality cannot pass the gate.
func TestApplyFieldPlanVerifyGate(t *testing.T) {
	st := newFakeEditor(t)
	srv := mockEditorServer(t, st)
	defer srv.Close()

	plan := testPlan()
	for i := range plan {
		if plan[i].Locator == "profile.experience.1.dates" {
			plan[i].Value = json.RawMessage(`{"startDate":"2022-07-01","endDate":null}`)
		}
	}
	factory := &fakeBrowserFactory{} // apply == nil: nothing lands
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	r := editorTestRunner(t, editorCfg(srv.URL), db, st, factory, nil)

	res, err := r.ApplyFieldPlan(context.Background(), "clone1", plan)
	if err != nil {
		t.Fatalf("ApplyFieldPlan: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty (verify gate)", res.Applied)
	}
	if len(res.Failed) != len(testPlan()) {
		t.Fatalf("failed = %+v, want every change", res.Failed)
	}
	for _, f := range res.Failed {
		if !strings.Contains(f.Err, "verify") {
			t.Fatalf("failure must name the verify gate: %+v", f)
		}
	}
}

func TestApplyFieldPlanNotClone(t *testing.T) {
	db := testStore(t) // no lineage row
	factory := &fakeBrowserFactory{}
	r := editorTestRunner(t, editorCfg("https://hh.ru"), db, newFakeEditor(t), factory, nil)
	_, err := r.ApplyFieldPlan(context.Background(), "unknown", testPlan())
	if !errors.Is(err, ErrNotClone) {
		t.Fatalf("err = %v, want ErrNotClone", err)
	}
	if factory.browser != nil {
		t.Fatal("no browser may start for a non-clone target")
	}
}

func TestApplyFieldPlanCookieFailureDegrades(t *testing.T) {
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	cookies := &fakeCookieSource{err: errors.New("keyring locked")}
	factory := &fakeBrowserFactory{}
	r := editorTestRunner(t, editorCfg("https://hh.ru"), db, newFakeEditor(t), factory, cookies)
	_, err := r.ApplyFieldPlan(context.Background(), "clone1", testPlan())
	if !errors.Is(err, ErrEditorUnavailable) {
		t.Fatalf("err = %v, want ErrEditorUnavailable", err)
	}
	if factory.browser != nil {
		t.Fatal("no browser may start without cookies")
	}
}

func TestApplyFieldPlanBrowserStartFailureDegrades(t *testing.T) {
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	factory := &fakeBrowserFactory{startErr: errors.New("chrome: executable file not found")}
	r := editorTestRunner(t, editorCfg("https://hh.ru"), db, newFakeEditor(t), factory, nil)
	_, err := r.ApplyFieldPlan(context.Background(), "clone1", testPlan())
	if !errors.Is(err, ErrEditorUnavailable) {
		t.Fatalf("err = %v, want ErrEditorUnavailable", err)
	}
}

func TestNewChromeEditorRunnerValidation(t *testing.T) {
	if _, err := NewChromeEditorRunner(EditorOptions{Log: testLoggerT()}); err == nil {
		t.Fatal("runner without db/cookies/verify must be rejected")
	}
}

// --- mock editor page -------------------------------------------------------

// mockEditorServer serves the data-qa editor fixture per wizard page and
// fails any path outside the wizard namespace (the runner must not
// wander). The URL shape mirrors the live editor: /resume/edit/{resume}/{segment}.
func mockEditorServer(t *testing.T, st *fakeEditor) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/resume/edit/clone1/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.Error(w, "outside the wizard", http.StatusNotFound)
			return
		}
		segment := strings.TrimPrefix(r.URL.Path, prefix)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(mockEditorHTML(segment, &st.state)))
	})
	return httptest.NewServer(mux)
}

func mockEditorHTML(segment string, st *contentState) string {
	save := `<button data-qa="resume-partial-edit-save" type="button">Сохранить</button>`
	switch segment {
	case "position":
		return `<html><body><textarea data-qa="resume-edit-title-suggest">` + plainString(st, "title") + `</textarea>` +
			`<input data-qa="resume-salary-amount" type="text">` + save + `</body></html>`
	case "key_skills":
		return `<html><body><input data-qa="key_skills" name="key_skills">` + save + `</body></html>`
	case "experience/0":
		return `<html><body>` +
			`<textarea data-qa="resume-editor-experience-description-input"></textarea>` +
			save + `</body></html>`
	case "experience/1":
		return `<html><body>` +
			`<input data-qa="resume-editor-experience-start-year-input" type="text">` +
			`<input data-qa="resume-editor-experience-end-year-input" type="text">` +
			save + `</body></html>`
	default:
		return "<html><body>" + save + "</body></html>"
	}
}

func plainString(st *contentState, key string) string {
	var parts []map[string]string
	if err := json.Unmarshal(st.Resume[key], &parts); err != nil || len(parts) == 0 {
		return ""
	}
	return parts[0]["string"]
}

// --- mock page helpers (fixture ↔ selector contract) ------------------------

func dataQAOf(selector string) string {
	const marker = `[data-qa="`
	i := strings.Index(selector, marker)
	if i < 0 {
		return ""
	}
	rest := selector[i+len(marker):]
	if j := strings.IndexByte(rest, '"'); j >= 0 {
		return rest[:j]
	}
	return ""
}

func fetchPage(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("fetch %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		t.Fatalf("read page: %v", err)
	}
	return buf.String()
}

// htmlHasDataQA walks the parsed page looking for a node carrying the
// data-qa attribute value (the same anchor the fill JS resolves).
func htmlHasDataQA(t *testing.T, page, value string) bool {
	t.Helper()
	if value == "" {
		return false
	}
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if found {
			return
		}
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == "data-qa" && a.Val == value {
					found = true
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// --- improver integration (the EditorRunner seam in the T29 pipeline) -------

// fakeEditorRunner is the scripted EditorRunner the improver receives
// (fake Filler pattern: the tests pin the plan→runner contract).
type fakeEditorRunner struct {
	mu     sync.Mutex
	clones []string
	plans  [][]FieldChange
	result ApplyResult
	err    error
}

func (f *fakeEditorRunner) ApplyFieldPlan(_ context.Context, cloneHash string, plan []FieldChange) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clones = append(f.clones, cloneHash)
	f.plans = append(f.plans, plan)
	return f.result, f.err
}

func newEditorImprover(t *testing.T, ed *fakeEditor, run ai.FieldPromptRunner, db *store.DB, rev review.Reviewer, er EditorRunner, fields ...string) *Improver {
	t.Helper()
	cfg := baseImproveCfg()
	cfg.Fields = fields
	if len(cfg.Fields) == 0 {
		cfg.Fields = []string{"title"}
	}
	reg, err := NewPromptRegistry(cfg)
	if err != nil {
		t.Fatalf("NewPromptRegistry: %v", err)
	}
	im, err := New(Options{
		API: ed, Runner: run, DB: db, Review: rev, Registry: reg,
		Cfg: cfg, Editor: er, Log: testLoggerT(),
		Now: func() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return im
}

func twoFieldRunner(t *testing.T) *fakeImproveRunner {
	t.Helper()
	return &fakeImproveRunner{verdicts: map[string]func() string{
		"title": improveVerdictFor(t, map[string]any{"value_string": "Новый заголовок"}),
		"about": improveVerdictFor(t, map[string]any{"value_string": "Улучшенное описание"}),
	}}
}

// TestImproveCloneEditorApplies: with the runner set, the plan rides the
// browser editor and the builder-update API is never called.
func TestImproveCloneEditorApplies(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	er := &fakeEditorRunner{result: ApplyResult{Applied: []string{"resume.title", "resume.skills"}}}
	im := newEditorImprover(t, ed, twoFieldRunner(t), db, &fakeReviewer{}, er, "title", "about")

	artifact, err := im.ImproveClone(context.Background(), "acc1", "parent1", "clone1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() != 0 {
		t.Fatal("the builder-update API must not be called when the editor runner is set")
	}
	if len(er.plans) != 1 || er.clones[0] != "clone1" {
		t.Fatalf("runner calls = %d (clone %v), want one plan for clone1", len(er.plans), er.clones)
	}
	plan := er.plans[0]
	if len(plan) != 2 {
		t.Fatalf("plan holds %d changes, want 2: %+v", len(plan), plan)
	}
	byLocator := planByLocator(plan)
	title := byLocator["resume.title"]
	if title.Field != "title" || title.Screen != "title" || title.Kind != KindText || title.Item != -1 {
		t.Fatalf("title change wrong: %+v", title)
	}
	if string(title.Value) != `"Новый заголовок"` {
		t.Fatalf("title natural value = %s, want the plain string", title.Value)
	}
	about := byLocator["resume.skills"]
	if about.Screen != "about" {
		t.Fatalf("about screen = %q, want about", about.Screen)
	}
	for _, loc := range []string{"resume.title", "resume.skills"} {
		if d := fieldDiffByLocator(t, artifact, loc); d.Status != StatusImproved {
			t.Fatalf("%s status = %s (%s), want improved", loc, d.Status, d.Reason)
		}
	}
	if !artifact.Applied {
		t.Fatal("artifact must report applied")
	}
}

// TestImproveCloneEditorDegradation: an unavailable browser parks the plan
// for manual apply (review), flips the planned fields to error and never
// touches the builder-update API.
func TestImproveCloneEditorDegradation(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	rev := &fakeReviewer{}
	er := &fakeEditorRunner{err: fmt.Errorf("runner: %w", ErrEditorUnavailable)}
	im := newEditorImprover(t, ed, twoFieldRunner(t), db, rev, er, "title", "about")

	artifact, err := im.ImproveClone(context.Background(), "acc1", "parent1", "clone1")
	if err != nil {
		t.Fatalf("a degradation must not error the improve flow: %v", err)
	}
	if ed.updateCount() != 0 {
		t.Fatal("the builder-update API must stay untouched on degradation")
	}
	for _, loc := range []string{"resume.title", "resume.skills"} {
		d := fieldDiffByLocator(t, artifact, loc)
		if d.Status != StatusError {
			t.Fatalf("%s status = %s, want error (parked)", loc, d.Status)
		}
	}
	if artifact.Applied {
		t.Fatal("artifact must not report applied")
	}
	if len(rev.kinds) == 0 || rev.kinds[0] != ReviewKindImproveNeedsHuman {
		t.Fatalf("review kinds = %v, want the plan parked for manual apply", rev.kinds)
	}
}

// TestImproveCloneEditorFieldFailure: per-change failures land as
// StatusError on their own fields only.
func TestImproveCloneEditorFieldFailure(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	er := &fakeEditorRunner{result: ApplyResult{
		Applied: []string{"resume.title"},
		Failed:  []FieldApplyError{{Field: "resume.skills", Err: "not_found"}},
	}}
	im := newEditorImprover(t, ed, twoFieldRunner(t), db, &fakeReviewer{}, er, "title", "about")

	artifact, err := im.ImproveClone(context.Background(), "acc1", "parent1", "clone1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if d := fieldDiffByLocator(t, artifact, "resume.title"); d.Status != StatusImproved {
		t.Fatalf("title status = %s, want improved", d.Status)
	}
	if d := fieldDiffByLocator(t, artifact, "resume.skills"); d.Status != StatusError || d.Reason != "not_found" {
		t.Fatalf("about status = %s (%s), want error/not_found", d.Status, d.Reason)
	}
}

// TestImproveCloneEditorInvariantError: a non-degradation runner failure
// (lineage invariant, read failure) propagates — T29 semantics.
func TestImproveCloneEditorInvariantError(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	er := &fakeEditorRunner{err: fmt.Errorf("runner: %w", ErrNotClone)}
	im := newEditorImprover(t, ed, twoFieldRunner(t), db, &fakeReviewer{}, er, "title", "about")

	if _, err := im.ImproveClone(context.Background(), "acc1", "parent1", "clone1"); !errors.Is(err, ErrNotClone) {
		t.Fatalf("err = %v, want the invariant error propagated", err)
	}
}

// TestImproveCloneNilEditorRidesAPI: without the runner the T29
// builder-update path stays exactly as shipped.
func TestImproveCloneNilEditorRidesAPI(t *testing.T) {
	ed := newFakeEditor(t)
	db := testStore(t)
	seedClone(t, db, "parent1", "clone1")
	im := newEditorImprover(t, ed, twoFieldRunner(t), db, &fakeReviewer{}, nil, "title", "about")

	artifact, err := im.ImproveClone(context.Background(), "acc1", "parent1", "clone1")
	if err != nil {
		t.Fatalf("ImproveClone: %v", err)
	}
	if ed.updateCount() == 0 {
		t.Fatal("nil editor must ride the builder-update API")
	}
	if d := fieldDiffByLocator(t, artifact, "resume.title"); d.Status != StatusImproved {
		t.Fatalf("title status = %s (%s), want improved via API", d.Status, d.Reason)
	}
}
