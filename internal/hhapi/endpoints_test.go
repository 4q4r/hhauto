package hhapi

import (
	"testing"
)

func TestNewEndpointSetDefaults(t *testing.T) {
	eps := newEndpointSet(epsCfg())
	if eps.site != "https://hh.ru" {
		t.Fatalf("site = %q, want https://hh.ru", eps.site)
	}
	if eps.chatik != "https://chatik.hh.ru" {
		t.Fatalf("chatik = %q, want https://chatik.hh.ru", eps.chatik)
	}
	if eps.searchPage != "https://hh.ru/search/vacancy" {
		t.Fatalf("searchPage = %q, want https://hh.ru/search/vacancy", eps.searchPage)
	}
}

func TestResolveOverride(t *testing.T) {
	eps := newEndpointSet(epsCfg())
	def := eps.site + "/shards/applicant/resumes"
	if got := eps.resolve(epResumes, def); got != "https://mirror.example.ru/shards/applicant/resumes" {
		t.Fatalf("resolve(resumes) = %q, want override URL", got)
	}
	if got := eps.resolve(epTouch, eps.site+"/applicant/resumes/touch"); got != eps.site+"/applicant/resumes/touch" {
		t.Fatalf("resolve(touch) = %q, want default (no override)", got)
	}
	if got := eps.resolve(epBootstrap, eps.site+"/shards/applicant/resumes"); got != eps.site+"/shards/applicant/resumes" {
		t.Fatalf("resolve(bootstrap) = %q, want default (no override)", got)
	}
}

func TestSameSite(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"hh.ru", "hh.ru", true},
		{"hh.ru", "taganrog.hh.ru", true},
		{"taganrog.hh.ru", "hh.ru", true},
		{"hh.ru", "evil.example.com", false},
		{"127.0.0.1:8080", "127.0.0.1:9090", true},
		{"", "hh.ru", false},
		{"hh.ru", "", false},
	}
	for _, c := range cases {
		if got := sameSite(c.a, c.b); got != c.want {
			t.Fatalf("sameSite(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
