// Per-reference dialer implementations over config.Proxy values.
//
// Security posture:
//   - fail-closed: an unknown registry reference is an error, never a direct
//     fallback (the engine resolves ProxyRef="" to direct explicitly).
//   - "https" proxies are reached over TLS with certificate verification
//     always enabled; there is deliberately no option to disable it.
//   - for SOCKS5 the target hostname is passed through as-is, so DNS
//     resolution happens on the proxy side, never locally.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"hhauto/internal/config"
)

// contextDialer is the optional interface x/net proxy.Dialer implementations
// provide for context-aware dialing (see proxy.ContextDialer).
type contextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

type registry struct {
	dialers map[string]Dialer
}

// NewRegistry builds one dialer per configured proxy reference. Construction
// fails on invalid entries so misconfiguration surfaces at startup, not on
// the first request.
func NewRegistry(proxies map[string]config.Proxy) (*registry, error) {
	dialers := make(map[string]Dialer, len(proxies))
	for ref, p := range proxies {
		d, err := buildDialer(p)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: %w", ref, err)
		}
		dialers[ref] = d
	}
	return &registry{dialers: dialers}, nil
}

// Dialer resolves a reference to its dialer. Unknown refs are an error:
// the registry never falls back to a direct connection.
func (r *registry) Dialer(ref string) (Dialer, error) {
	d, ok := r.dialers[ref]
	if !ok {
		return nil, fmt.Errorf("proxy: unknown proxy ref %q", ref)
	}
	return d, nil
}

// HasProxies reports whether any proxy is configured.
func (r *registry) HasProxies() bool {
	return len(r.dialers) > 0
}

func buildDialer(p config.Proxy) (Dialer, error) {
	if p.Host == "" {
		return nil, errors.New("proxy: empty host")
	}
	if p.Port <= 0 || p.Port > 65535 {
		return nil, fmt.Errorf("proxy: port %d out of range", p.Port)
	}
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	user, pass := credentials(p)

	switch p.Type {
	case "socks5":
		var auth *proxy.Auth
		if user != "" || pass != "" {
			auth = &proxy.Auth{User: user, Password: pass}
		}
		d, err := proxy.SOCKS5("tcp", addr, auth, nil)
		if err != nil {
			return nil, fmt.Errorf("proxy: socks5 dialer: %w", err)
		}
		cd, ok := d.(contextDialer)
		if !ok {
			return nil, errors.New("proxy: socks5 dialer does not support DialContext")
		}
		return cd, nil
	case "http", "https":
		return &connectDialer{
			proxyAddr: addr,
			proxyHost: p.Host,
			tlsProxy:  p.Type == "https",
			user:      user,
			pass:      pass,
		}, nil
	default:
		return nil, fmt.Errorf("proxy: unknown type %q (want socks5, http or https)", p.Type)
	}
}

// credentials resolves the proxy password: PassEnv (recommended) wins over
// the inline Pass (discouraged in config).
func credentials(p config.Proxy) (user, pass string) {
	user = p.User
	pass = p.Pass
	if p.PassEnv != "" {
		if v := os.Getenv(p.PassEnv); v != "" {
			pass = v
		}
	}
	return user, pass
}

// connectDialer dials targets through an HTTP/1.1 CONNECT proxy, optionally
// reached over TLS ("https" proxies).
type connectDialer struct {
	proxyAddr string // host:port of the proxy itself
	proxyHost string // TLS ServerName for https proxies
	tlsProxy  bool
	user      string
	pass      string
}

const maxConnectHeader = 8 << 10 // bound the CONNECT response header

func (c *connectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("proxy: CONNECT supports tcp networks only, got %q", network)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy: dial proxy: %w", err)
	}
	if c.tlsProxy {
		// Verification is always enabled: no InsecureSkipVerify anywhere.
		conn = tls.Client(conn, &tls.Config{ServerName: c.proxyHost})
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n"
	if c.user != "" || c.pass != "" {
		token := base64.StdEncoding.EncodeToString([]byte(c.user + ":" + c.pass))
		req += "Proxy-Authorization: Basic " + token + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy: send CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := readConnectResponse(br)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("proxy: read CONNECT response: %w", err)
	}
	code, ok := parseStatusCode(statusLine)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("proxy: malformed CONNECT response %q", statusLine)
	}
	if code != 200 {
		conn.Close()
		return nil, fmt.Errorf("proxy: CONNECT rejected with status %d", code)
	}

	// Tunnel established: the dial ctx scoped the handshake only; the caller
	// manages deadlines for the tunneled connection from here on.
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// readConnectResponse consumes the full CONNECT response header block
// (status line, headers, terminating empty line) so that no header bytes
// leak into the tunneled stream, and returns the status line.
func readConnectResponse(br *bufio.Reader) (string, error) {
	var total int
	var status string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		total += len(line)
		if total > maxConnectHeader {
			return "", errors.New("CONNECT response header too large")
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if status == "" {
				continue // tolerate stray empty lines
			}
			return status, nil
		}
		if status == "" && strings.HasPrefix(trimmed, "HTTP/") {
			status = trimmed
		}
	}
}

func parseStatusCode(statusLine string) (int, bool) {
	parts := strings.Split(statusLine, " ")
	if len(parts) < 2 {
		return 0, false
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// bufferedConn returns bytes already buffered by the CONNECT handshake
// reader before handing the tunneled connection to the caller.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
