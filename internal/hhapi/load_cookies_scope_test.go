package hhapi

// T38 regression: a header-line cookie export carries no Domain. The
// cookies must be scoped to the registrable parent (.hh.ru) on load so
// hh's regional redirects (hh.ru → taganrog.hh.ru) keep the session
// flowing — a host-only hh.ru scope drops the session on the redirect and
// every SSR page answers the anonymous ForbiddenPage.
import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"hhauto/internal/config"
)

func TestLoadCookiesScopesHeaderLineCookiesToRegistrableParent(t *testing.T) {
	c, err := NewClient(config.Account{DisplayName: "acc1", UserAgent: "ua/1"}, config.APIEndpoints{Site: "https://hh.ru"}, nil, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	domainless := []*http.Cookie{
		{Name: "hhtoken", Value: "tok1"},
		{Name: "_xsrf", Value: "tok2"},
		{Name: "scoped", Value: "v3", Domain: "example.ru"}, // explicit domain untouched
	}
	if err := c.LoadCookies(context.Background(), domainless); err != nil {
		t.Fatalf("LoadCookies: %v", err)
	}
	_ = context.Background()

	// The regional subdomain must see the session cookies.
	regional := &url.URL{Scheme: "https", Host: "taganrog.hh.ru", Path: "/"}
	got := c.jar.Cookies(regional)
	names := map[string]bool{}
	for _, ck := range got {
		names[ck.Name] = true
	}
	if !names["hhtoken"] || !names["_xsrf"] {
		t.Fatalf("regional host sees %v — the session would drop on the regional redirect", names)
	}
	// The explicitly scoped cookie must NOT leak to hh hosts.
	if names["scoped"] {
		t.Fatal("an explicitly domain-scoped cookie leaked to the site host")
	}
	// The base host must still see everything.
	base := c.jar.Cookies(&url.URL{Scheme: "https", Host: "hh.ru", Path: "/"})
	baseNames := map[string]bool{}
	for _, ck := range base {
		baseNames[ck.Name] = true
	}
	if !baseNames["hhtoken"] || !baseNames["_xsrf"] {
		t.Fatalf("base host sees %v", baseNames)
	}
}
