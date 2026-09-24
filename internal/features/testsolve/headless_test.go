package testsolve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/queue"
	"hhauto/internal/store"
	"hhauto/internal/webhook"
)

// --- T17 headless: unit tests (no Chrome required) ---

// --- Chrome discovery ---

func TestFindChromeEnvOverride(t *testing.T) {
	t.Setenv("HHAUTO_CHROME_PATH", "/bin/true") // existing executable
	got, err := FindChrome()
	if err != nil || got != "/bin/true" {
		t.Fatalf("FindChrome = %q, %v; want /bin/true, nil", got, err)
	}
}

func TestFindChromeEnvOverrideMissingBinary(t *testing.T) {
	t.Setenv("HHAUTO_CHROME_PATH", "/nonexistent/chrome")
	_, err := FindChrome()
	if !errors.Is(err, ErrChromeMissing) {
		t.Fatalf("explicit but missing override must yield ErrChromeMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "HHAUTO_CHROME_PATH") {
		t.Fatalf("error must name the env var, got %v", err)
	}
}

func TestFindChromeMissingFromPATH(t *testing.T) {
	t.Setenv("HHAUTO_CHROME_PATH", "")
	// Empty PATH: no candidate name can resolve.
	t.Setenv("PATH", "")
	if _, err := FindChrome(); !errors.Is(err, ErrChromeMissing) {
		t.Fatalf("FindChrome without Chrome must return ErrChromeMissing, got %v", err)
	}
}

// --- proxy flag building ---

func headlessCfg() config.Config {
	cfg := formConfig()
	return cfg
}

func TestProxyServerForDirect(t *testing.T) {
	cfg := headlessCfg()
	cfg.Accounts = map[string]config.Account{"acc": {}}
	got, err := proxyServerFor(cfg, "acc")
	if err != nil || got != "" {
		t.Fatalf("direct account: proxyServerFor = %q, %v; want \"\", nil", got, err)
	}
}

func TestProxyServerForUnknownAccount(t *testing.T) {
	cfg := headlessCfg()
	got, err := proxyServerFor(cfg, "ghost")
	if err != nil || got != "" {
		t.Fatalf("unknown account is direct: = %q, %v", got, err)
	}
}

func TestProxyServerForNoAuthProxy(t *testing.T) {
	cfg := headlessCfg()
	cfg.Accounts = map[string]config.Account{"acc": {ProxyRef: "p1"}}
	cfg.Proxy = map[string]config.Proxy{
		"p1": {Type: "socks5", Host: "1.2.3.4", Port: 1080},
		"p2": {Type: "http", Host: "5.6.7.8", Port: 3128},
	}
	cfg.Accounts["acc2"] = config.Account{ProxyRef: "p2"}
	cfg.Accounts["acc3"] = config.Account{ProxyRef: "missing"}

	got, err := proxyServerFor(cfg, "acc")
	if err != nil || got != "socks5://1.2.3.4:1080" {
		t.Fatalf("socks5: = %q, %v", got, err)
	}
	got, err = proxyServerFor(cfg, "acc2")
	if err != nil || got != "http://5.6.7.8:3128" {
		t.Fatalf("http: = %q, %v", got, err)
	}
	// Unknown ref must fail closed, like proxy.Registry does.
	if _, err = proxyServerFor(cfg, "acc3"); err == nil {
		t.Fatal("unknown proxy_ref must fail closed")
	}
}

func TestProxyServerForAuthProxyUnsupported(t *testing.T) {
	cfg := headlessCfg()
	cfg.Accounts = map[string]config.Account{"acc": {ProxyRef: "p1"}}
	for name, p := range map[string]config.Proxy{
		"user":     {Type: "http", Host: "h", Port: 1, User: "u"},
		"pass_env": {Type: "http", Host: "h", Port: 1, PassEnv: "PW"},
		"pass":     {Type: "http", Host: "h", Port: 1, Pass: "x"},
	} {
		cfg.Proxy = map[string]config.Proxy{"p1": p}
		_, err := proxyServerFor(cfg, "acc")
		if !errors.Is(err, ErrAuthProxyUnsupported) {
			t.Fatalf("%s: want ErrAuthProxyUnsupported, got %v", name, err)
		}
		if !strings.Contains(err.Error(), "draft_only") {
			t.Fatalf("%s: error must suggest the alternative, got %v", name, err)
		}
	}
}

// --- DOM extraction decoder on fixture ---

func TestDecodeExtractedFields(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "headless_fields.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	page, err := decodeExtracted(string(raw))
	if err != nil {
		t.Fatalf("decodeExtracted: %v", err)
	}
	if page.PasswordDetected {
		t.Fatal("fixture has no password field")
	}
	if len(page.Fields) != 5 {
		t.Fatalf("fields = %d, want 5: %+v", len(page.Fields), page.Fields)
	}
	f0 := page.Fields[0]
	if f0.Kind != "text" || f0.Label != "Сколько лет вы разрабатываете на Go?" || !f0.Required || len(f0.Ctrls) != 1 {
		t.Fatalf("field0 = %+v", f0)
	}
	f1 := page.Fields[1]
	if f1.Kind != "select" || len(f1.Options) != 3 || f1.Options[0] != "Да, готов" {
		t.Fatalf("field1 = %+v", f1)
	}
	f2 := page.Fields[2]
	if f2.Kind != "radio" || len(f2.Ctrls) != 2 || f2.Options[0] != "Распределёнными командами" {
		t.Fatalf("field2 (radio group) = %+v", f2)
	}
	if page.Fields[4].Kind != "check" {
		t.Fatalf("field4 = %+v", page.Fields[4])
	}

	// The extracted fields must project onto AI questions in the same shape
	// the draft parser produces.
	form := page.form()
	if len(form.Fields) != 5 || form.Fields[1].Type != "select" || form.Fields[3].Type != "textarea" {
		t.Fatalf("form projection = %+v", form.Fields)
	}
}

func TestDecodeExtractedPasswordFlag(t *testing.T) {
	page, err := decodeExtracted(`{"password":true,"title":"login","fields":[]}`)
	if err != nil || !page.PasswordDetected {
		t.Fatalf("password flag lost: %+v, %v", page, err)
	}
}

func TestDecodeExtractedGarbage(t *testing.T) {
	if _, err := decodeExtracted("not json"); err == nil {
		t.Fatal("garbage extraction payload must error")
	}
}

// --- fill op planning (answer -> DOM operations) ---

func TestPlanFillOps(t *testing.T) {
	page, err := decodeExtracted(func() string {
		b, _ := os.ReadFile(filepath.Join("testdata", "headless_fields.json"))
		return string(b)
	}())
	if err != nil {
		t.Fatal(err)
	}
	answers := []AnswerEntry{
		{Question: "q1", Answer: "6 лет"},
		{Question: "q2", Answer: "да, готов"},                           // select: case-insensitive match
		{Question: "q3", Answer: "работал с распределёнными командами"}, // radio: substring match -> option index 0
		{Question: "q4", Answer: "Комментарий"},
		{Question: "q5", Answer: "Да"}, // check -> checked
	}
	ops, unfilled := planFillOps(page.Fields, answers)
	if unfilled != 0 {
		t.Fatalf("unfilled = %d, want 0", unfilled)
	}
	if len(ops) != 5 {
		t.Fatalf("ops = %d, want 5: %+v", len(ops), ops)
	}
	if ops[0].Kind != "set" || ops[0].Value != "6 лет" {
		t.Fatalf("op0 = %+v", ops[0])
	}
	if ops[1].Kind != "select" || ops[1].Index != 0 {
		t.Fatalf("op1 = %+v", ops[1])
	}
	if ops[2].Kind != "radio" || len(ops[2].Ctrls) != 1 || ops[2].Ctrls[0] != 2 {
		t.Fatalf("op2 must click the matched radio control (ctrls[2]), got %+v", ops[2])
	}
	if ops[4].Kind != "check" || !ops[4].Checked {
		t.Fatalf("op4 = %+v", ops[4])
	}
}

func TestPlanFillOpsUnmatchedOptionCountsUnfilled(t *testing.T) {
	page, _ := decodeExtracted(`{"fields":[
		{"kind":"select","ctrls":[0],"label":"Цвет","options":["Красный","Синий"]},
		{"kind":"radio","ctrls":[1,2],"label":"Размер","options":["S","M"]}
	]}`)
	answers := []AnswerEntry{{Answer: "Зелёный"}, {Answer: "не знаю что писать"}}
	ops, unfilled := planFillOps(page.Fields, answers)
	if unfilled != 2 {
		t.Fatalf("unfilled = %d, want 2", unfilled)
	}
	if len(ops) != 0 {
		t.Fatalf("unmatched answers must not produce DOM ops, got %+v", ops)
	}
}

func TestMatchOption(t *testing.T) {
	cases := []struct {
		opt, ans string
		want     bool
	}{
		{"Да, готов", "да, готов", true}, // case + exact
		{"Да, готов", "да готов", true},  // punctuation ignored
		{"Распределёнными командами", "с распределёнными командами", true}, // substring
		{"S", "M", false},
		{"", "что-то", false},
	}
	for _, c := range cases {
		if got := matchOption(c.opt, c.ans); got != c.want {
			t.Fatalf("matchOption(%q,%q) = %v, want %v", c.opt, c.ans, got, c.want)
		}
	}
}

func TestPositiveAnswer(t *testing.T) {
	for _, s := range []string{"да", "yes", "true", "1", " ДА ", "согласен"} {
		if !positiveAnswer(s) {
			t.Fatalf("positiveAnswer(%q) = false", s)
		}
	}
	for _, s := range []string{"нет", "no", "false", "0", ""} {
		if positiveAnswer(s) {
			t.Fatalf("positiveAnswer(%q) = true", s)
		}
	}
}

// --- submit-once semantics over the real store intent flow ---

func newHeadlessFiller(t *testing.T, db *store.DB) *HeadlessFiller {
	t.Helper()
	return &HeadlessFiller{
		Cfg:    headlessCfg(),
		DB:     db,
		Log:    slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) },
		Chrome: func() (string, error) { return "/fake/chrome", nil },
	}
}

