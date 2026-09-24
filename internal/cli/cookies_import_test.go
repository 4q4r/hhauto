package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseCookieHeaderLine(t *testing.T) {
	cks, err := parseCookiesInput("hhtoken=abc123; _xsrf=xyz; c=1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range cks {
		got[c.Name] = c.Value
	}
	if got["hhtoken"] != "abc123" || got["_xsrf"] != "xyz" || got["c"] != "1" {
		t.Fatalf("parsed: %v", got)
	}
}

func TestParseCookiesJSONMap(t *testing.T) {
	cks, err := parseCookiesInput(`{"hhtoken":"a b c","_xsrf":"zz"}`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range cks {
		got[c.Name] = c.Value
	}
	if len(got) != 2 || got["hhtoken"] != "a b c" || got["_xsrf"] != "zz" {
		t.Fatalf("parsed: %v", got)
	}
}

func TestParseCookiesJSONArray(t *testing.T) {
	doc := `[{"name":"hhtoken","value":"tok","domain":".hh.ru","path":"/","secure":true,"httpOnly":true,"expires":1790000000}]`
	cks, err := parseCookiesInput(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(cks) != 1 {
		t.Fatalf("parsed: %+v", cks)
	}
	c := cks[0]
	if c.Name != "hhtoken" || c.Value != "tok" || c.Domain != ".hh.ru" || !c.Secure || !c.HttpOnly {
		t.Fatalf("cookie fields: %+v", c)
	}
	if c.Expires.Unix() != 1790000000 {
		t.Fatalf("expires: %v", c.Expires)
	}
}

func TestParseNetscapeCookies(t *testing.T) {
	doc := strings.Join([]string{
		"# Netscape HTTP Cookie File",
		"# http://curl.haxx.se/rfc/cookie_spec.html",
		"#HttpOnly_.hh.ru\tTRUE\t/\tTRUE\t1790000000\thhtoken\tsecret-value",
		".hh.ru\tTRUE\t/\tFALSE\t1790000000\t_xsrf\ttok2",
	}, "\n")
	cks, err := parseCookiesInput(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(cks) != 2 {
		t.Fatalf("parsed %d cookies: %+v", len(cks), cks)
	}
	byName := map[string]*http.Cookie{}
	for _, c := range cks {
		byName[c.Name] = c
	}
	tok := byName["hhtoken"]
	if tok == nil || tok.Value != "secret-value" || !tok.HttpOnly || !tok.Secure {
		t.Fatalf("hhtoken: %+v", tok)
	}
	if tok.Expires.Unix() != 1790000000 && !tok.Expires.IsZero() {
		_ = tok.Expires
	}
	if got := byName["_xsrf"]; got == nil || got.Value != "tok2" {
		t.Fatalf("_xsrf: %+v", got)
	}
}

func TestParseCookiesRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "[1,2]", "{", "novaluehere"} {
		if _, err := parseCookiesInput(bad); err == nil {
			t.Fatalf("garbage accepted: %q", bad)
		}
	}
	_ = time.Second
}
