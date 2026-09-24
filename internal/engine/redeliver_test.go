package engine

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
	"hhauto/internal/webhook"
)

func discardLogger() *slog.Logger { return slog.New(discardHandler{}) }

func TestRedelivererRawPresetSignsAndDelivers(t *testing.T) {
	t.Setenv("T12_OUTBOX_SECRET", "s3cret-outbox-key")

	var (
		gotSig      string
		gotTS       string
		gotBody     []byte
		gotCT       string
		gotRequests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequests++
		gotSig = r.Header.Get("X-hhauto-Signature")
		gotTS = r.Header.Get("X-hhauto-Timestamp")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newRedeliverer([]config.Webhook{{
		URL:       srv.URL,
		Preset:    "raw",
		SecretEnv: "T12_OUTBOX_SECRET",
	}}, nil, discardLogger())

	payload := []byte(`{"type":"apply_sent","account_id":"a1"}`)
	err := r.Redeliver(context.Background(), webhook.OutboxItem{EndpointID: srv.URL, Payload: payload})
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if gotRequests != 1 {
		t.Fatalf("requests = %d, want 1", gotRequests)
	}
	if string(gotBody) != string(payload) {
		t.Errorf("body replayed byte-for-byte: got %q, want %q", gotBody, payload)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("content type = %q", gotCT)
	}
	// The signature must verify against the received timestamp and body.
	mac := hmac.New(sha256.New, []byte("s3cret-outbox-key"))
	mac.Write([]byte("v1." + gotTS + "."))
	mac.Write(payload)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if gotSig != want {
		t.Errorf("signature = %q, want %q", gotSig, want)
	}
	if ts, perr := strconv.ParseInt(gotTS, 10, 64); perr != nil || time.Now().Unix()-ts > 60 {
		t.Errorf("timestamp %q is not fresh", gotTS)
	}
}

func TestRedelivererTelegramPresetPlainPOST(t *testing.T) {
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-hhauto-Signature")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newRedeliverer([]config.Webhook{{
		URL:    srv.URL,
		Preset: "telegram",
		ChatID: "42",
	}}, nil, discardLogger())

	err := r.Redeliver(context.Background(), webhook.OutboxItem{
		EndpointID: srv.URL,
		Payload:    []byte(`{"chat_id":"42","text":"hi"}`),
	})
	if err != nil {
		t.Fatalf("Redeliver: %v", err)
	}
	if gotSig != "" {
		t.Errorf("telegram preset must not be signed, got %q", gotSig)
	}
	if gotBody != `{"chat_id":"42","text":"hi"}` {
		t.Errorf("body = %q", gotBody)
	}
}

func TestRedelivererHTTPErrorIsRetryableFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := newRedeliverer([]config.Webhook{{
		URL:       srv.URL,
		Preset:    "raw",
		SecretEnv: "T12_OUTBOX_SECRET",
	}}, nil, discardLogger())

	err := r.Redeliver(context.Background(), webhook.OutboxItem{EndpointID: srv.URL, Payload: []byte(`{}`)})
	if err == nil {
		t.Fatal("5xx must be an error so the worker parks the item again")
	}
}

func TestRedelivererUnknownEndpointAndBadConfig(t *testing.T) {
	r := newRedeliverer(nil, nil, discardLogger())
	err := r.Redeliver(context.Background(), webhook.OutboxItem{EndpointID: "https://x/", Payload: []byte(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "unknown endpoint") {
		t.Fatalf("err = %v, want unknown endpoint", err)
	}

	// A raw endpoint without secret_env is disabled at construction.
	r2 := newRedeliverer([]config.Webhook{{URL: "https://x/", Preset: "raw"}}, nil, discardLogger())
	if len(r2.targets) != 0 {
		t.Fatal("misconfigured endpoint must be disabled, not stored")
	}
}

type stubProxyReg struct {
	dialErr error
}

func (s stubProxyReg) Dialer(string) (proxy.Dialer, error) {
	if s.dialErr != nil {
		return nil, s.dialErr
	}
	return stubDialer{}, nil
}

func (s stubProxyReg) HasProxies() bool { return true }

type stubDialer struct{}

func (stubDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("not used in tests")
}

func TestRedelivererProxyRefFailClosed(t *testing.T) {
	// proxy_ref with no registry: endpoint disabled.
	r := newRedeliverer([]config.Webhook{{
		URL: "https://x/", Preset: "telegram", ChatID: "1", ProxyRef: "p1",
	}}, nil, discardLogger())
	if len(r.targets) != 0 {
		t.Fatal("proxy_ref without registry must disable the endpoint")
	}

	// Unknown/unresolvable ref from a registry: also disabled.
	r2 := newRedeliverer([]config.Webhook{{
		URL: "https://x/", Preset: "telegram", ChatID: "1", ProxyRef: "p1",
	}}, stubProxyReg{dialErr: errors.New("unknown ref")}, discardLogger())
	if len(r2.targets) != 0 {
		t.Fatal("unresolvable proxy_ref must disable the endpoint")
	}
}