func TestSubmitOnceClicksExactlyOnce(t *testing.T) {
	db := newDB(t)
	h := newHeadlessFiller(t, db)
	clicks := 0
	click := func(context.Context) error { clicks++; return nil }

	st, err := h.submitOnce(t.Context(), "acc", "ext:abc", click)
	if err != nil || st != SubmitClicked {
		t.Fatalf("first submit = %v, %v", st, err)
	}
	if clicks != 1 {
		t.Fatalf("clicks = %d, want 1", clicks)
	}
	// A done intent is a hard barrier: the click must never repeat.
	st, err = h.submitOnce(t.Context(), "acc", "ext:abc", click)
	if err != nil || st != SubmitAlreadyDone {
		t.Fatalf("second submit = %v, %v; want SubmitAlreadyDone", st, err)
	}
	if clicks != 1 {
		t.Fatalf("clicks after done intent = %d, want 1 (never repeat)", clicks)
	}
	// The key is account-scoped.
	st, _ = h.submitOnce(t.Context(), "acc2", "ext:abc", click)
	if st != SubmitClicked || clicks != 2 {
		t.Fatalf("other account must submit independently: %v clicks=%d", st, clicks)
	}
}

func TestSubmitOnceAmbiguousErrorNeverRepeats(t *testing.T) {
	db := newDB(t)
	h := newHeadlessFiller(t, db)
	boom := errors.New("evaluate lost after dispatch")
	click := func(context.Context) error { return boom }

	st, err := h.submitOnce(t.Context(), "acc", "ext:xyz", click)
	if err == nil || st != SubmitConsumed {
		t.Fatalf("ambiguous submit = %v, %v; want SubmitConsumed", st, err)
	}
	// The intent must be settled done even on error: a replay could
	// double-submit an external form, which is worse than a lost submit.
	st, err = h.submitOnce(t.Context(), "acc", "ext:xyz", func(context.Context) error {
		t.Error("submit must never repeat")
		return nil
	})
	if err != nil || st != SubmitAlreadyDone {
		t.Fatalf("after ambiguous = %v, %v; want SubmitAlreadyDone, nil", st, err)
	}
}

