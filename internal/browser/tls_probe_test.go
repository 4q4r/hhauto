package browser

// Unit tests for the T37 v3 CONNECT tap: tunnel parsing, target
// guarding, hello sniffing and the byte-exact relay.

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"hhauto/internal/tlsfp"
)

// fakeUpstream is a minimal TCP echo used as the relay target.
func fakeUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close() //nolint:errcheck // test echo
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestConnectTargetAccepts443(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close() //nolint:errcheck // pipe
	defer c2.Close() //nolint:errcheck // pipe
	go func() {
		_, _ = io.WriteString(c1, "CONNECT hh.ru:443 HTTP/1.1\r\nHost: hh.ru:443\r\n\r\n")
	}()
	got, err := connectTarget(c2, allowTLS443)
	if err != nil {
		t.Fatalf("connectTarget: %v", err)
	}
	if got != "hh.ru:443" {
		t.Fatalf("target %q", got)
	}
}

func TestConnectTargetRejectsNonTLS(t *testing.T) {
	for _, req := range []string{
		"GET http://hh.ru/ HTTP/1.1\r\nHost: hh.ru\r\n\r\n",
		"CONNECT hh.ru:8080 HTTP/1.1\r\nHost: hh.ru:8080\r\n\r\n",
		"CONNECT hh.ru HTTP/1.1\r\nHost: hh.ru\r\n\r\n",
	} {
		c1, c2 := net.Pipe()
		go func(r string) {
			_, _ = io.WriteString(c1, r)
			_, _ = io.ReadAll(c1) // drain until close
		}(req)
		if _, err := connectTarget(c2, allowTLS443); err == nil {
			t.Fatalf("request must be rejected: %q", req)
		}
		_ = c2.Close()
		_ = c1.Close()
	}
}

