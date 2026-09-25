package multiapply

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/captcha"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/queue"
	"hhauto/internal/scheduler"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// --- stateful fake hh server wired through a real hhapi client ---

type applyEnv struct {
	t   *testing.T
	srv *httptest.Server
	api hhapi.HHAPI
	db  *store.DB

	mu          sync.Mutex
	suitable    []string // vacancy item JSON blocks (resume "hash1")
	searchItems []string // vacancy item JSON blocks per page (same on every page)
	searchFound int      // searchCounts.value (drives Pages)
	searchReqs  int
	// searchQueryLog records the raw query of every search-page request
	// (T25: pins the extended-filter wiring end-to-end).
	searchQueryLog []string
	popupByVac     map[int64]string
	popupErr       map[int64]int // vacancy → HTTP status
	applyErr       func(call int, vacancy int64, letter string) error
	applyCalls     []int64 // order of applied vacancy ids
	applyBodies    []string
	negPages       [][]hhapi.Negotiation
	negFail        bool
	negScript      []negAct // per-read script (1st read → negScript[0], …); empty → legacy fields
	negReads       int
	ratings        map[int64]*hhapi.EmployerRating
	ratingErr      map[int64]error
	ratingReqs     map[int64]int
	popupReqs      map[int64]int

	// T26 autoclone surface: mutable mine shard + scripted clone endpoint.
	resumeShard   []string                                 // resume JSON blocks served by GET /shards/applicant/resumes
	cloneNext     int                                      // issued clone serial (clone1, clone2, …)
	clonePosts    int                                      // total clone POST attempts (success or not)
	cloneStatus   string                                   // status stamped on auto-sharded clones (default published)
	cloneAuto     bool                                     // auto-append created clones to resumeShard (default true)
	cloneSawToken bool                                     // at least one clone POST carried X-XSRFToken
	cloneFail     func(call int) (status int, body string) // scripted clone failure
}