// --- HandleSolveTest wiring (fake Filler, no browser) ---

type fakeFiller struct {
	req   FillRequest
	res   FillResult
	err   error
	calls int
}

func (f *fakeFiller) Fill(_ context.Context, req FillRequest) (FillResult, error) {
	f.calls++
	f.req = req
	return f.res, f.err
}

func headlessSolver(t *testing.T, db *store.DB, runner ai.Runner, n *fakeNotifier, f Filler) *Solver {
	t.Helper()
	s := solver(t, db, runner, n, nil)
	s.Cfg.Forms.Mode = "headless"
	s.Filler = f
	return s
}

func TestHandleSolveTestHeadlessHumanPolicyDrafts(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	f := &fakeFiller{res: FillResult{
		URL:     okFormURL,
		DOM:     "<html>filled</html>",
		Answers: []AnswerEntry{{Question: "Вопрос 1", Answer: "6 лет", BasedOnProfile: true}},
	}}
	s := headlessSolver(t, db, &fakeRunner{}, n, f)

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("filler calls = %d", f.calls)
	}
	if f.req.SubmitPolicy != "human" || f.req.TestID != "ext:abc" || f.req.URL != okFormURL || f.req.AccountID != "acc" {
		t.Fatalf("fill request = %+v", f.req)
	}
	if f.req.ProfileText == "" {
		t.Fatal("candidate profile context must be passed to the filler")
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 || rows[0].Status != "in_progress" {
		t.Fatalf("rows = %+v", rows)
	}
	if !strings.Contains(deref(rows[0].Answers), `"mode":"headless"`) || !strings.Contains(deref(rows[0].Answers), "6 лет") {
		t.Fatalf("answers artifact missing: %s", deref(rows[0].Answers))
	}
	if deref(rows[0].Result) != "<html>filled</html>" {
		t.Fatalf("DOM artifact not stored: %q", deref(rows[0].Result))
	}
	if len(n.events) != 1 || n.events[0].Type != webhook.EvTestSolved {
		t.Fatalf("expected draft notification, got %+v", n.events)
	}
	if !strings.Contains(n.events[0].Message, "черновик готов") {
		t.Fatalf("draft-ready message expected, got %q", n.events[0].Message)
	}
}

