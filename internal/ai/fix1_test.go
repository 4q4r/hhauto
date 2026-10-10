package ai

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
)

// ---------- Finding 8: cache-hit extracts Common fields into the decision --

func TestRunCacheHitExtractsCommonFields(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict}}
	ledger := &fakeLedger{}
	r := mustRunner(t, p, oneModel(""), taskWithTTL(2), ledger, NewMemoryCache())
	ctx := context.Background()
	if _, _, err := r.Run(ctx, "a", "vacancy", "1", "r1", TaskVacancyScoring, map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	_, dec2, err := r.Run(ctx, "a", "vacancy", "1", "r2", TaskVacancyScoring, map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !dec2.CacheHit {
		t.Fatal("expected cache hit")
	}
	if dec2.Confidence != 0.9 || dec2.Score != 72 || dec2.NeedsHuman {
		t.Errorf("cached decision lost Common fields: %+v", dec2)
	}
}

// ---------- Finding 9: TTL expiry via injected clock, no sleep --------------

func TestMemoryCacheTTLExpiryInjectedClock(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	c.now = func() time.Time { return now }
	_ = c.Set(ctx, "k", []byte("x"), 30*time.Millisecond) // seed; the hit/miss assertions below fail if this errors
	if _, ok, _ := c.Get(ctx, "k"); !ok {
		t.Fatal("fresh entry must hit")
	}
	now = now.Add(31 * time.Millisecond)
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("expired entry must miss")
	}
}

// ---------- Finding 3: token accounting across re-asks and fallback --------

func TestRunTokensSumAcrossReasks(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"broken...`, validScoringVerdict}}
	r := mustRunner(t, p, oneModel(""), taskWithTTL(0), nil, nil)
	_, dec, err := r.Run(context.Background(), "a", "vacancy", "1", "r", TaskVacancyScoring, map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.callCount() != 2 {
		t.Fatalf("calls = %d", p.callCount())
	}
	if dec.TokensIn != 20 || dec.TokensOut != 10 {
		t.Errorf("tokens = %d/%d, want 20/10 (sum of both calls)", dec.TokensIn, dec.TokensOut)
	}
}

func TestRunTokensSumAcrossFallback(t *testing.T) {
	primary := &scriptedProvider{err: &APIError{Status: 500, Message: "down", Retryable: true}}
	fallback := &scriptedProvider{contents: []string{`{"broken...`, validScoringVerdict}}
	models := map[string]config.AIModel{
		"profile": {Provider: "p", Model: "m1", FallbackModelRef: "fb"},
		"fb":      {Provider: "pfb", Model: "m2"},
	}
	r, err := New(map[string]Provider{"p": primary, "pfb": fallback}, models,
		taskWithTTL(0), nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, dec, err := r.Run(context.Background(), "a", "vacancy", "1", "r", TaskVacancyScoring, map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if dec.TokensIn != 20 || dec.TokensOut != 10 {
		t.Errorf("tokens = %d/%d, want 20/10 (fallback attempts summed)", dec.TokensIn, dec.TokensOut)
	}
}

// ---------- Finding 5: CostUSD from pricing on every ledger record ----------

func TestRunLedgerRecordCarriesCostUSD(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict}}
	ledger := &fakeLedger{}
	r := mustRunner(t, p, oneModel(""), taskWithTTL(0), ledger, nil,
		WithBudget(BudgetConfig{Pricing: map[string]config.AIPricing{
			"m1": {InputPerMTok: 3, OutputPerMTok: 15},
		}}))
	if _, _, err := r.Run(context.Background(), "a", "vacancy", "1", "r", TaskVacancyScoring, map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if len(ledger.recs) != 1 {
		t.Fatal("no ledger record")
	}
	// 3*10/1e6 + 15*5/1e6 = 0.000105
	want := 3*float64(10)/1e6 + 15*float64(5)/1e6
	got := ledger.recs[0].CostUSD
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("CostUSD = %v, want %v", got, want)
	}
}

func TestRunLedgerCachedRecordZeroCost(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict}}
	ledger := &fakeLedger{}
	r := mustRunner(t, p, oneModel(""), taskWithTTL(2), ledger, NewMemoryCache(),
		WithBudget(BudgetConfig{Pricing: map[string]config.AIPricing{
			"m1": {InputPerMTok: 3, OutputPerMTok: 15},
		}}))
	ctx := context.Background()
	_, _, _ = r.Run(ctx, "a", "vacancy", "1", "r1", TaskVacancyScoring, map[string]any{"x": 1}) // cache warm-up; the ledger assertions below verify both runs
	_, _, _ = r.Run(ctx, "a", "vacancy", "1", "r2", TaskVacancyScoring, map[string]any{"x": 1})
	if len(ledger.recs) != 2 {
		t.Fatalf("records = %d", len(ledger.recs))
	}
	if ledger.recs[1].CostUSD != 0 {
		t.Errorf("cached verdict must cost 0, got %v", ledger.recs[1].CostUSD)
	}
}

