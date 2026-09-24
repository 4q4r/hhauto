package cli

// Test helpers: a fake hh.ru (httptest) speaking the few shapes the wiring
// smoke needs, plus a temp hhauto.toml pointed at it.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const settingsPage = `<!doctype html><html><body><template id="HH-Lux-InitialState" data-name="HH-Lux-InitialState">{"xsrfToken":"test-xsrf","session":{"userId":4242,"email":"tester@example.org","fullName":"Тест Тестов"}}</template></body></html>`

const emptySearchPage = `<!doctype html><html><body><template id="HH-Lux-InitialState" data-name="HH-Lux-InitialState">{"vacancySearchResult":{"items":[],"pages":1,"found":0}}</template></body></html>`

// newFakeHH serves the minimal hh surface: settings SSR (xsrf+session),
// the resume list JSON, an empty chatik page and an empty search page.
func newFakeHH(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/applicant/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(settingsPage))
	})
	mux.HandleFunc("/shards/applicant/resumes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"resumes":[{"hash":"res-h1","title":"Go разработчик",` +
			`"updatedAt":"2026-09-24T10:00:00Z",` +
			`"status":{"id":"published","name":"Опубликовано"},` +
			`"_attributes":{"isSearchable":true,"percent":95,"status":{"id":"published","name":"Опубликовано"}}}]}`))
	})
	mux.HandleFunc("/chatik/api/chats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"chats":{"items":[]},"chatsDisplayInfo":{}}`))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(emptySearchPage))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// writeTestConfig materializes a hermetic config: data_dir/store/cookies
// under t.TempDir(), every endpoint on the fake hh, autolift+chatops+
// multiapply enabled with one run profile.
func writeTestConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := `
[general]
data_dir = "` + filepath.ToSlash(dir) + `"

[storage]
path = "` + filepath.ToSlash(filepath.Join(dir, "hhauto.db")) + `"

[log]
level = "error"
format = "text"
redact_secrets = true

[api.endpoints]
site = "` + testSite + `"
chatik = "` + testSite + `"
search_page = "` + testSite + `/search"

[accounts.main]
display_name = "Test"
user_agent = "hhauto-test-agent/1.0"

[features.auto_lift]
enabled = true
per_day = 6

[features.chatops]
enabled = true
poll_interval = "5m"

[features.multiapply]
enabled = true
ai_filter = false
min_match_percent = 60

[runs.testrun]
enabled = true
account = "main"
resume_id = "res-h1"
query = "golang"
max_per_day = 5

[[runs.testrun.windows]]
time = ["09:00", "14:00"]
days = ["mon", "tue", "wed", "thu", "fri"]
` + extra + "\n"
	path := filepath.Join(dir, "hhauto.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HHAUTO_COOKIES_BACKEND", "encrypted-file")
	t.Setenv("HHAUTO_VAULT_PASS", "test-vault-passphrase")
	return path
}

var testSite string // set by newFakeHHConfig

// newFakeHHConfig wires a fake hh and a config pointed at it; returns the
// config path and the data dir.
func newFakeHHConfig(t *testing.T) (configPath, dataDir string) {
	t.Helper()
	testSite = newFakeHH(t).URL
	path := writeTestConfig(t, "")
	return path, filepath.Dir(path)
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("output missing %q:\n%s", sub, s)
		}
	}
}