func TestHandleSolveTestHeadlessAutoSubmitsOnce(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	f := &fakeFiller{res: FillResult{
		URL: okFormURL, DOM: "<html/>",
		Answers: []AnswerEntry{{Question: "q", Answer: "a"}},
		Submit:  SubmitClicked,
	}}
	s := headlessSolver(t, db, &fakeRunner{}, n, f)
	s.Cfg.Forms.SubmitPolicy = "auto"

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	if f.req.SubmitPolicy != "auto" {
		t.Fatalf("auto policy not forwarded: %+v", f.req)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 || rows[0].Status != "solved" {
		t.Fatalf("submitted test must be solved, got %+v", rows)
	}
	if len(n.events) != 1 || !strings.Contains(n.events[0].Message, "отправлен") {
		t.Fatalf("expected submitted notification, got %+v", n.events)
	}
}

func TestHandleSolveTestHeadlessSubmitAlreadyDoneNeverRepeats(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	f := &fakeFiller{res: FillResult{URL: okFormURL, DOM: "<html/>", Submit: SubmitAlreadyDone}}
	s := headlessSolver(t, db, &fakeRunner{}, n, f)
	s.Cfg.Forms.SubmitPolicy = "auto"

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("uncertain submit must be recorded, not failed: %v", err)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 || rows[0].Status != "escalated" {
		t.Fatalf("already-done submit must escalate, got %+v", rows)
	}
	items, _ := db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "test_submit_uncertain" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected test_submit_uncertain review, got %+v", items)
	}
	if len(n.events) == 0 || n.events[len(n.events)-1].Type != webhook.EvNeedsHuman {
		t.Fatalf("expected needs_human event, got %+v", n.events)
	}
}

