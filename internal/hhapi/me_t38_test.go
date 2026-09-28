package hhapi

// T38: hh changed the settings SSR state shape (live 2026-09-27): the user
// id moved to the top-level "hhid" node and the profile fields to
// "account"/"hhidAccount"; "session" no longer carries an id. Me must
// parse the new shape AND keep the legacy session/user shape working.
import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"hhauto/internal/config"
)

// The live 2026-09-27 ApplicantSettings shape (anonymized: ids/names are
// synthetic, structure mirrors the captured page).
const settingsT38Page = `<!DOCTYPE html><html><head></head><body>
<template style="display:none" id="HH-Lux-InitialState">{"account":{"firstName":"Тест","middleName":"Тестович","lastName":"Тестов","email":"user@example.ru","phone":"79990000000"},"applicantSettings":{"email":"user@example.ru"},"hhid":100500,"hhidAccount":{"firstName":"Тест","lastName":"Тестов"},"session":{"isFallback":false,"securityCheckFailed":false,"hasExtraAccounts":false,"isMultiAccount":false},"userType":"applicant","xsrfToken":"tok_t38"}</template>
</body></html>`

func TestMeParsesApplicantSettingsShapeT38(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /applicant/settings", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(settingsT38Page))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eps := epsCfg()
	eps.Site = srv.URL
	eps.Override = map[string]string{epSettings: srv.URL + "/applicant/settings"}
	c, err := NewClient(config.Account{DisplayName: "acc1", UserAgent: "ua/1"}, eps, nil, nil, WithBrowserTLSSpec(captureSpec(t)))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	u, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.ID != 100500 {
		t.Fatalf("ID = %d, want 100500 (top-level hhid)", u.ID)
	}
	if u.FullName != "Тест Тестов" {
		t.Fatalf("FullName = %q", u.FullName)
	}
	if u.Email != "user@example.ru" {
		t.Fatalf("Email = %q", u.Email)
	}
}
