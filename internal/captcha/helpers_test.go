package captcha

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// syncedBuffer is a concurrency-safe byte buffer for log capture in tests.
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Reset empties the buffer (test-only convenience).
func (b *syncedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// newTestLogger returns a slog.Logger writing WARN-level records to buf.
func newTestLogger(buf *syncedBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newTLSCaptchaServer hosts a captcha protocol fake over TLS and makes the
// process-wide default transport trust its self-signed certificate, so
// solvers built with the stock client can reach it. Adapters enforce
// https-only base URLs (fix round 1), and New-built solvers expose no client
// injection point, so trusting the certificate process-wide is the only way
// to exercise the full construction path without weakening production code.
// Cleanup order matters: the server closes first (waiting out every
// outstanding connection), and only then is the transport TLS config
// restored — otherwise a late dial can race the restore write. Tests using
// this helper must not run in parallel.
func newTLSCaptchaServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)

	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is not *http.Transport")
	}
	fakeTransport, ok := srv.Client().Transport.(*http.Transport)
	if !ok || fakeTransport.TLSClientConfig == nil {
		t.Fatalf("fake server client has no TLS config")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = fakeTransport.TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved }) // runs after srv.Close (LIFO)
	t.Cleanup(srv.Close)                              // runs first: drains and closes conns
	return srv
}
