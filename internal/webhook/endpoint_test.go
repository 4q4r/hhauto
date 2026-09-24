package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

// --- test helpers ---

const testSecret = "whsec_test_123"

// recordStore captures requests made by the endpoint under test.
type recordStore struct {
	srv    *httptest.Server
	mu     chan struct{} // used as mutex via single-capacity channel
	bodies []string
	paths  []string
	cts    []string
	sigs   []string
	tss    []string
}

func newRecordStore(t *testing.T, status func(call int) int) *recordStore {
	t.Helper()
	rs := &recordStore{mu: make(chan struct{}, 1)}
	rs.mu <- struct{}{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		<-rs.mu
		rs.bodies = append(rs.bodies, string(body))
		rs.paths = append(rs.paths, r.URL.Path)
		rs.cts = append(rs.cts, r.Header.Get("Content-Type"))
		rs.sigs = append(rs.sigs, r.Header.Get("X-hhauto-Signature"))
		rs.tss = append(rs.tss, r.Header.Get("X-hhauto-Timestamp"))
		rs.mu <- struct{}{}
		if status != nil {
			w.WriteHeader(status(len(rs.bodies)))
		}
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *recordStore) calls() int {
	<-rs.mu
	n := len(rs.bodies)
	rs.mu <- struct{}{}
	return n
}

// recordingOutbox is a minimal in-memory Outbox double.
type recordingOutbox struct {
	added []OutboxItem
}

func (o *recordingOutbox) Add(endpointID string, payload []byte) error {
	o.added = append(o.added, OutboxItem{ID: int64(len(o.added) + 1), EndpointID: endpointID, Payload: payload})
	return nil
}

func (o *recordingOutbox) Due(now time.Time, limit int) ([]OutboxItem, error) {
	return nil, nil
}

func (o *recordingOutbox) MarkDelivered(id int64) error { return nil }

func (o *recordingOutbox) MarkFailed(id int64, nextRetry time.Time) error { return nil }

// fakeRegistry records resolved refs and redirects every dial to one address.
type fakeRegistry struct {
	refs   []string
	target string // host:port every dial is redirected to
}

func (f *fakeRegistry) Dialer(ref string) (proxy.Dialer, error) {
	f.refs = append(f.refs, ref)
	return redirectDialer{addr: f.target}, nil
}

func (f *fakeRegistry) HasProxies() bool { return true }

type redirectDialer struct{ addr string }

func (d redirectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.addr)
}

// newTestEndpoint builds an endpoint with instantaneous backoff for tests.
func newTestEndpoint(t *testing.T, cfg config.Webhook, reg proxy.Registry) (*endpoint, error) {
	t.Helper()
	t.Setenv("HHAUTO_TEST_SECRET", testSecret)
	cfg.SecretEnv = "HHAUTO_TEST_SECRET"
	ep, err := newEndpoint(cfg, reg)
	if err != nil {
		return ep, err
	}
	ep.backoff = func(int) time.Duration { return 0 }
	ep.client.Timeout = 2 * time.Second
	return ep, nil
}

// --- raw preset ---

func TestRawSignatureIsCorrectHMAC(t *testing.T) {
	rs := newRecordStore(t, nil)
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL, Preset: "raw"}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	ev := Event{Type: EvOfferReceived, AccountID: "acc1", Time: time.Now().UTC(), Message: "hello"}
	if _, err := ep.deliver(context.Background(), ev); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if got := rs.calls(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	body := rs.bodies[0]
	var decoded Event
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("body is not the JSON event: %v\nbody: %s", err, body)
	}
	if decoded.Type != EvOfferReceived || decoded.Message != "hello" {
		t.Fatalf("decoded event mismatch: %+v", decoded)
	}
	if rs.cts[0] != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", rs.cts[0])
	}

	ts := rs.tss[0]
	if ts == "" {
		t.Fatal("missing X-hhauto-Timestamp header")
	}
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte("v1." + ts + "." + body))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if rs.sigs[0] != want {
		t.Fatalf("signature = %q, want %q", rs.sigs[0], want)
	}
}

// --- telegram preset ---

