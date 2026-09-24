package e2e

// Fake hh.ru: the full scenario surface the e2e flows walk through. This
// extends the wiring-smoke fake from internal/cli helpers_test.go to the
// complete happy/skip path: settings SSR (xsrf), 2 published resumes, 5
// suitable vacancies (clean / exclude-word / low rating / duplicate /
// captcha-on-apply), apply popup + apply (201), negotiations (reconcile
// source), employer rating widgets, vacancy/employer cards and the chatik
// surface (chats list, chat history, send, participant_action).
//
// All served documents are deterministic; only the observed interaction
// counters (apply posts, sends, leaves, request totals) mutate, and every
// mutation is mutex-guarded so the tests can poll them while the engine
// runs. Timestamps are relative to server construction (now-…), which is
// what drives the chatops silence and warmup ladder timing.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Scenario fixtures (shared by every e2e scenario; per-test variation goes
// through the config, not through different server data).
const (
	acctID   = "main"
	resumeID = "res-h1"
	runID    = "e2e"

	vacClean     = int64(101) // passes the whole cascade → applied
	vacExclude   = int64(102) // F2 exclude word in the name
	vacLowRating = int64(103) // F4 employer rating below min
	vacDup       = int64(104) // F5 live application row seeded by the test
	vacCaptcha   = int64(105) // F8 apply answers with a captcha challenge

	employerGood = int64(900) // rating 4.8 (vacClean, vacDup, chats)
	employerBad  = int64(901) // rating 2.3 (vacLowRating)
	employerMid  = int64(902) // rating 4.5 (vacCaptcha)

	chatQuestion  = "300201" // inbound question → chat_reply → send
	chatRejection = "300204" // structural refusal (negotiations refused)
	chatTest      = "300203" // test assignment → answers → send
	chatWarmup    = "300205" // silence ping target (E2E-4)

	chatParticipantSelf = "me"
	chatParticipantHR   = "emp"

	questionMessageID  = "m-q-1"
	rejectionMessageID = "m-r-1"
	testMessageID      = "m-t-1"

	questionText  = "Здравствуйте! Подскажите, пожалуйста, вакансия ещё актуальна?"
	rejectionText = "К сожалению, мы приняли решение продолжить с другим кандидатом."
	testText      = "Тестовое задание:\n1. Расскажите о вашем опыте с Go?\n2. Какой у вас опыт с PostgreSQL?"
	testQuestion1 = "Расскажите о вашем опыте с Go?"
	testQuestion2 = "Какой у вас опыт с PostgreSQL?"

	// e2eLetter is the fixed cover_letter verdict text: no digits (the
	// runner's number-leak post-check), no links, no contact handles.
	e2eLetter = "Здравствуйте! Готов обсудить вашу задачу: релевантный опыт есть, стек совпадает. Буду рад созвониться."
	// e2eReply is the fixed chat_reply draft (auto-send threshold cleared).
	e2eReply = "Здравствуйте! Да, вакансия актуальна — готов обсудить детали и назначить созвон."
	// e2ePing is the first entry of the warmup ping list.
	e2ePing = "Добрый день! Подскажите, пожалуйста, статус рассмотрения моего отклика."

	// xsrfToken is what the settings SSR page hands out; the fake asserts
	// that mutations carry it back.
	xsrfToken = "e2e-xsrf-token"
)

// applyPost is one recorded POST /applicant/vacancy_response/popup.
type applyPost struct {
	VacancyID  int64
	ResumeHash string
	Letter     string
	XSRF       string
}

// sendPost is one recorded POST /chatik/api/send.
type sendPost struct {
	ChatID         string
	IdempotencyKey string
	Text           string
}

