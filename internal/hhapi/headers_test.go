package hhapi

import (
	"strings"
	"testing"
)

const chromeWinUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

const edgeWinUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.0.0"

const androidUA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"

func TestParseClientHintsChrome(t *testing.T) {
	brands, mobile, platform, ok := parseClientHints(chromeWinUA)
	if !ok {
		t.Fatal("parseClientHints(chrome) ok = false, want true")
	}
	if !strings.Contains(brands, `v="128"`) {
		t.Fatalf("brands = %q, want major version 128 inside", brands)
	}
	if !strings.Contains(brands, "Google Chrome") || !strings.Contains(brands, "Chromium") {
		t.Fatalf("brands = %q, want Chromium and Google Chrome entries", brands)
	}
	if mobile != "?0" {
		t.Fatalf("mobile = %q, want ?0", mobile)
	}
	if platform != `"Windows"` {
		t.Fatalf("platform = %q, want %q", platform, `"Windows"`)
	}
}

func TestParseClientHintsEdge(t *testing.T) {
	brands, _, _, ok := parseClientHints(edgeWinUA)
	if !ok {
		t.Fatal("parseClientHints(edge) ok = false, want true")
	}
	if !strings.Contains(brands, "Microsoft Edge") {
		t.Fatalf("brands = %q, want Microsoft Edge entry", brands)
	}
}

func TestParseClientHintsAndroid(t *testing.T) {
	_, mobile, platform, ok := parseClientHints(androidUA)
	if !ok {
		t.Fatal("parseClientHints(android) ok = false, want true")
	}
	if mobile != "?1" {
		t.Fatalf("mobile = %q, want ?1", mobile)
	}
	if platform != `"Android"` {
		t.Fatalf("platform = %q, want %q", platform, `"Android"`)
	}
}

func TestParseClientHintsNonChrome(t *testing.T) {
	for _, ua := range []string{"", "Mozilla/4.0 (compatible; MSIE 6.0)", "curl/8.0"} {
		if _, _, _, ok := parseClientHints(ua); ok {
			t.Fatalf("parseClientHints(%q) ok = true, want false", ua)
		}
	}
}

func TestBrandOrderStable(t *testing.T) {
	a, _, _, _ := parseClientHints(chromeWinUA)
	b, _, _, _ := parseClientHints(chromeWinUA)
	if a != b {
		t.Fatalf("brands not deterministic: %q vs %q", a, b)
	}
}
