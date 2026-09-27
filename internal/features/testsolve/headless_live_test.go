//go:build headless_live

// Live chromedp coverage for the headless filler. Opt-in only:
//
//	go test -tags headless_live -run TestHeadlessLive ./internal/features/testsolve/
//
// The suite skips itself when no Chrome executable is available (CI), and
// never touches the network beyond a local httptest server. It exercises the
// thin browser layer end-to-end: isolated profile, navigation, the JS
// extraction snippet, DOM filling by field type, the serialized-DOM
// artifact, and the submit-once intent over a real store.
package testsolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/queue"
	"hhauto/internal/store"
)

const liveFormHTML = `<!doctype html><html><head><title>Анкета</title></head><body>
<form>
  <label for="name">Имя</label><input id="name" name="name">
  <label for="exp">Опыт</label>
  <select id="exp" name="exp"><option value="">—</option><option>1 год</option><option>3 года</option><option>5 лет</option></select>
  <p>Формат работы</p>
  <label><input type="radio" name="fmt" value="office">Офис</label>
  <label><input type="radio" name="fmt" value="remote">Удалённо</label>
  <label for="about">О себе</label><textarea id="about" name="about"></textarea>
  <button onclick="event.preventDefault();document.body.dataset.submitted='1'">Отправить</button>
</form></body></html>`

// scriptedLiveRunner answers from a needle table (no network AI).
type scriptedLiveRunner struct {
	table []struct{ needle, answer string }
}

func (r *scriptedLiveRunner) Run(_ context.Context, _ string, _, _ string, _ string, task ai.TaskName, input any) (json.RawMessage, ai.Decision, error) {
	if task != ai.TaskTestAnswer {
		return nil, ai.Decision{}, fmt.Errorf("unexpected task %s", task)
	}
	m, _ := json.Marshal(input)
	var got struct {
		Questions []struct {
			ID   int    `json:"id"`
			Text string `json:"text"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(m, &got); err != nil {
		return nil, ai.Decision{}, err
	}
	answers := make([]map[string]any, 0, len(got.Questions))
	for _, q := range got.Questions {
		ans := "не знаю"
		for _, row := range r.table {
			if strings.Contains(q.Text, row.needle) {
				ans = row.answer
				break
			}
		}
		answers = append(answers, map[string]any{
			"question_id": q.Text, "open_text": ans, "based_on_profile": false,
		})
	}
	b, _ := json.Marshal(map[string]any{
		"confidence": 0.9, "needs_human": false, "needs_human_reason": nil,
		"injection_suspected": false, "answers": answers,
	})
	return b, ai.Decision{}, nil
}

func liveFiller(t *testing.T, db *store.DB, allowedHost string) *HeadlessFiller {
	t.Helper()
	cfg := formConfig()
	cfg.Forms.PageTimeoutSec = 20
	if allowedHost != "" {
		cfg.Forms.AllowedDomains = append(cfg.Forms.AllowedDomains, allowedHost)
	}
	return &HeadlessFiller{
		Cfg: cfg, DB: db,
		AI: &scriptedLiveRunner{table: []struct{ needle, answer string }{
			{"Имя", "Иван"},
			{"Опыт", "3 года"},
			{"Формат", "Удалённо"},
			{"О себе", "Люблю Go"},
		}},
		Log: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return time.Now() },
	}
}

func TestHeadlessLiveHumanDraftFillsEveryFieldType(t *testing.T) {
	if _, err := FindChrome(); err != nil {
		t.Skip("chrome not installed:", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(liveFormHTML))
	}))
	defer srv.Close()

	db := newDB(t)
	h := liveFiller(t, db, srv.URL[len("http://"):])

	res, err := h.Fill(context.Background(), FillRequest{
		AccountID: "acc", TestID: "ext:live", URL: srv.URL,
		SubmitPolicy: "human", ProfileText: "ПРОФИЛЬ",
	})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if res.Unsure || res.Unfilled != 0 {
		t.Fatalf("every field must be filled: unsure=%v unfilled=%d", res.Unsure, res.Unfilled)
	}
	if res.Submit != SubmitNotRequested {
		t.Fatalf("human policy must not click submit, got %v", res.Submit)
	}
	// Serialized DOM reflects the filled values (the artifact).
	for _, want := range []string{`value="Иван"`, `<option selected`, `>Люблю Go</textarea>`, "checked"} {
		if !strings.Contains(res.DOM, want) {
			t.Fatalf("DOM artifact missing %q:\n%s", want, res.DOM)
		}
	}
	if res.URL != srv.URL {
		t.Fatalf("final URL = %s", res.URL)
	}
}

func TestHeadlessLiveAutoSubmitsOnceAndNeverRepeats(t *testing.T) {
	if _, err := FindChrome(); err != nil {
		t.Skip("chrome not installed:", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(liveFormHTML))
	}))
	defer srv.Close()

	db := newDB(t)
	h := liveFiller(t, db, srv.URL[len("http://"):])
	req := FillRequest{AccountID: "acc", TestID: "ext:live-auto", URL: srv.URL,
		SubmitPolicy: "auto", ProfileText: "ПРОФИЛЬ"}

	res, err := h.Fill(context.Background(), req)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if res.Submit != SubmitClicked {
		t.Fatalf("auto policy must click submit once, got %v", res.Submit)
	}
	// The extsubmit intent is settled done: a replay must never click.
	if _, err := db.IntentsBegin(context.Background(), "extsubmit:acc:ext:live-auto", "pending", "x"); !errors.Is(err, store.ErrIntentDone) {
		t.Fatalf("intent must be done, got %v", err)
	}
	res2, err := h.Fill(context.Background(), req)
	if err != nil {
		t.Fatalf("replay Fill: %v", err)
	}
	if res2.Submit != SubmitAlreadyDone {
		t.Fatalf("replay must be SubmitAlreadyDone, got %v", res2.Submit)
	}
}

func TestHeadlessLiveQueueTaskEndToEndHuman(t *testing.T) {
	if _, err := FindChrome(); err != nil {
		t.Skip("chrome not installed:", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(liveFormHTML))
	}))
	defer srv.Close()

	db := newDB(t)
	seedResume(t, db)
	s := solver(t, db, &fakeRunner{}, &fakeNotifier{}, nil)
	s.Cfg.Forms.Mode = "headless"
	s.Cfg.Forms.AllowedDomains = append(s.Cfg.Forms.AllowedDomains, srv.URL[len("http://"):])
	s.Filler = liveFiller(t, db, srv.URL[len("http://"):])
	s.Queue = queue.New()

	task := solveTask(srv.URL)
	task.DedupKey = "solve:acc:live"
	if err := s.HandleSolveTest(context.Background(), task); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	rows, _ := db.AssessmentsListByAccount(context.Background(), "acc")
	if len(rows) != 1 || rows[0].Status != "in_progress" {
		t.Fatalf("rows = %+v", rows)
	}
	if deref(rows[0].Result) == "" {
		t.Fatal("DOM artifact must be stored on the row")
	}
}