func TestTelegramShaping(t *testing.T) {
	rs := newRecordStore(t, nil)
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL + "/bottoken/sendMessage", Preset: "telegram", ChatID: "@mychannel"}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	ev := Event{Type: EvNeedsHuman, Time: time.Now().UTC(), Message: `<b>alert</b> & "done"`}
	if _, err := ep.deliver(context.Background(), ev); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if got := rs.calls(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if want := "/bottoken/sendMessage"; rs.paths[0] != want {
		t.Fatalf("path = %q, want %q", rs.paths[0], want)
	}
	var payload struct {
		ChatID                string `json:"chat_id"`
		Text                  string `json:"text"`
		ParseMode             string `json:"parse_mode"`
		DisableWebPagePreview bool   `json:"disable_web_page_preview"`
	}
	if err := json.Unmarshal([]byte(rs.bodies[0]), &payload); err != nil {
		t.Fatalf("payload not JSON: %v\nbody: %s", err, rs.bodies[0])
	}
	if payload.ChatID != "@mychannel" {
		t.Errorf("chat_id = %q, want @mychannel", payload.ChatID)
	}
	if payload.ParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want HTML", payload.ParseMode)
	}
	if !payload.DisableWebPagePreview {
		t.Error("disable_web_page_preview = false, want true")
	}
	if want := `&lt;b&gt;alert&lt;/b&gt; &amp; &#34;done&#34;`; payload.Text != want {
		t.Errorf("text = %q, want %q", payload.Text, want)
	}
}

func TestTelegramTruncatesTo3500Chars(t *testing.T) {
	rs := newRecordStore(t, nil)
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL, Preset: "telegram", ChatID: "42"}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	long := strings.Repeat("ж", 4000) // multibyte runes: byte-length truncation would be wrong
	if _, err := ep.deliver(context.Background(), Event{Type: EvDailySummary, Message: long}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(rs.bodies[0]), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if got := len([]rune(payload.Text)); got != 3500 {
		t.Fatalf("text rune length = %d, want 3500", got)
	}
	if payload.Text != strings.Repeat("ж", 3500) {
		t.Fatal("truncated text lost or altered its runes")
	}
}

func TestTelegramTruncationKeepsEntitiesIntact(t *testing.T) {
	rs := newRecordStore(t, nil)
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL, Preset: "telegram", ChatID: "42"}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	// The cut lands exactly where an entity would start: a '&' at the limit.
	msg := strings.Repeat("a", telegramTextLimit-1) + "&"
	if _, err := ep.deliver(context.Background(), Event{Type: EvDailySummary, Message: msg}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := rs.calls(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(rs.bodies[0]), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	want := strings.Repeat("a", telegramTextLimit-1) + "&amp;"
	if payload.Text != want {
		t.Fatalf("truncated+escaped text = %q (tail %q), want ...%q", payload.Text, payload.Text[max(0, len(payload.Text)-12):], "&amp;")
	}
}

// --- discord preset ---

func TestDiscordShapingAndTruncation(t *testing.T) {
	rs := newRecordStore(t, nil)
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL, Preset: "discord"}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	long := strings.Repeat("x", 2500)
	if _, err := ep.deliver(context.Background(), Event{Type: EvApplySent, Message: long}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rs.bodies[0]), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if got := len(payload.Content); got != 2000 {
		t.Fatalf("content length = %d, want 2000", got)
	}
	var extraFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rs.bodies[0]), &extraFields); err != nil {
		t.Fatal(err)
	}
	if len(extraFields) != 1 {
		t.Fatalf("discord payload must contain only {content}, got %d fields", len(extraFields))
	}
}

// --- retry policy ---

func TestRetry429ThenSuccess(t *testing.T) {
	rs := newRecordStore(t, func(call int) int {
		if call < 3 {
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	})
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL, Preset: "raw", RetryMax: 3}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	if _, err := ep.deliver(context.Background(), Event{Type: EvTestSolved, Message: "m"}); err != nil {
		t.Fatalf("deliver should succeed after retries: %v", err)
	}
	if got := rs.calls(); got != 3 {
		t.Fatalf("calls = %d, want 3 (two 429 retries then success)", got)
	}
}

func TestRetry4xxNoRetry(t *testing.T) {
	rs := newRecordStore(t, func(int) int { return http.StatusBadRequest })
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL, Preset: "raw", RetryMax: 3}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	_, err = ep.deliver(context.Background(), Event{Type: EvApplySent, Message: "m"})
	if err == nil {
		t.Fatal("deliver must fail on 400")
	}
	if got := rs.calls(); got != 1 {
		t.Fatalf("calls = %d, want 1 (4xx without 429 must not retry)", got)
	}
}

