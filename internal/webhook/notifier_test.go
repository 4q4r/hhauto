package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/proxy"
)

// --- doubles ---

type countingServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	total int // monotonic request counter
	url   string
}

func newCountingServer(t *testing.T, status int) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		cs.mu.Lock()
		cs.total++
		cs.mu.Unlock()
		w.WriteHeader(status)
	}))
	cs.url = cs.srv.URL
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *countingServer) hits() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.total
}

func (cs *countingServer) waitHits(t *testing.T, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for cs.hits() < want {
		select {
		case <-deadline:
			t.Fatalf("server got %d hits, want %d (timeout)", cs.hits(), want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// newTestNotifier builds a notifier with instantaneous backoff.
func newTestNotifier(t *testing.T, endpoints []config.Webhook, reg proxy.Registry, outbox Outbox) *notifier {
	t.Helper()
	t.Setenv("HHAUTO_TEST_SECRET", testSecret)
	n := New(endpoints, reg, outbox, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Close(ctx) // idempotent; bounded so a stuck worker cannot hang the suite
	})
	for _, ep := range n.endpoints {
		ep.backoff = func(int) time.Duration { return 0 }
	}
	return n
}

// closeNotifier drains n with a bounded deadline.
func closeNotifier(t *testing.T, n *notifier) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func rawCfg(url string, retryMax int) config.Webhook {
	return config.Webhook{URL: url, Preset: "raw", SecretEnv: "HHAUTO_TEST_SECRET", RetryMax: retryMax}
}

// --- tests ---

func TestNotifierFiltersEventsPerEndpoint(t *testing.T) {
	filtered := newCountingServer(t, http.StatusOK)
	all := newCountingServer(t, http.StatusOK)

	n := newTestNotifier(t, []config.Webhook{
		{URL: filtered.url, Preset: "raw", SecretEnv: "HHAUTO_TEST_SECRET", Events: []string{EvOfferReceived}},
		rawCfg(all.url, 0),
	}, nil, nil)

	n.Emit(Event{Type: EvApplySent, Message: "first"})
	all.waitHits(t, 1)
	if got := filtered.hits(); got != 0 {
		t.Fatalf("filtered endpoint got %d hits for unsubscribed event, want 0", got)
	}

	n.Emit(Event{Type: EvOfferReceived, Message: "second"})
	all.waitHits(t, 2) // "all" endpoint receives every event
	if got := filtered.hits(); got != 1 {
		t.Fatalf("filtered endpoint got %d hits for subscribed event, want 1", got)
	}

	closeNotifier(t, n)
}

func TestNotifierEmitDoesNotBlockWhenServerIsSlow(t *testing.T) {
	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-releaseCh // stall the single worker on the first event
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(release)

	n := newTestNotifier(t, []config.Webhook{rawCfg(srv.URL, 0)}, nil, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < notifyQueueCapacity+500; i++ {
			n.Emit(Event{Type: EvApplySent, Message: "burst"})
		}
	}()

	select {
	case <-done:
		// Emit loop returned without blocking: correct.
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked past queue capacity: worker backpressure leaks to callers")
	}

	if n.Dropped() == 0 {
		t.Fatal("overflowing events were not dropped/counted")
	}

	release() // unblock the handler so the drain below can finish
	closeNotifier(t, n)
}

func TestNotifierCloseDrainsQueuedEvents(t *testing.T) {
	ok := newCountingServer(t, http.StatusOK)
	n := newTestNotifier(t, []config.Webhook{rawCfg(ok.url, 0)}, nil, nil)

	const total = 200
	for i := 0; i < total; i++ {
		n.Emit(Event{Type: EvContactCollected, Message: "drain me"})
	}
	closeNotifier(t, n)
	if got := ok.hits(); got != total {
		t.Fatalf("delivered %d of %d events after Close", got, total)
	}
}

func TestNotifierParksExhaustedDeliveriesInOutbox(t *testing.T) {
	dead := newCountingServer(t, http.StatusInternalServerError)
	ob := &recordingOutbox{}
	n := newTestNotifier(t, []config.Webhook{rawCfg(dead.url, 1)}, nil, ob) // 2 attempts total

	n.Emit(Event{Type: EvAccountError, Message: "park-me"})
	dead.waitHits(t, 2)
	closeNotifier(t, n)

	if len(ob.added) != 1 {
		t.Fatalf("outbox additions = %d, want 1", len(ob.added))
	}
	if ob.added[0].EndpointID != dead.url {
		t.Errorf("endpointID = %q, want %q", ob.added[0].EndpointID, dead.url)
	}
	var decoded Event
	if err := json.Unmarshal(ob.added[0].Payload, &decoded); err != nil {
		t.Fatalf("parked payload is not the raw event JSON: %v", err)
	}
	if decoded.Message != "park-me" {
		t.Errorf("parked payload message = %q", decoded.Message)
	}
}

func TestNotifierNilOutboxDropsAfterRetries(t *testing.T) {
	dead := newCountingServer(t, http.StatusBadGateway)
	n := newTestNotifier(t, []config.Webhook{rawCfg(dead.url, 0)}, nil, nil)

	n.Emit(Event{Type: EvAccountError, Message: "lost"})
	dead.waitHits(t, 1)
	closeNotifier(t, n)
	if got := dead.hits(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (RetryMax=0)", got)
	}
}

func TestNotifierSkipsMisconfiguredEndpoints(t *testing.T) {
	ok := newCountingServer(t, http.StatusOK)
	endpoints := []config.Webhook{
		rawCfg(ok.url, 0),
		{URL: "http://bad-preset.invalid", Preset: "slack"},
		{URL: "http://no-secret.invalid", Preset: "raw", SecretEnv: "HHAUTO_DEFINITELY_UNSET_ENV"},
	}
	n := newTestNotifier(t, endpoints, nil, nil)

	if len(n.endpoints) != 1 {
		t.Fatalf("kept %d endpoints, want 1 (misconfigured must be dropped)", len(n.endpoints))
	}

	n.Emit(Event{Type: EvNeedsHuman, Message: "who is left"})
	ok.waitHits(t, 1)
	closeNotifier(t, n)
	if got := ok.hits(); got != 1 {
		t.Fatalf("healthy endpoint got %d hits, want 1", got)
	}
}

func TestNotifierNoEndpointsIsUsable(t *testing.T) {
	n := newTestNotifier(t, nil, nil, nil)
	n.Emit(Event{Type: EvApplySent, Message: "nobody listens"})
	closeNotifier(t, n)
}
