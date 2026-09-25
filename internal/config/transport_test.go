package config

// T24: [api] transport selects the hhapi HTTP stack: "tls-client" (default,
// browser TLS fingerprint) or "http" (legacy stdlib transport for debugging).

import (
	"strings"
	"testing"
)

func TestTransportDefaultsToTLSClient(t *testing.T) {
	var c Config
	applyDefaults(&c)
	if c.API.Transport != TransportTLSClient {
		t.Fatalf("api.transport default = %q, want %q", c.API.Transport, TransportTLSClient)
	}
}

func TestTransportPreservesExplicitHTTP(t *testing.T) {
	c := Config{API: API{Transport: TransportHTTP}}
	applyDefaults(&c)
	if c.API.Transport != TransportHTTP {
		t.Fatalf("explicit api.transport = %q, want %q", c.API.Transport, TransportHTTP)
	}
}

func TestValidateAcceptsBothTransports(t *testing.T) {
	for _, mode := range []string{TransportTLSClient, TransportHTTP} {
		c := validConfig()
		c.API.Transport = mode
		if err := Validate(c); err != nil {
			t.Fatalf("Validate(transport=%q): %v", mode, err)
		}
	}
}

func TestValidateRejectsUnknownTransport(t *testing.T) {
	c := validConfig()
	c.API.Transport = "grpc"
	err := Validate(c)
	if err == nil || !strings.Contains(err.Error(), "api.transport") {
		t.Fatalf("Validate(transport=grpc) = %v, want api.transport error", err)
	}
}