func TestRetriesExhaustedThenOutbox(t *testing.T) {
	rs := newRecordStore(t, func(int) int { return http.StatusServiceUnavailable })
	ep, err := newTestEndpoint(t, config.Webhook{URL: rs.srv.URL, Preset: "raw", RetryMax: 2}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}
	ob := &recordingOutbox{}

	ev := Event{Type: EvContactCollected, Message: "payload-42"}
	payload, derr := ep.deliver(context.Background(), ev)
	if derr == nil {
		t.Fatal("deliver must fail when all attempts return 503")
	}
	if err := ep.park(context.Background(), payload, ob); err != nil {
		t.Fatalf("park: %v", err)
	}
	if len(ob.added) != 1 {
		t.Fatalf("outbox additions = %d, want 1", len(ob.added))
	}
	if ob.added[0].EndpointID != rs.srv.URL {
		t.Errorf("endpointID = %q, want %q", ob.added[0].EndpointID, rs.srv.URL)
	}
	// The parked body must be byte-for-byte the body that was sent.
	sent := rs.bodies[len(rs.bodies)-1]
	if !bytes.Equal(ob.added[0].Payload, []byte(sent)) {
		t.Fatalf("parked payload differs from sent body:\nsent  = %q\nparked= %q", sent, ob.added[0].Payload)
	}
	if !bytes.Equal(ob.added[0].Payload, payload) {
		t.Fatalf("parked payload differs from deliver() payload:\ndeliver  = %q\nparked   = %q", payload, ob.added[0].Payload)
	}
	var decoded Event
	if err := json.Unmarshal(ob.added[0].Payload, &decoded); err != nil {
		t.Fatalf("parked payload is not the raw event JSON: %v", err)
	}
	if decoded.Message != "payload-42" {
		t.Errorf("parked payload message = %q", decoded.Message)
	}
	if got := rs.calls(); got != 3 {
		t.Fatalf("calls = %d, want 3 (1 attempt + RetryMax=2 retries)", got)
	}
}

// --- outbox when server unreachable ---

func TestOutboxOnUnreachableServer(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := dead.Addr().String()
	dead.Close() // port now refused

	ep, err := newTestEndpoint(t, config.Webhook{URL: "http://" + addr + "/hook", Preset: "telegram", ChatID: "7"}, nil)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}
	ob := &recordingOutbox{}

	ev := Event{Type: EvAccountError, Message: "boom"}
	shaped, derr := ep.deliver(context.Background(), ev)
	if derr == nil {
		t.Fatal("deliver must fail on connection refused")
	}
	if err := ep.park(context.Background(), shaped, ob); err != nil {
		t.Fatalf("park: %v", err)
	}
	if len(ob.added) != 1 {
		t.Fatalf("outbox additions = %d, want 1", len(ob.added))
	}
	if !bytes.Equal(ob.added[0].Payload, shaped) {
		t.Fatal("parked payload must be byte-for-byte the shaped telegram body")
	}
	// The parked payload must be the shaped telegram body, replayable as-is.
	var payload struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(ob.added[0].Payload, &payload); err != nil {
		t.Fatalf("parked payload not telegram JSON: %v", err)
	}
	if payload.ChatID != "7" || payload.Text != "boom" {
		t.Errorf("parked telegram payload = %+v", payload)
	}
}

// --- proxy selection ---

func TestProxyClientSelectedByRef(t *testing.T) {
	rs := newRecordStore(t, nil)
	reg := &fakeRegistry{target: rs.srv.Listener.Addr().String()}
	ep, err := newTestEndpoint(t, config.Webhook{URL: "http://behind-proxy.invalid/hook", Preset: "raw", ProxyRef: "socks5-1"}, reg)
	if err != nil {
		t.Fatalf("newEndpoint: %v", err)
	}

	if _, err := ep.deliver(context.Background(), Event{Type: EvApplySkipped, Message: "via proxy"}); err != nil {
		t.Fatalf("deliver through proxy dialer: %v", err)
	}
	if len(reg.refs) != 1 || reg.refs[0] != "socks5-1" {
		t.Fatalf("registry refs = %v, want [socks5-1]", reg.refs)
	}
	if got := rs.calls(); got != 1 {
		t.Fatalf("calls = %d, want 1 (dial was redirected to the test server)", got)
	}
}

// --- configuration errors ---

func TestMissingSecretEnvIsEndpointConfigError(t *testing.T) {
	if _, err := newEndpoint(config.Webhook{URL: "http://example.invalid", Preset: "raw", SecretEnv: "HHAUTO_DEFINITELY_UNSET_ENV"}, nil); err == nil {
		t.Fatal("newEndpoint must fail when the secret env var is absent")
	}
}

func TestUnknownPresetIsEndpointConfigError(t *testing.T) {
	t.Setenv("HHAUTO_TEST_SECRET", testSecret)
	if _, err := newEndpoint(config.Webhook{URL: "http://example.invalid", Preset: "slack", SecretEnv: "HHAUTO_TEST_SECRET"}, nil); err == nil {
		t.Fatal("newEndpoint must fail on unknown preset")
	}
}

func TestUnknownProxyRefIsEndpointConfigError(t *testing.T) {
	reg := &failingRegistry{}
	if _, err := newEndpoint(config.Webhook{URL: "http://example.invalid", Preset: "raw", ProxyRef: "nope"}, reg); err == nil {
		t.Fatal("newEndpoint must fail when proxy_ref cannot be resolved (fail-closed)")
	}
}

type failingRegistry struct{}

func (failingRegistry) Dialer(string) (proxy.Dialer, error) {
	return nil, os.ErrNotExist
}

func (failingRegistry) HasProxies() bool { return true }