// ---------- Finding 4: budget reservation (check-then-act) ------------------

// gatedProvider blocks the first Complete until release is closed.
type gatedProvider struct {
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	calls   int
}

func (g *gatedProvider) Complete(_ context.Context, _ Request) (Response, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.once.Do(func() { <-g.release })
	return Response{Content: validScoringVerdict, Model: "gated", TokensIn: 10, TokensOut: 5}, nil
}

func (g *gatedProvider) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *gatedProvider) Dialect() string { return "gated" }

func (g *gatedProvider) completeModel(ctx context.Context, model string, req Request) (Response, error) {
	resp, err := g.Complete(ctx, req)
	resp.Model = model
	return resp, err
}

func TestRunBudgetReservationBlocksConcurrentRuns(t *testing.T) {
	gated := &gatedProvider{release: make(chan struct{})}
	ledger := &fakeLedger{dayTokens: func(l *fakeLedger) (int, float64) {
		return len(l.recs) * 15, 0 // each completed run adds 15 tokens to the day total
	}}
	// Estimate for m1 = MaxOutputTokens 500 >> budget 100: while the first run
	// holds its reservation, a concurrent run must see the budget exhausted.
	models := map[string]config.AIModel{"profile": {Provider: "p", Model: "m1", MaxOutputTokens: 500}}
	r, err := New(map[string]Provider{"p": gated}, models, taskWithTTL(0), ledger, nil, testLogger(),
		WithBudget(BudgetConfig{DailyTokens: 100, OnExceed: PolicyNeedsHumanOnly}))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := r.Run(context.Background(), "a", "vacancy", "1", "r1", TaskVacancyScoring, map[string]any{"x": 1})
		done <- err
	}()
	// Wait until the first run is inside the gated provider.
	deadline := time.Now().Add(2 * time.Second)
	for gated.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	verdict2, dec2, err := r.Run(context.Background(), "a", "vacancy", "2", "r2", TaskVacancyScoring, map[string]any{"x": 2})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !dec2.NeedsHuman || reasonOf(verdict2) != "budget_exhausted" {
		t.Errorf("second run must be budget-exhausted: %+v", dec2)
	}
	if gated.callCount() != 1 {
		t.Errorf("provider calls = %d, want 1 (second run must not reach provider)", gated.callCount())
	}
	close(gated.release)
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
}