func newApplyEnv(t *testing.T) *applyEnv {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "apply.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &applyEnv{
		t:           t,
		db:          db,
		popupByVac:  map[int64]string{},
		popupErr:    map[int64]int{},
		ratings:     map[int64]*hhapi.EmployerRating{},
		ratingErr:   map[int64]error{},
		ratingReqs:  map[int64]int{},
		popupReqs:   map[int64]int{},
		searchFound: 1,
		cloneAuto:   true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /applicant/settings", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_test", Path: "/"})
		_, _ = io.WriteString(w, xsrfPage)
	})
	mux.HandleFunc("GET /shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "tok_test", Path: "/"})
		e.mu.Lock()
		items := append([]string(nil), e.resumeShard...)
		e.mu.Unlock()
		_, _ = io.WriteString(w, `{"resumes":[`+strings.Join(items, ",")+`]}`)
	})
	mux.HandleFunc("POST /applicant/resumes/clone", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"resumeId"`) {
			t.Errorf("clone request body %q carries no resumeId", body)
		}
		// hh rejects a tokenless mutation with a 403 xsrf signature; the
		// client must refresh once and retry with X-XSRFToken.
		if r.Header.Get("X-XSRFToken") == "" {
			http.Error(w, `{"error":"xsrf token required"}`, http.StatusForbidden)
			return
		}
		e.mu.Lock()
		e.cloneSawToken = true
		e.clonePosts++
		call := e.cloneNext + 1
		fn := e.cloneFail
		e.mu.Unlock()
		if fn != nil {
			if status, b := fn(call); status != 0 {
				http.Error(w, b, status)
				return
			}
		}
		e.mu.Lock()
		e.cloneNext = call
		hash := fmt.Sprintf("clone%d", call)
		status := e.cloneStatus
		if status == "" {
			status = "published"
		}
		if e.cloneAuto {
			e.resumeShard = append(e.resumeShard, fmt.Sprintf(
				`{"_attributes":{"hash":%q,"status":%q,"isSearchable":true,"updated":1758700000000}}`, hash, status))
		}
		e.mu.Unlock()
		_, _ = io.WriteString(w, fmt.Sprintf(`{"url":"/profile/resume/?resume=%s"}`, hash))
	})
	mux.HandleFunc("GET /applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		items := append([]string(nil), e.suitable...)
		e.mu.Unlock()
		_, _ = io.WriteString(w, initialStatePage(fmt.Sprintf(
			`{"applicantSuitableVacancyByResume":{"hash1":{"vacancies":[%s],"searchCounts":{"value":%d}}}}`,
			strings.Join(items, ","), len(items))))
	})
	mux.HandleFunc("GET /search/vacancy", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.searchReqs++
		e.searchQueryLog = append(e.searchQueryLog, r.URL.RawQuery)
		items := append([]string(nil), e.searchItems...)
		found := e.searchFound
		e.mu.Unlock()
		_, _ = io.WriteString(w, initialStatePage(fmt.Sprintf(
			`{"vacancySearchResult":{"vacancies":[%s]},"searchCounts":{"value":%d}}`,
			strings.Join(items, ","), found)))
	})
	mux.HandleFunc("GET /applicant/vacancy_response/popup", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("vacancyId"), 10, 64)
		e.mu.Lock()
		e.popupReqs[id]++
		body, has := e.popupByVac[id]
		status := e.popupErr[id]
		e.mu.Unlock()
		if status != 0 {
			http.Error(w, "popup fail", status)
			return
		}
		if !has {
			body = `{"type":"POPUP","letterRequired":false,"test":{"hasTests":false},"acceptAutoResponse":true,"chatWritePossibility":"ENABLED"}`
		}
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("POST /applicant/vacancy_response/popup", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := parseForm(string(body))
		vid, _ := strconv.ParseInt(form["vacancy_id"], 10, 64)
		e.mu.Lock()
		call := len(e.applyCalls) + 1
		e.applyCalls = append(e.applyCalls, vid)
		e.applyBodies = append(e.applyBodies, string(body))
		fn := e.applyErr
		e.mu.Unlock()
		if fn != nil {
			if err := fn(call, vid, form["letter"]); err != nil {
				if te, ok := err.(*respErr); ok {
					if te.redirect != "" {
						http.Redirect(w, r, te.redirect, http.StatusFound)
						return
					}
					http.Error(w, te.body, te.status)
					return
				}
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"success":true,"topic_id":%d}`, 555000+vid))
	})
	mux.HandleFunc("GET /applicant/negotiations", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		read := e.negReads + 1
		e.negReads = read
		script := append([]negAct(nil), e.negScript...)
		pages := append([][]hhapi.Negotiation(nil), e.negPages...)
		fail := e.negFail
		e.mu.Unlock()
		if read <= len(script) {
			if script[read-1].fail {
				http.Error(w, "negotiations down", http.StatusInternalServerError)
				return
			}
			pages = [][]hhapi.Negotiation{script[read-1].pages}
			fail = false
		}
		if fail {
			http.Error(w, "negotiations down", http.StatusInternalServerError)
			return
		}
		page := 1
		if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
			page = p
		}
		var items []string
		if page-1 < len(pages) {
			for _, n := range pages[page-1] {
				items = append(items, fmt.Sprintf(
					`{"id":%q,"vacancyId":%d,"vacancyName":%q,"lastState":%q,"updatedAt":"2026-09-24T08:00:00+0300"}`,
					n.ID, n.VacancyID, n.VacancyName, n.State))
			}
		}
		_, _ = io.WriteString(w, initialStatePage(
			`{"applicantNegotiations":{"topicList":[`+strings.Join(items, ",")+`]}}`))
	})
	mux.HandleFunc("GET /employer_reviews/proxy_components/small_widget", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.URL.Query().Get("employerId"), 10, 64)
		e.mu.Lock()
		e.ratingReqs[id]++
		rating := e.ratings[id]
		rerr := e.ratingErr[id]
		e.mu.Unlock()
		if rerr != nil {
			http.Error(w, "rating down", http.StatusInternalServerError)
			return
		}
		if rating == nil {
			http.Error(w, "no rating", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"employerReviews":{"totalRating":%q,"recommendationsPercent":%d,"reviewsCount":%d,`+
				`"last3MonthsReviewsInfo":{"totalRating":%q,"reviewsCount":3}}}`,
			strconv.FormatFloat(rating.ScoreAll, 'f', 1, 64), rating.RecommendPct, rating.ReviewsCount,
			strconv.FormatFloat(rating.ScoreRecent, 'f', 1, 64)))
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	c, err := hhapi.NewClient(
		config.Account{DisplayName: "acc1", UserAgent: "test-ua/1.0", RPS: 50},
		config.APIEndpoints{Site: e.srv.URL, Chatik: e.srv.URL, SearchPage: e.srv.URL + "/search/vacancy"},
		nil, nil,
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	e.api = c
	return e
}

// negAct is one scripted negotiations read: fixed pages or a failure.
type negAct struct {
	pages []hhapi.Negotiation
	fail  bool
}

type respErr struct {
	status   int
	body     string
	redirect string
}

func (e *respErr) Error() string { return fmt.Sprintf("fake status %d: %s", e.status, e.body) }

// parseForm is a tiny urlencoded parser (POST bodies in the fake).
func parseForm(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(s, "&") {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			v, err := urlUnescape(parts[1])
			if err != nil {
				return nil, err
			}
			out[parts[0]] = v
		}
	}
	return out, nil
}

func urlUnescape(s string) (string, error) {
	r := strings.NewReplacer("+", " ", "%2B", "+")
	return urlUnescapeSlow(r.Replace(s)), nil
}

func urlUnescapeSlow(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

const xsrfPage = `<!DOCTYPE html><html><body><template id="HH-Lux-InitialState">` +
	`{&#34;xsrfToken&#34;:&#34;tok_test&#34;,&#34;session&#34;:{&#34;userId&#34;:1}}</template></body></html>`

func initialStatePage(state string) string {
	r := strings.NewReplacer("&", "&amp;", `"`, "&#34;", "<", "&lt;", ">", "&gt;")
	return "<!DOCTYPE html><html><body><template id=\"HH-Lux-InitialState\">" +
		r.Replace(state) + "</template></body></html>"
}

// vacItem builds one search-result vacancy block; extra carries raw JSON
// fields appended verbatim (description, autoResponse, employerManager…).
func vacItem(id int64, name string, empID int64, empName, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"vacancyId":%d,"name":%q,"employer":{"id":%d,"name":%q},`+
		`"area":{"name":"Таганрог"},"publishedAt":"2026-09-20T10:00:00+0300",`+
		`"chatWritePossibility":"ENABLED"%s}`, id, name, empID, empName, extra)
}

// --- test doubles for the injected dependencies ---

type fakeChain struct {
	solves int
	err    error
}

func (c *fakeChain) Solve(_ context.Context, _ captcha.Challenge) (captcha.Solution, error) {
	c.solves++
	if c.err != nil {
		return captcha.Solution{}, c.err
	}
	return captcha.Solution{Token: "tok", ProviderID: "fake"}, nil
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []webhook.Event
}

func (n *fakeNotifier) Emit(ev webhook.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
}

func (n *fakeNotifier) Close(_ context.Context) error { return nil }

func (n *fakeNotifier) has(t string) bool {
	return n.count(t) > 0
}

// ofType returns the events of one type (single-goroutine test access).
func (n *fakeNotifier) ofType(t string) []webhook.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []webhook.Event
	for _, ev := range n.events {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

func (n *fakeNotifier) count(t string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, ev := range n.events {
		if ev.Type == t {
			c++
		}
	}
	return c
}

type fakeAIRunner struct {
	scoring string
	letter  string
	err     error
	calls   []ai.TaskName
	inputs  []string
}

func (r *fakeAIRunner) Run(_ context.Context, _ string, _ string, _ string, _ string,
	task ai.TaskName, input any) (json.RawMessage, ai.Decision, error) {
	r.calls = append(r.calls, task)
	b, _ := json.Marshal(input)
	r.inputs = append(r.inputs, string(b))
	if r.err != nil {
		return nil, ai.Decision{}, r.err
	}
	if task == ai.TaskCoverLetter {
		return json.RawMessage(r.letter), ai.Decision{}, nil
	}
	return json.RawMessage(r.scoring), ai.Decision{}, nil
}

var _ = profile.Profile{} // referenced by Options (compile check in tests)

// --- harness ---

var now = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func baseCfg() config.Config {
	return config.Config{
		General:  config.General{Timezone: "UTC"},
		Accounts: map[string]config.Account{"acc1": {DisplayName: "acc1", UserAgent: "u", RPS: 50, DailyApplyCap: 200}},
		Features: config.Features{MultiApply: config.MultiApply{Enabled: true, AIFilter: true, MinMatchPercent: 60}},
		Runs: map[string]config.Run{"run1": {
			Enabled: true, Account: "acc1", ResumeID: "hash1",
			Query: "go", MaxPerDay: 10,
		}},
	}
}

type harness struct {
	*applyEnv
	cfg     config.Config
	runner  *fakeAIRunner
	chain   *fakeChain
	notify  *fakeNotifier
	profile profile.Profile
	q       queue.Queue
	reg     *scheduler.Registry
	noChain bool // build the feature without a captcha chain
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()
	e := newApplyEnv(t)
	h := &harness{
		applyEnv: e,
		cfg:      baseCfg(),
		runner:   &fakeAIRunner{scoring: scoringVerdict(80, "ok", false, false), letter: letterVerdict("Здравствуйте! Готов обсудить детали.")},
		chain:    &fakeChain{},
		notify:   &fakeNotifier{},
		profile: profile.Profile{
			AccountID:    "acc1",
			ResumeDigest: "Резюме: Go, 5 лет",
			ProfileText:  "Ищу бэкенд-роли",
			Facts:        map[string]string{"target_roles": "Go-разработчик"},
		},
	}
	if mutate != nil {
		mutate(&h.cfg)
	}
	return h
}

func (h *harness) feature(t *testing.T) *Feature {
	t.Helper()
	chainFor := func(string) (captcha.Chain, bool) { return h.chain, true }
	if h.noChain {
		chainFor = nil
	}
	return New(Options{
		Config:   h.cfg,
		DB:       h.db,
		APIFor:   func(string) (hhapi.HHAPI, error) { return h.api, nil },
		ChainFor: chainFor,
		Runner:   h.runner,
		ProfileFor: func(context.Context, string) (profile.Profile, error) {
			return h.profile, nil
		},
		Notifier: h.notify,
		Now:      func() time.Time { return now },
		Log:      quietLogger(),
	})
}

func (h *harness) run(t *testing.T) {
	t.Helper()
	f := h.feature(t)
	h.q, h.reg = queue.New(), scheduler.NewRegistry()
	if err := f.RegisterTasks(context.Background(), h.q, h.reg); err != nil {
		t.Fatalf("RegisterTasks: %v", err)
	}
	if err := f.RunProfile(context.Background(), "run1"); err != nil {
		t.Fatalf("RunProfile: %v", err)
	}
}

func scoringVerdict(score int, band string, needsHuman, injection bool) string {
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.9, "needs_human": needsHuman, "needs_human_reason": nil,
		"injection_suspected": injection, "score": score, "band": band,
		"reasons": []string{"r1"}, "used_facts": []string{}, "missing_data": []string{},
	})
	return string(b)
}

func letterVerdict(letter string) string {
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.9, "needs_human": false, "needs_human_reason": nil,
		"injection_suspected": false, "letter": letter,
		"used_facts": []string{}, "missing_data": []string{},
	})
	return string(b)
}

func popupWith(letterRequired bool, inconsistencies bool, chat string) string {
	inc := ""
	if inconsistencies {
		inc = `,"resumeInconsistencies":{"resume":[{"inconsistencies":{"inconsistency":[{"type":"EXPERIENCE","required":"A","actual":"B"}]}}]}`
	}
	return fmt.Sprintf(`{"type":"POPUP","letterRequired":%t,"test":{"hasTests":false},"acceptAutoResponse":true,"chatWritePossibility":%q%s}`,
		letterRequired, chat, inc)
}
