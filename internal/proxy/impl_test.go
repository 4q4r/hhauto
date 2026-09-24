package proxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hhauto/internal/config"
)

// --- test doubles: minimal fake proxies ---------------------------------

// socksRequest records what the fake SOCKS5 server observed.
type socksRequest struct {
	atyp      byte   // 1=IPv4, 3=domain, 4=IPv6
	target    string // "host:port" as sent by the client
	user      string
	pass      string
	hadDomain bool
}

// startFakeSocks5 runs a minimal SOCKS5 server enforcing username/password
// auth (RFC 1928 method 0x02 + RFC 1929 subnegotiation) and relaying accepted
// connections to relayTo (regardless of the target the client names, so
// hostname pass-through can be asserted without real DNS).
func startFakeSocks5(t *testing.T, wantUser, wantPass string, failAuth bool, relayTo string) (addr string, reqs <-chan socksRequest, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake socks5 listen: %v", err)
	}
	ch := make(chan socksRequest, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				handleFakeSocks5(t, c, wantUser, wantPass, failAuth, relayTo, ch)
			}(conn)
		}
	}()
	return ln.Addr().String(), ch, func() { ln.Close(); wg.Wait() }
}

func handleFakeSocks5(t *testing.T, c net.Conn, wantUser, wantPass string, failAuth bool, relayTo string, reqs chan<- socksRequest) {
	t.Helper()
	r := bufio.NewReader(c)

	// Greeting: VER NMETHODS METHODS (RFC 1928).
	ver, err := r.ReadByte()
	if err != nil || ver != 5 {
		t.Errorf("socks5: bad greeting ver %v err %v", ver, err)
		return
	}
	nm, err := r.ReadByte()
	if err != nil || nm == 0 {
		t.Errorf("socks5: bad nmethods %v err %v", nm, err)
		return
	}
	methods := make([]byte, int(nm))
	if _, err := io.ReadFull(r, methods); err != nil {
		t.Errorf("socks5: read methods: %v", err)
		return
	}
	hasUP := false
	for _, m := range methods {
		if m == 0x02 {
			hasUP = true
		}
	}
	if !hasUP {
		t.Error("socks5: client did not offer username/password method 0x02")
		return
	}
	// Server selects method 0x02.
	if _, err := c.Write([]byte{0x05, 0x02}); err != nil {
		return
	}

	// RFC 1929 subnegotiation: VER ULEN UNAME PLEN PASSWD.
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		t.Errorf("socks5: read subneg header: %v", err)
		return
	}
	if hdr[0] != 0x01 {
		t.Errorf("socks5: subneg ver = %#x, want 0x01", hdr[0])
		return
	}
	uname := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(r, uname); err != nil {
		t.Errorf("socks5: read uname: %v", err)
		return
	}
	if _, err := io.ReadFull(r, hdr[:1]); err != nil {
		t.Errorf("socks5: read plen: %v", err)
		return
	}
	passwd := make([]byte, int(hdr[0]))
	if _, err := io.ReadFull(r, passwd); err != nil {
		t.Errorf("socks5: read passwd: %v", err)
		return
	}
	req := socksRequest{user: string(uname), pass: string(passwd)}
	status := byte(0x00)
	if failAuth || req.user != wantUser || req.pass != wantPass {
		status = 0x01
	}
	if _, err := c.Write([]byte{0x01, status}); err != nil {
		return
	}
	if status != 0x00 {
		return // RFC 1929: on failure the server MUST close the connection.
	}

	// Request: VER CMD RSV ATYP DST.ADDR DST.PORT.
	h := make([]byte, 4)
	if _, err := io.ReadFull(r, h); err != nil {
		t.Errorf("socks5: read request: %v", err)
		return
	}
	if h[0] != 0x05 || h[1] != 0x01 { // CONNECT only
		t.Errorf("socks5: ver=%#x cmd=%#x, want 0x05 0x01", h[0], h[1])
		return
	}
	req.atyp = h[3]
	var target string
	switch h[3] {
	case 0x01:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(r, ip); err != nil {
			return
		}
		target = net.IP(ip).String()
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return
		}
		dom := make([]byte, int(l[0]))
		if _, err := io.ReadFull(r, dom); err != nil {
			return
		}
		req.hadDomain = true
		target = string(dom)
	case 0x04:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(r, ip); err != nil {
			return
		}
		target = net.IP(ip).String()
	default:
		t.Errorf("socks5: unexpected atyp %#x", h[3])
		return
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(r, port); err != nil {
		return
	}
	req.target = net.JoinHostPort(target, strconv.Itoa(int(binary.BigEndian.Uint16(port))))
	select {
	case reqs <- req:
	default:
	}

	upstream, err := net.Dial("tcp", relayTo)
	if err != nil {
		c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // success
	relayBoth(c, r, upstream)
}

