package hhapi

// T37 v3 integration: a tls-client transport built with
// WithBrowserTLSSpec must emit the captured browser ClientHello on the
// wire. The local CONNECT-style tap records the first TLS record the
// transport produces and it is matched structurally against the capture
// (GREASE-normalized, order-insensitive). The tap deliberately does not
// complete the handshake — the assertion target is the emitted bytes,
// not a negotiated session.

import (
	"context"
	"log/slog"
	"net"
	"net/http/cookiejar"
	"net/url"
	"testing"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"

	"hhauto/internal/tlsfp"
)

// buildCaptureHello assembles a compact but realistic chrome-shaped
// ClientHello record (GREASE ciphers, GREASE key-share entry, MLKEM-less
// X25519 shares, supported_versions, ALPN h2+http/1.1, ALPS on the
// 17613 codepoint, psk_modes, compress_certificate) — enough structure
// to pin every replay-critical decision without duplicating the full
// tlsfp golden.
func buildCaptureHello(t *testing.T, sni string) []byte {
	t.Helper()

	ext := func(id uint16, data []byte) []byte {
		hdr := []byte{byte(id >> 8), byte(id), byte(len(data) >> 8), byte(len(data))}
		return append(hdr, data...)
	}
	name := []byte(sni)
	sniBody := append([]byte{0x00, byte((len(name) + 3) >> 8), byte(len(name) + 3), 0x00, byte(len(name) >> 8), byte(len(name))}, name...)
	alps := append([]byte{0x00, 0x03, 0x02}, []byte("h2")...)
	alpn := append([]byte{0x00, 0x0c, 0x02}, append([]byte("h2"), append([]byte{0x08}, []byte("http/1.1")...)...)...)
	groups := append([]byte{0x00, 0x06, 0x1a, 0x1a}, []byte{0x00, 0x1d, 0x00, 0x17}...)
	sigalgs := append([]byte{0x00, 0x10}, []byte{0x04, 0x03, 0x08, 0x04, 0x04, 0x01, 0x05, 0x03, 0x08, 0x05, 0x05, 0x01, 0x08, 0x06, 0x06, 0x01}...)
	kshare := append([]byte{
		0x00, 0x29, // client shares length (filled below for clarity)
		0x1a, 0x1a, 0x00, 0x01, 0x00, // grease group, 1-byte share
		0x00, 0x1d, 0x00, 0x20,
	}, make([]byte, 32)...)
	kshare[0], kshare[1] = byte((len(kshare)-2)>>8), byte(len(kshare)-2)

	var exts []byte
	for _, e := range [][]byte{
		ext(0x0a0a, []byte{0x00}),       // GREASE ext
		ext(0x0000, sniBody),            // server_name
		ext(0x0017, nil),                // extended_master_secret
		ext(0xff01, []byte{0x00}),       // renegotiation_info
		ext(0x000a, groups),             // supported_groups
		ext(0x000b, []byte{0x01, 0x00}), // ec_point_formats
		ext(0x0023, nil),                // session_ticket
		ext(0x44cd, alps),               // ALPS new codepoint
		ext(0x0010, alpn),               // alpn
		ext(0x0005, nil),                // status_request: zero-length (RFC 8446 form)
		ext(0x000d, sigalgs),            // signature_algorithms
		ext(0x0033, kshare),             // key_share
		ext(0x002d, []byte{0x01, 0x01}), // psk_key_exchange_modes
		ext(0x002b, []byte{0x06, 0x1a, 0x1a, 0x03, 0x04, 0x03, 0x03}), // supported_versions
		ext(0x001b, []byte{0x04, 0x00, 0x02, 0x00, 0x01}),             // compress_certificate (1-byte list len)
	} {
		exts = append(exts, e...)
	}

	ciphers := []uint16{0x1a1a, 0x1301, 0x1302, 0x1303}
	hello := append([]byte{0x03, 0x03}, make([]byte, 32)...) // version + random
	hello = append(hello, 0x20)                              // session id len
	hello = append(hello, make([]byte, 32)...)               // session id
	hello = append(hello, byte(len(ciphers)*2>>8), byte(len(ciphers)*2))
	for _, c := range ciphers {
		hello = append(hello, byte(c>>8), byte(c))
	}
	hello = append(hello, 0x01, 0x00) // compression: null
	hello = append(hello, byte((len(exts) >> 8)), byte(len(exts)))
	hello = append(hello, exts...)

	hs := append([]byte{0x01, 0x00, byte(len(hello) >> 8), byte(len(hello))}, hello...)
	return append([]byte{0x16, 0x03, 0x01, byte(len(hs) >> 8), byte(len(hs))}, hs...)
}

func captureSpec(t *testing.T) *tlsfp.Spec {
	t.Helper()
	meta := tlsfp.CaptureMeta{
		CapturedAt:  time.Now().UTC().Format(time.RFC3339),
		BrowserPath: "/tmp/chrome",
		BrowserVer:  "151.0.7922.108.6",
		Host:        "example.test",
	}
	s, err := tlsfp.ParseClientHello(buildCaptureHello(t, meta.Host), meta)
	if err != nil {
		t.Fatalf("parse capture: %v", err)
	}
	if err := s.Verify(2); err != nil {
		t.Fatalf("capture must self-verify: %v", err)
	}
	return s
}

func TestTLSClientTransportReplaysBrowserHello(t *testing.T) {
	spec := captureSpec(t)
	prof, err := browserClientProfile("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36", spec)
	if err != nil {
		t.Fatalf("browser profile: %v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	// The transport is assembled exactly as newTLSTransport does for the
	// browser-spec path.
	tc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(tlsClientTimeout),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithClientProfile(prof),
		tls_client.WithRandomTLSExtensionOrder(),
	)
	if err != nil {
		t.Fatalf("tls-client: %v", err)
	}
	tp := &tlsTransport{tc: tc, jar: jar}

	// The tap is the endpoint: it records the hello and closes.
	tap, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer tap.Close() //nolint:errcheck // test tap
	helloCh := make(chan []byte, 1)
	go func() {
		c, err := tap.Accept()
		if err != nil {
			return
		}
		defer c.Close() //nolint:errcheck // test tap
		raw, err := tlsfp.ReadClientHelloRecord(c)
		if err != nil {
			return
		}
		helloCh <- raw
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	u, err := url.Parse("https://" + tap.Addr().String() + "/")
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	// The handshake against the bare tap always fails after the hello is
	// read; the round trip error is expected and ignored.
	_, _ = tp.roundTrip(ctx, "GET", u, nil, nil)

	select {
	case raw := <-helloCh:
		if err := spec.MatchesWire(raw); err != nil {
			t.Fatalf("emitted hello diverges from the capture: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no ClientHello captured at the tap")
	}
}

// T50 strict parity: a broken capture cannot fall back to a preset
// profile — the transport must fail the account build instead.
func TestTLSClientTransportFailsOnBrokenSpec(t *testing.T) {
	spec := captureSpec(t)
	broken := *spec
	broken.RawClientHelloHex = "zznot-hex"
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	if _, err := newTLSTransport(jar, "Chrome/151", "", &broken, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a spec that cannot build must fail the transport, not fall back")
	}
}

// T50 strict parity: the tls-client transport without a captured spec is
// a wiring error — the approximate preset path no longer exists.
func TestTLSClientTransportRequiresSpec(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	if _, err := newTLSTransport(jar, "Chrome/151", "", nil, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("nil spec must fail the tls-client transport")
	}
}