// fakeHH is the fake hh.ru + chatik server.
type fakeHH struct {
	srv *httptest.Server
	now time.Time

	mu              sync.Mutex
	applyPosts      []applyPost
	sends           []sendPost
	leaves          []string
	negotiations    int // ok-applies that appended a negotiation row
	touches         int
	mutationPosts   int // apply + send + touch + leave POSTs (hh mutations)
	totalRequests   int
	chatDataLookups map[string]int
	popupHits       int
	applyTries      map[int64]int

	// onMutation is the kill-switch test seam: called (under the fake's
	// lock) on every mutation POST, before the mutation settles.
	onMutation func()

	// extraInbound carries messages injected mid-scenario (the dry-run →
	// real seam: the real pass needs fresh inbound work to classify).
	extraInbound map[string][]any
}

// appendInbound makes the fake serve one more inbound message in chatID.
func (f *fakeHH) appendInbound(chatID, id, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.extraInbound == nil {
		f.extraInbound = map[string][]any{}
	}
	f.extraInbound[chatID] = append(f.extraInbound[chatID], f.msgItem(id, chatParticipantHR, text, time.Minute))
}

func newFakeHH(t *testing.T) *fakeHH {
	t.Helper()
	f := &fakeHH{
		now:             time.Now(),
		chatDataLookups: map[string]int{},
		applyTries:      map[int64]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /applicant/settings", f.serveSettings)
	mux.HandleFunc("GET /shards/applicant/resumes", f.serveResumes)
	mux.HandleFunc("GET /applicant/resumes", f.serveSuitable)
	mux.HandleFunc("GET /applicant/vacancy_response/popup", f.servePopup)
	mux.HandleFunc("POST /applicant/vacancy_response/popup", f.serveApply)
	mux.HandleFunc("GET /applicant/negotiations", f.serveNegotiations)
	mux.HandleFunc("POST /applicant/resumes/touch", f.serveTouch)
	mux.HandleFunc("GET /vacancy/", f.serveVacancyCard)
	mux.HandleFunc("GET /employer/", f.serveEmployerCard)
	mux.HandleFunc("GET /employer_reviews/proxy_components/small_widget", f.serveRatingWidget)
	mux.HandleFunc("GET /search/vacancy", f.serveEmptySearch)
	mux.HandleFunc("GET /chatik/api/chats", f.serveChats)
	mux.HandleFunc("GET /chatik/api/chat_data", f.serveChatData)
	mux.HandleFunc("POST /chatik/api/send", f.serveSend)
	mux.HandleFunc("POST /chatik/api/participant_action", f.serveLeave)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHH) URL() string { return f.srv.URL }

func (f *fakeHH) serveJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// ssrPage renders an SSR page with the HH-Lux-InitialState template carrier
// (the shape internal/hhapi initialState() parses).
func ssrPage(state any) []byte {
	raw, _ := json.Marshal(state)
	return []byte(`<!doctype html><html><body><template id="HH-Lux-InitialState" data-name="HH-Lux-InitialState">` +
		string(raw) + `</template></body></html>`)
}

func (f *fakeHH) track(_ http.ResponseWriter, _ *http.Request, mutation bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.totalRequests++
	if mutation {
		f.mutationPosts++
		if f.onMutation != nil {
			f.onMutation() // test seam: fires on every mutation POST
		}
	}
}

// --- hh.ru: reads ---

func (f *fakeHH) serveSettings(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(ssrPage(map[string]any{
		"xsrfToken": xsrfToken,
		"session":   map[string]any{"userId": 777, "email": "e2e@example.org", "fullName": "E2E Кандидат"},
	}))
}

func (f *fakeHH) resumeItem(hash, title string, age time.Duration) map[string]any {
	return map[string]any{
		"hash":      hash,
		"title":     title,
		"updatedAt": f.now.Add(-age).Format(time.RFC3339),
		"status":    map[string]any{"id": "published", "name": "Опубликовано"},
		"_attributes": map[string]any{
			"isSearchable": true,
			"percent":      100,
			"status":       map[string]any{"id": "published", "name": "Опубликовано"},
		},
		"skill_set": []string{"Go", "PostgreSQL", "Docker"},
		"area":      map[string]any{"name": "Москва"},
	}
}

func (f *fakeHH) serveResumes(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	f.serveJSON(w, map[string]any{
		"resumes": []any{
			f.resumeItem(resumeID, "Go-разработчик", time.Hour),
			f.resumeItem("res-h2", "QA-инженер", 2*time.Hour),
		},
	})
}

func (f *fakeHH) vacItem(id int64, name string, employerID int64, employerName string) map[string]any {
	return map[string]any{
		"vacancyId":     id,
		"name":          name,
		"employer":      map[string]any{"id": employerID, "name": employerName},
		"area":          map[string]any{"name": "Москва"},
		"publishedAt":   f.now.Add(-24 * time.Hour).Format(time.RFC3339),
		"description":   "Продуктовая команда, современный стек.",
		"alternate_url": f.srv.URL + "/vacancy/" + strconv.FormatInt(id, 10),
		"compensation":  map[string]any{"from": 250000, "currencyCode": "RUR"},
	}
}

func (f *fakeHH) suitableVacancies() []any {
	return []any{
		f.vacItem(vacClean, "Senior Go-разработчик", employerGood, "Хорошие Решения"),
		f.vacItem(vacExclude, "Менеджер по продажам (входящие заявки)", employerGood, "Хорошие Решения"),
		f.vacItem(vacLowRating, "Go-разработчик (жёсткий офис)", employerBad, "Плохой Работодатель"),
		f.vacItem(vacDup, "Go-разработчик (дубль)", employerGood, "Хорошие Решения"),
		f.vacItem(vacCaptcha, "Go backend-разработчик", employerMid, "Средний Работодатель"),
	}
}

func (f *fakeHH) serveSuitable(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(ssrPage(map[string]any{
		"applicantSuitableVacancyByResume": map[string]any{
			resumeID: map[string]any{
				"vacancies":    f.suitableVacancies(),
				"searchCounts": map[string]any{"value": 5},
			},
			"res-h2": map[string]any{
				"vacancies":    []any{},
				"searchCounts": map[string]any{"value": 0},
			},
		},
	}))
}

func (f *fakeHH) servePopup(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	id, _ := strconv.ParseInt(r.URL.Query().Get("vacancyId"), 10, 64)
	f.mu.Lock()
	f.popupHits++
	f.mu.Unlock()
	letterRequired := id == vacClean // the clean vacancy demands a cover letter
	f.serveJSON(w, map[string]any{
		"test":                      map[string]any{"hasTests": false},
		"letterRequired":            letterRequired,
		"acceptAutoResponse":        true,
		"chatWritePossibility":      "ALLOWED",
		"resumeInconsistencies":     nil,
		"negotiationsLimitExceeded": false,
	})
}

func (f *fakeHH) chatItems() []any {
	item := func(id string, vacancyID int64, age time.Duration) map[string]any {
		return map[string]any{
			"id":          id,
			"vacancyId":   vacancyID,
			"unreadCount": 1,
			"lastMessage": map[string]any{"createDateTime": f.now.Add(-age).Format(time.RFC3339)},
		}
	}
	return []any{
		item(chatQuestion, 201, 2*time.Minute),
		item(chatRejection, 204, 3*time.Minute),
		item(chatTest, 203, 4*time.Minute),
		item(chatWarmup, 205, 10*time.Hour),
	}
}

func (f *fakeHH) serveChats(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	items := []any{}
	if r.URL.Query().Get("page") == "" || r.URL.Query().Get("page") == "1" {
		items = f.chatItems()
	}
	info := map[string]any{}
	for _, it := range f.chatItems() {
		id := it.(map[string]any)["id"].(string)
		info[id] = map[string]any{"title": "Хорошие Решения"}
	}
	f.serveJSON(w, map[string]any{"chats": map[string]any{"items": items}, "chatsDisplayInfo": info})
}

func (f *fakeHH) msgItem(id, from, text string, age time.Duration) map[string]any {
	return map[string]any{
		"id":             id,
		"type":           "SIMPLE",
		"participantId":  from,
		"text":           text,
		"createDateTime": f.now.Add(-age).Format(time.RFC3339),
		"participantDisplay": map[string]any{
			"name": map[bool]string{true: "E2E Кандидат", false: "HR Хороших Решений"}[from == chatParticipantSelf],
		},
	}
}

func (f *fakeHH) chatHistory(chatID string) []any {
	var extra []any
	if f.extraInbound != nil {
		extra = f.extraInbound[chatID]
	}
	switch chatID {
	case chatQuestion:
		return append(extra, f.msgItem(questionMessageID, chatParticipantHR, questionText, 2*time.Minute))
	case chatRejection:
		return append(extra, f.msgItem(rejectionMessageID, chatParticipantHR, rejectionText, 3*time.Minute))
	case chatTest:
		return append(extra, f.msgItem(testMessageID, chatParticipantHR, testText, 4*time.Minute))
	case chatWarmup:
		// Inbound first, then OUR outgoing message 10h ago: this is what
		// makes the chat silence-expired for the warmup ladder.
		return []any{
			f.msgItem("m-w-1", chatParticipantHR, "Здравствуйте! Ваш отклик получен.", 11*time.Hour),
			f.msgItem("m-w-2", chatParticipantSelf, "Спасибо! Жду обратной связи по статусу.", 10*time.Hour),
		}
	default:
		return []any{}
	}
}

func (f *fakeHH) serveChatData(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	chatID := r.URL.Query().Get("chatId")
	f.mu.Lock()
	f.chatDataLookups[chatID]++
	f.mu.Unlock()
	f.serveJSON(w, map[string]any{
		"chat": map[string]any{
			"currentParticipantId": chatParticipantSelf,
			"messages":             map[string]any{"items": f.chatHistory(chatID)},
		},
	})
}

func (f *fakeHH) negotiationsTopicList() []any {
	topic := func(id string, vacancyID int64, state string) map[string]any {
		return map[string]any{
			"id":           id,
			"vacancyId":    vacancyID,
			"vacancyName":  "Go-разработчик",
			"employer":     map[string]any{"id": employerGood, "name": "Хорошие Решения"},
			"employerName": "Хорошие Решения",
			"lastState":    state,
			"updatedAt":    f.now.Add(-time.Hour).Format(time.RFC3339),
		}
	}
	return []any{topic("neg-204", 204, "refused")}
}

func (f *fakeHH) serveNegotiations(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	items := []any{}
	if r.URL.Query().Get("page") == "" || r.URL.Query().Get("page") == "1" {
		items = f.negotiationsTopicList()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(ssrPage(map[string]any{"applicantNegotiations": map[string]any{"topicList": items}}))
}

func (f *fakeHH) serveVacancyCard(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/vacancy/"), 10, 64)
	names := map[int64]string{
		201: "Go-разработчик (чат)",
		203: "Go-разработчик (тест)",
		204: "Go-разработчик (отказ)",
		205: "Go-разработчик (подогрев)",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(ssrPage(map[string]any{"vacancyView": map[string]any{
		"vacancyId": id,
		"name":      names[id],
		"employer":  map[string]any{"id": employerGood, "name": "Хорошие Решения"},
		"area":      map[string]any{"name": "Москва"},
	}}))
}

func (f *fakeHH) serveEmployerCard(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/employer/"), 10, 64)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(ssrPage(map[string]any{"employerInfo": map[string]any{
		"employerId": id,
		"name":       "Хорошие Решения",
		"site":       "",
		"contacts":   []any{},
	}}))
}