// relayBoth copies clientR to upstream and upstream to the client, closing
// both sides when either direction ends, so half-closes propagate and no
// goroutine leaks on wg.Wait.
func relayBoth(c net.Conn, clientR io.Reader, upstream net.Conn) {
	go func() {
		io.Copy(upstream, clientR)
		upstream.Close()
	}()
	io.Copy(c, upstream)
	c.Close()
	upstream.Close()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// startFakeConnectProxy runs a minimal HTTP CONNECT proxy. With
// tlsConfig != nil the listener speaks TLS (an "https" proxy). When quiet is
// true, client-side handshake aborts (expected in negative verification
// tests) are not reported as errors.
func startFakeConnectProxy(t *testing.T, wantUser, wantPass string, deny bool, tlsConfig *tls.Config, quiet bool) (addr string, seen <-chan string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake connect proxy listen: %v", err)
	}
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}
	ch := make(chan string, 8)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				handleFakeConnect(t, c, wantUser, wantPass, deny, quiet, ch)
			}(conn)
		}
	}()
	return ln.Addr().String(), ch, func() { ln.Close(); wg.Wait() }
}

func handleFakeConnect(t *testing.T, c net.Conn, wantUser, wantPass string, deny, quiet bool, seen chan<- string) {
	t.Helper()
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		if !quiet {
			t.Errorf("connect proxy: read request line: %v", err)
		}
		return
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "CONNECT" {
		if !quiet {
			t.Errorf("connect proxy: bad request line %q", line)
		}
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	target := fields[1]
	auth := ""
	for {
		h, err := r.ReadString('\n')
		if err != nil || h == "\r\n" || h == "\n" {
			break
		}
		if k, v, ok := strings.Cut(h, ":"); ok && strings.TrimSpace(k) == "Proxy-Authorization" {
			auth = strings.TrimSpace(v)
		}
	}
	select {
	case seen <- target:
	default:
	}
	want := ""
	if wantUser != "" || wantPass != "" {
		want = "Basic " + base64.StdEncoding.EncodeToString([]byte(wantUser+":"+wantPass))
	}
	if deny || auth != want {
		c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
		return
	}
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()
	c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	relayBoth(c, r, upstream)
}

// startEchoTarget starts a plain TCP echo server used as the tunnel target.
func startEchoTarget(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo target listen: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				io.Copy(c, c)
				c.Close()
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close(); wg.Wait() }
}

// testCA is a throwaway certificate authority for TLS-proxy tests. The
// client under test always uses default verification (no bypass option by
// design), so trust is injected via SSL_CERT_FILE (honored by crypto/x509).
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemPath string // path for SSL_CERT_FILE
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hhauto test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	pemPath := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(pemPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write ca pem: %v", err)
	}
	return testCA{cert: cert, key: key, pemPath: pemPath}
}

// issueCert mints a server certificate for ip/dnsName signed by the CA.
func (ca testCA) issueCert(t *testing.T, dnsName string, ip net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("server key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{dnsName}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("issue cert: %v", err)
	}
	if _, err := x509.ParseCertificate(der); err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: nil}
}

// unrelatedSelfSigned is a certificate from no trusted CA at all.
func unrelatedSelfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "stranger.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func tlsServerConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

// --- tests ---------------------------------------------------------------

