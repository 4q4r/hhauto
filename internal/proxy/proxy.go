// Package proxy builds per-reference dialers from config. Fail-closed:
// an unknown reference is an error, never a silent direct fallback.
package proxy

import (
	"context"
	"net"
)

// Dialer dials connections through a specific proxy.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// Registry resolves proxy references ("socks5-1", ...) from [proxy.*] config.
type Registry interface {
	Dialer(ref string) (Dialer, error) // unknown ref => error (fail-closed)
	HasProxies() bool
}