func TestRelayAndSniffCapturesAndRelays(t *testing.T) {
	upAddr := fakeUpstream(t)
	upPort := upAddr[strings.LastIndexByte(upAddr, ':')+1:]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test tap
	captured := make(chan []byte, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveTap(ctx, ln, func(_, port string) bool { return port == upPort }, captured)

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close() //nolint:errcheck // test client

	// Open the tunnel to the echo upstream.
	if _, err := io.WriteString(client, "CONNECT "+upAddr+" HTTP/1.1\r\nHost: "+upAddr+"\r\n\r\n"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	br := bufio.NewReader(client)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("tunnel response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("tunnel status %d", resp.StatusCode)
	}

	// Send the golden hello through the tunnel; the tap must capture it
	// and relay it byte-exact to the echo upstream, which bounces it back.
	rawHello := tlsfpSpecForTest(t).RawClientHelloHex
	if rawHello == "" {
		t.Fatalf("empty golden hello")
	}
	raw := mustUnhex(t, rawHello)
	if _, err := client.Write(raw); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	select {
	case got := <-captured:
		if string(got) != string(raw) {
			t.Fatalf("captured hello differs from the sent bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no hello captured")
	}

	echo := make([]byte, len(raw))
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != string(raw) {
		t.Fatalf("relayed hello differs from the sent bytes")
	}
}

func TestRelayAndSniffIgnoresNonTLSFirstBytes(t *testing.T) {
	upAddr := fakeUpstream(t)
	upPort := upAddr[strings.LastIndexByte(upAddr, ':')+1:]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test tap
	captured := make(chan []byte, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveTap(ctx, ln, func(_, port string) bool { return port == upPort }, captured)

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close() //nolint:errcheck // test client
	if _, err := io.WriteString(client, "CONNECT "+upAddr+" HTTP/1.1\r\nHost: "+upAddr+"\r\n\r\n"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	br := bufio.NewReader(client)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatalf("tunnel response: %v", err)
	}
	// A plaintext first record must not be posted as a hello.
	junk := []byte("not a tls record at all, just text")
	if _, err := client.Write(junk); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	// The relay still forwards it — the echo proves the tunnel stays up.
	select {
	case raw := <-captured:
		t.Fatalf("non-TLS bytes captured as a hello: %q", raw[:min(len(raw), 20)])
	case <-time.After(1500 * time.Millisecond):
	}
	_ = client.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	echo := make([]byte, len(junk)+4)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo[len(junk):]) != "ping" {
		t.Fatalf("echo broken: %q", echo)
	}
}

func TestChromeVersionFallback(t *testing.T) {
	// A nonexistent executable degrades to the file name, not an error.
	got := ChromeVersion(context.Background(), "/opt/fake-dir/cloak-chrome-42", slog.New(slog.DiscardHandler))
	if got == "" {
		t.Fatalf("empty version fallback")
	}
	if want := "cloak-chrome-42"; got != want {
		t.Fatalf("fallback %q, want %q", got, want)
	}
}

func mustUnhex(t *testing.T, s string) []byte {
	t.Helper()
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		hi, lo := hexNibble(t, s[i]), hexNibble(t, s[i+1])
		out = append(out, hi<<4|lo)
	}
	return out
}

func hexNibble(t *testing.T, b byte) byte {
	t.Helper()
	switch {
	case b >= '0' && b <= '9':
		return b - '0'
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10
	}
	t.Fatalf("bad hex nibble %q", b)
	return 0
}

// tlsfpSpecForTest builds a small capture directly through the tlsfp
// parser: the probe tests only need valid hello bytes, and keeping the
// golden in the tlsfp package avoids a test-only export cycle.
func tlsfpSpecForTest(t *testing.T) *tlsfp.Spec {
	t.Helper()
	return captureHelloFixture(t)
}

// captureHelloFixture builds a compact valid ClientHello record for the
// tap tests (GREASE cipher, key shares, supported_versions, ALPN).
func captureHelloFixture(t *testing.T) *tlsfp.Spec {
	t.Helper()
	ext := func(id uint16, data []byte) []byte {
		hdr := []byte{byte(id >> 8), byte(id), byte(len(data) >> 8), byte(len(data))}
		return append(hdr, data...)
	}
	alpn := append([]byte{0x00, 0x0c, 0x02}, append([]byte("h2"), append([]byte{0x08}, []byte("http/1.1")...)...)...)
	kshare := append([]byte{
		0x00, 0x29,
		0x1a, 0x1a, 0x00, 0x01, 0x00,
		0x00, 0x1d, 0x00, 0x20,
	}, make([]byte, 32)...)
	kshare[0], kshare[1] = byte((len(kshare)-2)>>8), byte(len(kshare)-2)

	var exts []byte
	for _, e := range [][]byte{
		ext(0x0a0a, []byte{0x00}),
		ext(0x0010, alpn),
		ext(0x000a, []byte{0x00, 0x04, 0x1a, 0x1a, 0x00, 0x1d}),
		ext(0x000d, []byte{0x00, 0x04, 0x04, 0x03, 0x08, 0x04}),
		ext(0x0033, kshare),
		ext(0x002d, []byte{0x01, 0x01}),
		ext(0x002b, []byte{0x04, 0x1a, 0x1a, 0x03, 0x04}),
	} {
		exts = append(exts, e...)
	}
	hello := append([]byte{0x03, 0x03}, make([]byte, 32)...)
	hello = append(hello, 0x20)
	hello = append(hello, make([]byte, 32)...)
	hello = append(hello, 0x00, 0x08, 0x1a, 0x1a, 0x13, 0x01, 0x13, 0x02, 0x13, 0x03)
	hello = append(hello, 0x01, 0x00)
	hello = append(hello, byte((len(exts) >> 8)), byte(len(exts)))
	hello = append(hello, exts...)
	hs := append([]byte{0x01, 0x00, byte(len(hello) >> 8), byte(len(hello))}, hello...)
	raw := append([]byte{0x16, 0x03, 0x01, byte(len(hs) >> 8), byte(len(hs))}, hs...)

	spec, err := tlsfp.ParseClientHello(raw, tlsfp.CaptureMeta{Host: "example.test", BrowserVer: "test"})
	if err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	return spec
}