func TestRegistryUnknownRefFailsClosed(t *testing.T) {
	reg, err := NewRegistry(map[string]config.Proxy{
		"socks5-1": {Type: "socks5", Host: "127.0.0.1", Port: 1080},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, err := reg.Dialer("nope"); err == nil {
		t.Fatal("Dialer(unknown ref) = nil error, want fail-closed error")
	}
	if _, err := reg.Dialer(""); err == nil {
		t.Fatal(`Dialer("") = nil error, want fail-closed error`)
	}
}

func TestRegistryUnknownTypeRejected(t *testing.T) {
	if _, err := NewRegistry(map[string]config.Proxy{
		"bad": {Type: "ftp", Host: "127.0.0.1", Port: 21},
	}); err == nil {
		t.Fatal("NewRegistry(type=ftp) = nil error, want error")
	}
	if _, err := NewRegistry(map[string]config.Proxy{
		"nohost": {Type: "socks5", Host: "", Port: 1080},
	}); err == nil {
		t.Fatal("NewRegistry(empty host) = nil error, want error")
	}
	if _, err := NewRegistry(map[string]config.Proxy{
		"noport": {Type: "http", Host: "127.0.0.1", Port: 0},
	}); err == nil {
		t.Fatal("NewRegistry(port=0) = nil error, want error")
	}
}

func TestRegistryHasProxies(t *testing.T) {
	empty, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}
	if empty.HasProxies() {
		t.Error("HasProxies() on empty registry = true, want false")
	}
	if _, err := empty.Dialer("any"); err == nil {
		t.Error("Dialer on empty registry must fail closed")
	}

	full, err := NewRegistry(map[string]config.Proxy{
		"p1": {Type: "socks5", Host: "127.0.0.1", Port: 1080},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if !full.HasProxies() {
		t.Error("HasProxies() on populated registry = false, want true")
	}
}

func TestSocks5DialThroughFakeServer(t *testing.T) {
	target, stopTarget := startEchoTarget(t)
	defer stopTarget()
	addr, reqs, stopProxy := startFakeSocks5(t, "alice", "wonderland", false, target)
	defer stopProxy()

	reg, err := NewRegistry(map[string]config.Proxy{
		"s1": {Type: "socks5", Host: hostOf(addr), Port: portOf(addr), User: "alice", Pass: "wonderland"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, err := reg.Dialer("s1")
	if err != nil {
		t.Fatalf("Dialer(s1): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Hostname (not IP): the target host must reach the proxy as a domain
	// name (ATYP=0x03) — DNS resolution happens proxy-side, never locally.
	const dst = "echo.invalid:80"
	conn, err := d.DialContext(ctx, "tcp", dst)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()

	select {
	case req := <-reqs:
		if !req.hadDomain {
			t.Errorf("socks5 request ATYP=%d, want 0x03 (domain, resolved proxy-side)", req.atyp)
		}
		if req.target != dst {
			t.Errorf("socks5 target = %q, want %q", req.target, dst)
		}
		if req.user != "alice" || req.pass != "wonderland" {
			t.Errorf("socks5 creds = %q/%q, want alice/wonderland", req.user, req.pass)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake socks5 server saw no request")
	}

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("tunnel payload = %q, want %q", buf, "ping")
	}
}

func TestSocks5AuthFailureIsError(t *testing.T) {
	addr, _, stopProxy := startFakeSocks5(t, "alice", "wonderland", true, "") // server rejects every subneg
	defer stopProxy()

	reg, err := NewRegistry(map[string]config.Proxy{
		"s1": {Type: "socks5", Host: hostOf(addr), Port: portOf(addr), User: "alice", Pass: "wonderland"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, err := reg.Dialer("s1")
	if err != nil {
		t.Fatalf("Dialer(s1): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:1"); err == nil {
		conn.Close()
		t.Fatal("DialContext through rejecting socks5 server = nil error, want error")
	}
}

func TestHTTPConnectProxy(t *testing.T) {
	target, stopTarget := startEchoTarget(t)
	defer stopTarget()
	addr, seen, stopProxy := startFakeConnectProxy(t, "bob", "builder", false, nil, false)
	defer stopProxy()

	reg, err := NewRegistry(map[string]config.Proxy{
		"h1": {Type: "http", Host: hostOf(addr), Port: portOf(addr), User: "bob", Pass: "builder"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, err := reg.Dialer("h1")
	if err != nil {
		t.Fatalf("Dialer(h1): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer conn.Close()

	select {
	case got := <-seen:
		if got != target {
			t.Errorf("CONNECT target = %q, want %q", got, target)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake CONNECT proxy saw no request")
	}

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("pong")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(buf) != "pong" {
		t.Errorf("tunnel payload = %q, want %q", buf, "pong")
	}
}

func TestHTTPConnectProxyBadCredentialsRejected(t *testing.T) {
	addr, _, stopProxy := startFakeConnectProxy(t, "bob", "builder", false, nil, false)
	defer stopProxy()

	reg, err := NewRegistry(map[string]config.Proxy{
		"h1": {Type: "http", Host: hostOf(addr), Port: portOf(addr), User: "bob", Pass: "wrong"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, err := reg.Dialer("h1")
	if err != nil {
		t.Fatalf("Dialer(h1): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:1"); err == nil {
		conn.Close()
		t.Fatal("DialContext with bad proxy credentials = nil error, want error (407)")
	}
}

func TestHTTPSProxyTLSVerification(t *testing.T) {
	target, stopTarget := startEchoTarget(t)
	defer stopTarget()

	ca := newTestCA(t)
	t.Setenv("SSL_CERT_FILE", ca.pemPath) // inject trust for the default pool

	// Positive: CA-signed certificate valid for 127.0.0.1 — the https proxy
	// must connect.
	good := ca.issueCert(t, "proxy.local", net.IPv4(127, 0, 0, 1))
	addr, _, stopProxy := startFakeConnectProxy(t, "", "", false, tlsServerConfig(good), false)
	defer stopProxy()

	reg, err := NewRegistry(map[string]config.Proxy{
		"hs": {Type: "https", Host: hostOf(addr), Port: portOf(addr)},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, err := reg.Dialer("hs")
	if err != nil {
		t.Fatalf("Dialer(hs): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatalf("DialContext through https proxy with valid cert: %v", err)
	}
	conn.Close()

	// Negative 1: trusted CA but wrong name — hostname verification is on.
	badName := ca.issueCert(t, "other.example", nil)
	addrN1, _, stopN1 := startFakeConnectProxy(t, "", "", false, tlsServerConfig(badName), true)
	defer stopN1()
	regN1, err := NewRegistry(map[string]config.Proxy{
		"n1": {Type: "https", Host: hostOf(addrN1), Port: portOf(addrN1)},
	})
	if err != nil {
		t.Fatalf("NewRegistry n1: %v", err)
	}
	dN1, err := regN1.Dialer("n1")
	if err != nil {
		t.Fatalf("Dialer(n1): %v", err)
	}
	if conn, err := dN1.DialContext(ctx, "tcp", target); err == nil {
		conn.Close()
		t.Fatal("DialContext with mismatched cert name = nil error, want hostname verification error")
	}

	// Negative 2: untrusted self-signed certificate — chain verification is
	// on. There is deliberately no option to disable any of this.
	stranger := unrelatedSelfSigned(t)
	addrN2, _, stopN2 := startFakeConnectProxy(t, "", "", false, tlsServerConfig(stranger), true)
	defer stopN2()
	regN2, err := NewRegistry(map[string]config.Proxy{
		"n2": {Type: "https", Host: hostOf(addrN2), Port: portOf(addrN2)},
	})
	if err != nil {
		t.Fatalf("NewRegistry n2: %v", err)
	}
	dN2, err := regN2.Dialer("n2")
	if err != nil {
		t.Fatalf("Dialer(n2): %v", err)
	}
	if conn, err := dN2.DialContext(ctx, "tcp", target); err == nil {
		conn.Close()
		t.Fatal("DialContext with untrusted certificate = nil error, want chain verification error")
	}
}

func TestPassEnvResolvesPassword(t *testing.T) {
	t.Setenv("HHAUTO_T3_TEST_PASS", "from-env")
	target, stopTarget := startEchoTarget(t)
	defer stopTarget()
	addr, reqs, stopProxy := startFakeSocks5(t, "carol", "from-env", false, target)
	defer stopProxy()

	// PassEnv wins over the discouraged inline Pass.
	reg, err := NewRegistry(map[string]config.Proxy{
		"e1": {Type: "socks5", Host: hostOf(addr), Port: portOf(addr), User: "carol", Pass: "inline", PassEnv: "HHAUTO_T3_TEST_PASS"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d, err := reg.Dialer("e1")
	if err != nil {
		t.Fatalf("Dialer(e1): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatalf("DialContext with PassEnv creds: %v", err)
	}
	defer conn.Close()
	select {
	case req := <-reqs:
		if req.user != "carol" || req.pass != "from-env" {
			t.Errorf("creds = %q/%q, want carol/from-env (PassEnv wins)", req.user, req.pass)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake socks5 server saw no request")
	}
}

// hostOf/portOf split a "127.0.0.1:port" listener address for config.Proxy.
func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	return h
}

func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		panic(err)
	}
	return n
}