func (f *fakeHH) serveRatingWidget(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	id, _ := strconv.ParseInt(r.URL.Query().Get("employerId"), 10, 64)
	ratings := map[int64][3]any{
		employerGood: {"4.8", 21, 87},
		employerBad:  {"2.3", 14, 40},
		employerMid:  {"4.5", 9, 80},
	}
	row := [3]any{"4.0", 5, 75}
	if v, ok := ratings[id]; ok {
		row = v
	}
	// totalRating must be a STRING: the client treats a missing string
	// totalRating as "no reviews widget" (ErrNotFound).
	f.serveJSON(w, map[string]any{
		"employerReviews": map[string]any{
			"totalRating":            row[0],
			"reviewsCount":           row[1],
			"recommendationsPercent": row[2],
			"last3MonthsReviewsInfo": map[string]any{"totalRating": row[0]},
		},
	})
}

func (f *fakeHH) serveEmptySearch(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, false)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(ssrPage(map[string]any{
		"vacancySearchResult": map[string]any{"vacancies": []any{}},
		"searchCounts":        map[string]any{"value": 0},
	}))
}

// --- hh.ru / chatik: mutations ---

func (f *fakeHH) serveApply(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, true)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	vacID, _ := strconv.ParseInt(r.FormValue("vacancy_id"), 10, 64)
	post := applyPost{
		VacancyID:  vacID,
		ResumeHash: r.FormValue("resume_hash"),
		Letter:     r.FormValue("letter"),
		XSRF:       r.Header.Get("X-XSRFToken"),
	}
	f.mu.Lock()
	f.applyPosts = append(f.applyPosts, post)
	f.applyTries[vacID]++
	tries := f.applyTries[vacID]
	f.mu.Unlock()

	if vacID == vacCaptcha {
		// hh in-house challenge served with 200: the client classifies the
		// body marker into ErrCaptcha (no side effect, no negotiation).
		f.serveJSON(w, map[string]any{"hhcaptcha": map[string]any{"isBot": true}})
		return
	}
	// Successful apply: 201 + negotiation topic created server-side.
	f.mu.Lock()
	f.negotiations++
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	f.serveJSON(w, map[string]any{
		"success":  true,
		"topic_id": "neg-" + strconv.FormatInt(vacID, 10) + "-" + strconv.Itoa(tries),
	})
}