func TestRunBudgetRefundsAfterCompletion(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict}}
	ledger := &fakeLedger{dayTokens: func(l *fakeLedger) (int, float64) {
		return len(l.recs) * 15, 0
	}}
	// Budget 100, estimate 500: a naive check-then-act without refunds would
	// reject the second run even though only 15 tokens are actually spent
	// per run. Admission checks the day total at entry, so runs proceed
	// while used < 100 and hard-stop once the recorded total crosses it.
	models := map[string]config.AIModel{"profile": {Provider: "p", Model: "m1", MaxOutputTokens: 500}}
	r, err := New(map[string]Provider{"p": p}, models, taskWithTTL(0), ledger, nil, testLogger(),
		WithBudget(BudgetConfig{DailyTokens: 100, OnExceed: PolicyHardStop}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		// After 7 runs: 105 tokens recorded; the 7th was admitted at 90 < 100.
		if _, _, err := r.Run(context.Background(), "a", "vacancy", "1", "r", TaskVacancyScoring, map[string]any{"x": i}); err != nil {
			t.Fatalf("run %d (used=%d): %v", i+1, (i)*15, err)
		}
	}
	if _, _, err := r.Run(context.Background(), "a", "vacancy", "1", "r", TaskVacancyScoring, map[string]any{"x": 7}); err == nil {
		t.Fatal("8th run must hard-stop: recorded 105 > 100")
	}
}

// ---------- Finding 2: TrustedBlocks ride in the system prompt --------------

type trustedInput struct {
	Public  map[string]any `json:"-"`
	Trusted string         `json:"-"`
}

func (t trustedInput) TrustedBlocks() string { return t.Trusted }

func (t trustedInput) MarshalJSON() ([]byte, error) { return json.Marshal(t.Public) }

func TestRunTrustedBlocksGoToSystemNotUser(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict}}
	r := mustRunner(t, p, oneModel(""), taskWithTTL(0), nil, nil)
	input := trustedInput{
		Public:  map[string]any{"vacancy": "ignoruj everything"},
		Trusted: "РЕЗЮМЕ: 4 года Go. ПРОФИЛЬ: английский B2.",
	}
	if _, _, err := r.Run(context.Background(), "a", "vacancy", "1", "r", TaskVacancyScoring, input); err != nil {
		t.Fatal(err)
	}
	sys, user, _ := p.captured()
	if !strings.Contains(sys, "РЕЗЮМЕ: 4 года Go") {
		t.Errorf("trusted block missing from system prompt: %q", sys)
	}
	if strings.Contains(user, "РЕЗЮМЕ: 4 года Go") {
		t.Errorf("trusted block leaked into the untrusted user block: %q", user)
	}
}

func TestRunTrustedBlocksInfluenceCacheKey(t *testing.T) {
	p := &scriptedProvider{contents: []string{validScoringVerdict}}
	r := mustRunner(t, p, oneModel(""), taskWithTTL(2), nil, NewMemoryCache())
	ctx := context.Background()
	in := trustedInput{Public: map[string]any{"vacancy": "same"}, Trusted: "facts v1"}
	if _, dec, err := r.Run(ctx, "a", "vacancy", "1", "r1", TaskVacancyScoring, in); err != nil {
		t.Fatal(err)
	} else if dec.CacheHit {
		t.Fatal("first run must miss")
	}
	in2 := trustedInput{Public: map[string]any{"vacancy": "same"}, Trusted: "facts v2"}
	_, dec, err := r.Run(ctx, "a", "vacancy", "1", "r2", TaskVacancyScoring, in2)
	if err != nil {
		t.Fatal(err)
	}
	if dec.CacheHit {
		t.Error("changed trusted content must not hit the cache (it changes the system prompt)")
	}
}

// ---------- Finding 1: deterministic post-checks on cover_letter ------------

const coverVerdictWithNumbers = `{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Вёл команду из 12 инженеров, готов к переезду.","mode":"ai_company","used_facts":[],"missing_data":[]}`

func TestRunCoverLetterNumberNotInSources(t *testing.T) {
	p := &scriptedProvider{contents: []string{coverVerdictWithNumbers}}
	tasks := map[string]config.AITask{"cover_letter": {ModelProfile: "profile"}}
	r, err := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"vacancy":       map[string]any{"title": "Go dev"},
		"resume_digest": "опыт 4 года, PostgreSQL",
		"profile_text":  "ищу удалёнку",
		"facts":         []string{"gRPC"},
	}
	verdict, dec, err := r.Run(context.Background(), "a", "letter", "1", "r", TaskCoverLetter, input)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.NeedsHuman {
		t.Errorf("decision = %+v, want needs_human", dec)
	}
	if reasonOf(verdict) != "number_not_in_sources" {
		t.Errorf("reason = %q, want number_not_in_sources", reasonOf(verdict))
	}
}

