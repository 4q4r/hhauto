package proxy

// T24: the tls-client transport consumes full proxy URLs (SetProxy), so the
// [proxy.*] config table needs a URL projection with URL-escaped credentials.

import (
	"testing"

	"hhauto/internal/config"
)

func TestProxyURLFromConfig(t *testing.T) {
	t.Setenv("T24_TEST_PASS", "s3cret:pass/w ord")

	tests := []struct {
		name    string
		p       config.Proxy
		want    string
		wantErr bool
	}{
		{
			name: "socks5 with auth from pass_env",
			p:    config.Proxy{Type: "socks5", Host: "1.2.3.4", Port: 1080, User: "u", PassEnv: "T24_TEST_PASS"},
			want: "socks5://u:s3cret%3Apass%2Fw%20ord@1.2.3.4:1080",
		},
		{
			name: "http without auth",
			p:    config.Proxy{Type: "http", Host: "p.example", Port: 8080},
			want: "http://p.example:8080",
		},
		{
			name: "https without auth",
			p:    config.Proxy{Type: "https", Host: "p.example", Port: 443},
			want: "https://p.example:443",
		},
		{name: "unknown type", p: config.Proxy{Type: "ftp", Host: "h", Port: 21}, wantErr: true},
		{name: "empty host", p: config.Proxy{Type: "socks5", Port: 1080}, wantErr: true},
		{name: "port out of range", p: config.Proxy{Type: "socks5", Host: "h", Port: 70000}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := URL(tt.p)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("URL(%+v) = %q, want error", tt.p, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("URL(%+v): %v", tt.p, err)
			}
			if got != tt.want {
				t.Fatalf("URL(%+v) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}
