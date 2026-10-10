package cli

// T106 e2e: `account check` is a full-shard store sync. Rows whose hash is
// absent from the live shard are deleted-on-hh zombies and must leave the
// store (they surface as ghosts in `resume list`). The mine-shard is a
// single complete payload (no pagination contract), so every fetched hash
// survives the prune — however deep in the list — and audit rows that
// reference a deleted hash (review items) survive untouched.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/store"
)

// t106LiveHashes: twelve live resumes. The deep tail pins the
// no-pagination invariant: a page-sliced fetch would drop the last rows,
// a complete fetch must keep them all.
func t106LiveHashes() []string {
	out := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		out = append(out, fmt.Sprintf("live-%02d", i))
	}
	return out
}

func t106ShardBody() string {
	entries := make([]string, 0, 12)
	for i, h := range t106LiveHashes() {
		entries = append(entries, fmt.Sprintf(
			`{"hash":"%s","title":"Живое резюме %02d","updatedAt":"2026-10-09T10:00:00Z",`+
				`"status":{"id":"published","name":"Опубликовано"},`+
				`"_attributes":{"isSearchable":true,"percent":95,"status":{"id":"published","name":"Опубликовано"}}}`,
			h, i+1))
	}
	return `{"resumes":[` + strings.Join(entries, ",") + `]}`
}

func TestAccountCheckPrunesDeletedOnHHZombies(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/applicant/settings", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(settingsPage))
	})
	mux.HandleFunc("/shards/applicant/resumes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(t106ShardBody()))
	})
	mux.HandleFunc("/chatik/api/chats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chats":{"items":[]},"chatsDisplayInfo":{}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	testSite = srv.URL
	cfgPath := writeTestConfig(t, "")
	dataDir := filepath.Dir(cfgPath)

	// Pre-seed the store the command opens: stale live rows, three
	// deleted-on-hh zombies, and a review item referencing one zombie.
	seedDB, err := store.Open(context.Background(), filepath.Join(dataDir, "hhauto.db"), store.Options{BusyTimeoutMS: 2000})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	ctx := context.Background()
	if err := seedDB.AccountsUpsert(ctx, store.Account{ID: "main"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	stale := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	for i, h := range t106LiveHashes() {
		row := store.Resume{AccountID: "main", HhResumeID: h,
			Title: fmt.Sprintf("Старый титул %02d", i+1), Status: "published",
			Raw: `{}`, UpdatedAt: stale}
		if err := seedDB.ResumesUpsert(ctx, &row); err != nil {
			t.Fatalf("seed live: %v", err)
		}
	}
	for _, z := range []string{"zombie-1", "zombie-2", "zombie-3"} {
		row := store.Resume{AccountID: "main", HhResumeID: z,
			Title: "Удалённое на hh", Status: "published",
			Raw: `{}`, UpdatedAt: stale}
		if err := seedDB.ResumesUpsert(ctx, &row); err != nil {
			t.Fatalf("seed zombie: %v", err)
		}
	}
	if _, err := seedDB.ReviewItemsAdd(ctx, "main", "letter_needs_human", "seed", `{"resume":"zombie-1"}`); err != nil {
		t.Fatalf("seed review item: %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	out, err := runRoot(t, "account", "check", "main", "--config", cfgPath)
	if err != nil {
		t.Fatalf("account check: %v\n%s", err, out)
	}
	mustContain(t, out, "Сессия жива", "Резюме в store: 12")

	// The store mirrors the shard exactly: all twelve live hashes
	// (including the deep tail), zero zombies.
	db, err := store.Open(context.Background(), filepath.Join(dataDir, "hhauto.db"), store.Options{BusyTimeoutMS: 2000})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.ResumesListByAccount(ctx, "main")
	if err != nil {
		t.Fatalf("list store: %v", err)
	}
	got := make(map[string]store.Resume, len(rows))
	for _, r := range rows {
		got[r.HhResumeID] = r
	}
	for _, h := range t106LiveHashes() {
		if _, ok := got[h]; !ok {
			t.Fatalf("live resume %s missing after sync: store = %v", h, hashesOf(rows))
		}
	}
	for _, z := range []string{"zombie-1", "zombie-2", "zombie-3"} {
		if _, ok := got[z]; ok {
			t.Fatalf("zombie %s survived the sync: store = %v", z, hashesOf(rows))
		}
	}
	if len(rows) != 12 {
		t.Fatalf("store rows = %d, want 12 (%v)", len(rows), hashesOf(rows))
	}
	// Live rows were refreshed by the sync (write-through intact).
	if got["live-01"].Title != "Живое резюме 01" {
		t.Fatalf("live-01 title = %q, want refreshed from the shard", got["live-01"].Title)
	}
	// Audit rows survive the prune (bare-column doctrine, no FK to resumes).
	open, err := db.ReviewItemsOpen(ctx, "main", 100)
	if err != nil {
		t.Fatalf("review items: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("open review items = %d, want 1 (audit must survive prune)", len(open))
	}
	if !strings.Contains(open[0].Payload, "zombie-1") {
		t.Fatalf("review payload changed: %q", open[0].Payload)
	}

	// `resume list` shows no ghosts and keeps the live rows.
	listOut, err := runRoot(t, "resume", "list", "--config", cfgPath)
	if err != nil {
		t.Fatalf("resume list: %v\n%s", err, listOut)
	}
	if strings.Contains(listOut, "zombie-") {
		t.Fatalf("resume list still shows deleted-on-hh ghosts:\n%s", listOut)
	}
	mustContain(t, listOut, "live-01", "live-12")

	// Consumer contract: improve on a deleted hash fails honestly at the
	// store level (no panic, clear error).
	rt, err := Build(context.Background(), BuildOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer func() { _ = rt.Close() }()
	if _, _, err := resolveImproveTarget(ctx, rt, "zombie-1", ""); err == nil || !strings.Contains(err.Error(), "не найдено") {
		t.Fatalf("improve on deleted hash: err = %v, want honest not-found error", err)
	}
	// Regression: a live hash still resolves to its account.
	acc, isClone, err := resolveImproveTarget(ctx, rt, "live-01", "")
	if err != nil || acc != "main" || isClone {
		t.Fatalf("improve on live hash: (%q, %v, %v), want (main, false, nil)", acc, isClone, err)
	}
}

func hashesOf(rows []store.Resume) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.HhResumeID)
	}
	return out
}