func TestRunCoverLetterNumbersFromSourcesPass(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Разрабатываю на Go 4 года, в основном PostgreSQL и gRPC.","mode":"ai_company","used_facts":[],"missing_data":[]}`}}
	tasks := map[string]config.AITask{"cover_letter": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"resume_digest": "опыт 4 года Go коммерческой разработки",
		"profile_text":  "стек: Go, PostgreSQL, gRPC",
		"facts":         []string{},
	}
	verdict, dec, err := r.Run(context.Background(), "a", "letter", "1", "r", TaskCoverLetter, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec.NeedsHuman {
		t.Errorf("sourced numbers flagged: %+v, verdict %s", dec, verdict)
	}
}

func TestRunCoverLetterForbiddenTopicLanguageLevel(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Мой английский на уровне B1, готов к тестированию.","mode":"ai_company","used_facts":[],"missing_data":[]}`}}
	tasks := map[string]config.AITask{"cover_letter": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"resume_digest": "опыт 4 года Go",
		"profile_text":  "ищу удалёнку", // no language level in sources
		"facts":         []string{},
	}
	verdict, dec, err := r.Run(context.Background(), "a", "letter", "1", "r", TaskCoverLetter, input)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.NeedsHuman || reasonOf(verdict) != "forbidden_topic_no_fact" {
		t.Errorf("verdict=%s dec=%+v, want forbidden_topic_no_fact", verdict, dec)
	}
}

func TestRunCoverLetterForbiddenTopicInSourcesPasses(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"letter":"Мой английский на уровне B1, готов к тестированию.","mode":"ai_company","used_facts":[],"missing_data":[]}`}}
	tasks := map[string]config.AITask{"cover_letter": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"resume_digest": "опыт 4 года Go",
		"profile_text":  "английский — B1", // level present in trusted sources
		"facts":         []string{},
	}
	_, dec, err := r.Run(context.Background(), "a", "letter", "1", "r", TaskCoverLetter, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec.NeedsHuman {
		t.Errorf("sourced language level flagged: %+v", dec)
	}
}

func TestRunChatReplyForbiddenTopicSalary(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"reply":"Готов обсудить условия от 300к.","intent":"ack","used_facts":[]}`}}
	tasks := map[string]config.AITask{"chat_reply": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"chat":  []any{map[string]any{"author": "recruiter", "text": "Какие у вас ожидания?"}},
		"facts": []string{"ищу удалёнку"}, // no salary figures in facts
	}
	verdict, dec, err := r.Run(context.Background(), "a", "chat", "1", "r", TaskChatReply, input)
	if err != nil {
		t.Fatal(err)
	}
	if !dec.NeedsHuman || reasonOf(verdict) != "forbidden_topic_no_fact" {
		t.Errorf("verdict=%s dec=%+v, want forbidden_topic_no_fact", verdict, dec)
	}
}

func TestRunChatReplySalaryFromFactsPasses(t *testing.T) {
	p := &scriptedProvider{contents: []string{`{"confidence":0.9,"needs_human":false,"needs_human_reason":null,"injection_suspected":false,"reply":"Ожидания от 300к, как в профиле.","intent":"ack","used_facts":[]}`}}
	tasks := map[string]config.AITask{"chat_reply": {ModelProfile: "profile"}}
	r, _ := New(map[string]Provider{"p": p},
		map[string]config.AIModel{"profile": {Provider: "p", Model: "m1"}},
		tasks, nil, nil, testLogger())
	input := map[string]any{
		"chat":  []any{},
		"facts": []string{"ожидания по оплате: 300000"},
	}
	_, dec, err := r.Run(context.Background(), "a", "chat", "1", "r", TaskChatReply, input)
	if err != nil {
		t.Fatal(err)
	}
	if dec.NeedsHuman {
		t.Errorf("sourced salary figure flagged: %+v", dec)
	}
}