func (f *fakeHH) serveTouch(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, true)
	f.mu.Lock()
	f.touches++
	f.mu.Unlock()
	f.serveJSON(w, map[string]any{"ok": true})
}

func (f *fakeHH) serveSend(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, true)
	var body struct {
		ChatID         int64  `json:"chatId"`
		IdempotencyKey string `json:"idempotencyKey"`
		Text           string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.sends = append(f.sends, sendPost{
		ChatID:         strconv.FormatInt(body.ChatID, 10),
		IdempotencyKey: body.IdempotencyKey,
		Text:           body.Text,
	})
	f.mu.Unlock()
	f.serveJSON(w, map[string]any{"ok": true})
}

func (f *fakeHH) serveLeave(w http.ResponseWriter, r *http.Request) {
	f.track(w, r, true)
	var body struct {
		ChatID     int64  `json:"chatId"`
		ActionType string `json:"actionType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.leaves = append(f.leaves, strconv.FormatInt(body.ChatID, 10))
	f.mu.Unlock()
	f.serveJSON(w, map[string]any{"ok": true})
}

// --- observed-state accessors (poll-safe) ---

func (f *fakeHH) applyPostsSnapshot() []applyPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]applyPost(nil), f.applyPosts...)
}

func (f *fakeHH) sendsSnapshot() []sendPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sendPost(nil), f.sends...)
}

func (f *fakeHH) sendCount(chatID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sends {
		if s.ChatID == chatID {
			n++
		}
	}
	return n
}

func (f *fakeHH) applyTryCount(vacancyID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyTries[vacancyID]
}

func (f *fakeHH) chatDataHits(chatID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chatDataLookups[chatID]
}

func (f *fakeHH) stats() (mutations, total, leaves, negotiationAppends int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutationPosts, f.totalRequests, len(f.leaves), f.negotiations
}