func TestHandleSolveTestHeadlessPasswordAborts(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	f := &fakeFiller{err: ErrPasswordDetected}
	s := headlessSolver(t, db, &fakeRunner{}, n, f)

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("password abort must be recorded, not failed: %v", err)
	}
	items, _ := db.ReviewItemsOpen(t.Context(), "acc", 10)
	if len(items) != 1 || items[0].Kind != "phishing_suspect" {
		t.Fatalf("expected phishing_suspect review, got %+v", items)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 0 {
		t.Fatalf("no assessment on password abort, got %+v", rows)
	}
}

func TestHandleSolveTestHeadlessChromeMissingSurfaces(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	f := &fakeFiller{err: fmt.Errorf("wrap: %w", ErrChromeMissing)}
	s := headlessSolver(t, db, &fakeRunner{}, &fakeNotifier{}, f)

	err := s.HandleSolveTest(t.Context(), solveTask(okFormURL))
	if !errors.Is(err, ErrChromeMissing) {
		t.Fatalf("ErrChromeMissing must surface for engine/reporting, got %v", err)
	}
}

func TestHandleSolveTestHeadlessUnsureEscalates(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	n := &fakeNotifier{}
	f := &fakeFiller{res: FillResult{
		URL: okFormURL, DOM: "<html/>",
		Answers: []AnswerEntry{{Question: "q", Answer: "a"}},
		Unsure:  true, Unfilled: 1,
	}}
	s := headlessSolver(t, db, &fakeRunner{}, n, f)

	if err := s.HandleSolveTest(t.Context(), solveTask(okFormURL)); err != nil {
		t.Fatalf("HandleSolveTest: %v", err)
	}
	rows, _ := db.AssessmentsListByAccount(t.Context(), "acc")
	if len(rows) != 1 || rows[0].Status != "escalated" {
		t.Fatalf("unsure must escalate, got %+v", rows)
	}
	items, _ := db.ReviewItemsOpen(t.Context(), "acc", 10)
	found := false
	for _, it := range items {
		if it.Kind == "test_unsure" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected test_unsure review, got %+v", items)
	}
}

func TestHandleSolveTestHeadlessTransientFillErrorRequeues(t *testing.T) {
	db := newDB(t)
	seedResume(t, db)
	f := &fakeFiller{err: errors.New("page load timeout")}
	s := headlessSolver(t, db, &fakeRunner{}, &fakeNotifier{}, f)
	s.Queue = queue.New()
	task := solveTask(okFormURL)
	task.DedupKey = "solve:acc:ext:abc"

	if err := s.HandleSolveTest(t.Context(), task); err == nil {
		t.Fatal("transient fill error must surface for retry")
	}
	if n := s.Queue.Len("acc"); n != 1 {
		t.Fatalf("transient fill failure must requeue, len=%d", n)
	}
}

func TestHeadlessArtifactJSON(t *testing.T) {
	res := FillResult{
		URL: "https://x/form", DOM: "<html/>",
		Answers: []AnswerEntry{{Question: "q", Answer: "a", BasedOnProfile: true}},
		Unsure:  false, Unfilled: 0, Submit: SubmitClicked,
	}
	art := headlessArtifact("https://x/form", "chat_message", "chat1", res, time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(art, `"mode":"headless"`) || !strings.Contains(art, `"submitted":true`) {
		t.Fatalf("artifact = %s", art)
	}
}
