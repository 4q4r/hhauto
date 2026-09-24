package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

var baseTime = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// fakeAPI satisfies hhapi.HHAPI; any method other than ListResumes panics
// via the embedded nil interface, so unexpected calls are loud.
type fakeAPI struct {
	hhapi.HHAPI
	resumes []hhapi.Resume
	err     error
	calls   int
}

func (f *fakeAPI) ListResumes(_ context.Context) ([]hhapi.Resume, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.resumes, nil
}

func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), store.Options{BusyTimeoutMS: 2000})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func testCfg(t *testing.T) config.Config {
	t.Helper()
	var cfg config.Config
	cfg.General.DataDir = t.TempDir()
	return cfg
}

func mustSeedAccount(t *testing.T, db *store.DB, id string) {
	t.Helper()
	if err := db.AccountsUpsert(context.Background(), store.Account{ID: id}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func seedResume(t *testing.T, db *store.DB, accountID, hash, title, raw string, updatedAt time.Time) {
	t.Helper()
	r := store.Resume{AccountID: accountID, HhResumeID: hash, Title: title, Status: "published", Raw: raw, UpdatedAt: updatedAt}
	if err := db.ResumesUpsert(context.Background(), &r); err != nil {
		t.Fatalf("seed resume: %v", err)
	}
}

const accID = "acc-1"

func TestLoadUserContextWriteThroughFetchesAndStores(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	raw := fixtureRaw(t)
	api := &fakeAPI{resumes: []hhapi.Resume{{
		HHID:      "hash-1",
		Title:     "Senior Go-разработчик",
		Status:    "published",
		UpdatedAt: baseTime.Add(-time.Hour),
		Raw:       raw,
	}}}
	cfg := testCfg(t)

	p, err := loadUserContext(context.Background(), cfg, accID, db, api, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}

	if api.calls != 1 {
		t.Fatalf("api calls = %d, want 1 (store was empty)", api.calls)
	}
	assertContains(t, p.ResumeDigest, "Senior Go-разработчик", "Зарплата: 350000 RUR")

	rows, err := db.ResumesListByAccount(context.Background(), accID)
	if err != nil {
		t.Fatalf("ResumesListByAccount: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("store rows = %d, want 1", len(rows))
	}
	if rows[0].HhResumeID != "hash-1" || rows[0].Raw != string(raw) {
		t.Fatalf("stored row = %+v, want raw write-through of hash-1", rows[0])
	}
	if !rows[0].UpdatedAt.Equal(baseTime.Add(-time.Hour)) {
		t.Fatalf("stored updated_at = %v, want %v", rows[0].UpdatedAt, baseTime.Add(-time.Hour))
	}
	if p.Facts == nil || len(p.Facts) != 0 {
		t.Fatalf("Facts = %#v, want empty non-nil map", p.Facts)
	}
}

func TestLoadUserContextFreshSnapshotSkipsAPI(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	raw := fixtureRaw(t)
	seedResume(t, db, accID, "hash-1", "Senior Go-разработчик", string(raw), baseTime.Add(-30*time.Minute))
	api := &fakeAPI{}
	cfg := testCfg(t)

	p, err := loadUserContext(context.Background(), cfg, accID, db, api, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	if api.calls != 0 {
		t.Fatalf("api calls = %d, want 0 (snapshot is fresh)", api.calls)
	}
	assertContains(t, p.ResumeDigest, "Senior Go-разработчик")
}

func TestLoadUserContextStaleSnapshotRefreshes(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	seedResume(t, db, accID, "hash-1", "Старое резюме", `{"title":"Старое резюме"}`, baseTime.Add(-2*time.Hour))

	newRaw := json.RawMessage(`{"title":"Свежее резюме","salary":{"amount":1,"currency":"RUR"}}`)
	api := &fakeAPI{resumes: []hhapi.Resume{{
		HHID:      "hash-1",
		Title:     "Свежее резюме",
		Status:    "published",
		UpdatedAt: baseTime.Add(-10 * time.Minute),
		Raw:       newRaw,
	}}}
	cfg := testCfg(t)

	p, err := loadUserContext(context.Background(), cfg, accID, db, api, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("api calls = %d, want 1 (snapshot was stale)", api.calls)
	}
	assertContains(t, p.ResumeDigest, "Свежее резюме")
	assertNotContains(t, p.ResumeDigest, "Старое резюме")

	rows, err := db.ResumesListByAccount(context.Background(), accID)
	if err != nil {
		t.Fatalf("ResumesListByAccount: %v", err)
	}
	if len(rows) != 1 || rows[0].Raw != string(newRaw) {
		t.Fatalf("store was not overwritten by write-through: %+v", rows)
	}
}

func TestLoadUserContextAPIErrorFallsBackToStale(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	seedResume(t, db, accID, "hash-1", "Старое резюме", `{"title":"Старое резюме"}`, baseTime.Add(-2*time.Hour))
	api := &fakeAPI{err: errors.New("boom")}
	cfg := testCfg(t)
	var logs bytes.Buffer

	p, err := loadUserContext(context.Background(), cfg, accID, db, api, time.Hour, func() time.Time { return baseTime }, testLogger(&logs))
	if err != nil {
		t.Fatalf("loadUserContext with stale fallback: %v", err)
	}
	assertContains(t, p.ResumeDigest, "Старое резюме")
	if !strings.Contains(logs.String(), "stale") {
		t.Fatalf("fallback must be logged as a warning, logs:\n%s", logs.String())
	}
}

func TestLoadUserContextAPIErrorWithoutDataFails(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	api := &fakeAPI{err: errors.New("boom")}
	cfg := testCfg(t)

	if _, err := loadUserContext(context.Background(), cfg, accID, db, api, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("loadUserContext = nil error, want error (no data and API failed)")
	}
}

func TestLoadUserContextNilAPIUsesStore(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	raw := fixtureRaw(t)
	seedResume(t, db, accID, "hash-1", "Senior Go-разработчик", string(raw), baseTime.Add(-time.Minute))
	cfg := testCfg(t)

	p, err := loadUserContext(context.Background(), cfg, accID, db, nil, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	assertContains(t, p.ResumeDigest, "Senior Go-разработчик")

	// Empty store and no API is a hard error: facts cannot be invented.
	db2 := newTestDB(t)
	mustSeedAccount(t, db2, accID)
	if _, err := loadUserContext(context.Background(), cfg, accID, db2, nil, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("nil API with empty store: want error")
	}
}

func TestLoadUserContextMultipleResumesNewestFirst(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	seedResume(t, db, accID, "hash-old", "Старое резюме", `{"title":"Старое резюме"}`, baseTime.Add(-3*time.Hour))
	seedResume(t, db, accID, "hash-new", "Новое резюме", `{"title":"Новое резюме"}`, baseTime.Add(-time.Hour))
	cfg := testCfg(t)

	p, err := loadUserContext(context.Background(), cfg, accID, db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	iNew := strings.Index(p.ResumeDigest, "Новое резюме")
	iOld := strings.Index(p.ResumeDigest, "Старое резюме")
	if iNew < 0 || iOld < 0 || iNew > iOld {
		t.Fatalf("newest resume must come first, digest:\n%s", p.ResumeDigest)
	}
}

func TestLoadUserContextSkipsResumeWithoutHash(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	api := &fakeAPI{resumes: []hhapi.Resume{
		{HHID: "", Title: "Без хеша", Raw: json.RawMessage(`{"title":"Без хеша"}`)},
		{HHID: "hash-2", Title: "Нормальное", UpdatedAt: baseTime, Raw: json.RawMessage(`{"title":"Нормальное"}`)},
	}}
	cfg := testCfg(t)
	var logs bytes.Buffer

	p, err := loadUserContext(context.Background(), cfg, accID, db, api, time.Hour, func() time.Time { return baseTime }, testLogger(&logs))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	assertNotContains(t, p.ResumeDigest, "Без хеша")
	assertContains(t, p.ResumeDigest, "Нормальное")
	rows, err := db.ResumesListByAccount(context.Background(), accID)
	if err != nil {
		t.Fatalf("ResumesListByAccount: %v", err)
	}
	if len(rows) != 1 || rows[0].HhResumeID != "hash-2" {
		t.Fatalf("store rows = %+v, want only hash-2", rows)
	}
	if !strings.Contains(logs.String(), "without hh id") {
		t.Fatalf("skipping a hashless resume must be logged, logs:\n%s", logs.String())
	}
}

func TestLoadUserContextCorruptedRawFails(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	seedResume(t, db, accID, "hash-1", "Битый raw", `{"title": `, baseTime)
	cfg := testCfg(t)

	if _, err := loadUserContext(context.Background(), cfg, accID, db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("corrupted stored raw must surface as an error, not as an empty digest")
	}
}

func TestLoadUserContextEmptyRawFallsBackToStoredFields(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	seedResume(t, db, accID, "hash-1", "Резюме без raw", "", baseTime)
	cfg := testCfg(t)

	p, err := loadUserContext(context.Background(), cfg, accID, db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	assertContains(t, p.ResumeDigest, "Резюме «Резюме без raw»", "raw недоступен")
}

func TestLoadUserContextGlobalProfileFile(t *testing.T) {
	cfg := testCfg(t)
	if err := os.WriteFile(filepath.Join(cfg.General.DataDir, "profile.md"), []byte("Глобальный профиль"), 0o600); err != nil {
		t.Fatalf("write profile.md: %v", err)
	}
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)

	p, err := loadUserContext(context.Background(), cfg, accID, db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	if p.ProfileText != "Глобальный профиль" {
		t.Fatalf("ProfileText = %q, want file content", p.ProfileText)
	}
}

func TestLoadUserContextMissingProfileFileIsInfoNotError(t *testing.T) {
	cfg := testCfg(t) // no profile.md inside
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	var logs bytes.Buffer

	p, err := loadUserContext(context.Background(), cfg, accID, db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&logs))
	if err != nil {
		t.Fatalf("missing profile.md must not fail: %v", err)
	}
	if p.ProfileText != "" {
		t.Fatalf("ProfileText = %q, want empty", p.ProfileText)
	}
	if !strings.Contains(logs.String(), "profile.md not found") {
		t.Fatalf("missing file must be logged at INFO, logs:\n%s", logs.String())
	}
}

func TestLoadUserContextProfileFileOverrideWins(t *testing.T) {
	cfg := testCfg(t)
	global := filepath.Join(cfg.General.DataDir, "profile.md")
	if err := os.WriteFile(global, []byte("Глобальный профиль"), 0o600); err != nil {
		t.Fatalf("write global profile.md: %v", err)
	}
	overridePath := filepath.Join(t.TempDir(), "custom-profile.md")
	if err := os.WriteFile(overridePath, []byte("OVERRIDE-профиль"), 0o600); err != nil {
		t.Fatalf("write override profile: %v", err)
	}
	cfg.Accounts = map[string]config.Account{accID: {ProfileFile: overridePath}}
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)

	p, err := loadUserContext(context.Background(), cfg, accID, db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	if p.ProfileText != "OVERRIDE-профиль" {
		t.Fatalf("ProfileText = %q, want override content", p.ProfileText)
	}
}

func TestLoadUserContextFactsRoundtrip(t *testing.T) {
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	ctx := context.Background()
	for q, a := range map[string]string{
		"Какая вилка?":    "от 250000 RUR",
		"Запретные темы?": "не обсуждать возраст",
	} {
		if err := db.ProfileFactsSet(ctx, accID, q, a); err != nil {
			t.Fatalf("ProfileFactsSet(%q): %v", q, err)
		}
	}
	cfg := testCfg(t)

	p, err := loadUserContext(ctx, cfg, accID, db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext: %v", err)
	}
	if len(p.Facts) != 2 {
		t.Fatalf("Facts = %#v, want 2 entries", p.Facts)
	}
	for q, a := range map[string]string{
		"Какая вилка?":    "от 250000 RUR",
		"Запретные темы?": "не обсуждать возраст",
	} {
		if p.Facts[q] != a {
			t.Fatalf("Facts[%q] = %q, want %q", q, p.Facts[q], a)
		}
	}

	// Facts are per-account: another account sees none.
	if err := db.ProfileFactsSet(ctx, "acc-2", "Какая вилка?", "другая"); err != nil {
		t.Fatalf("ProfileFactsSet(acc-2): %v", err)
	}
	p2, err := loadUserContext(ctx, cfg, "acc-2", db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{}))
	if err != nil {
		t.Fatalf("loadUserContext(acc-2): %v", err)
	}
	if len(p2.Facts) != 1 || p2.Facts["Какая вилка?"] != "другая" {
		t.Fatalf("acc-2 Facts = %#v, want its own single fact", p2.Facts)
	}
}

func TestLoadUserContextEmptyAccountID(t *testing.T) {
	db := newTestDB(t)
	cfg := testCfg(t)
	if _, err := loadUserContext(context.Background(), cfg, "", db, &fakeAPI{}, time.Hour, func() time.Time { return baseTime }, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("empty accountID must fail fast")
	}
}

func TestLoadUserContextExportedWrapper(t *testing.T) {
	// Smoke-check the exported entry point end-to-end with the real clock.
	db := newTestDB(t)
	mustSeedAccount(t, db, accID)
	api := &fakeAPI{resumes: []hhapi.Resume{{
		HHID:      "hash-1",
		Title:     "Senior Go-разработчик",
		Status:    "published",
		UpdatedAt: time.Now().Add(-time.Minute),
		Raw:       fixtureRaw(t),
	}}}
	var cfg config.Config
	cfg.General.DataDir = t.TempDir()

	p, err := LoadUserContext(context.Background(), cfg, accID, db, api)
	if err != nil {
		t.Fatalf("LoadUserContext: %v", err)
	}
	assertContains(t, p.ResumeDigest, "Senior Go-разработчик")
	if p.AccountID != accID {
		t.Fatalf("AccountID = %q, want %q", p.AccountID, accID)
	}
	if api.calls != 1 {
		t.Fatalf("api calls = %d, want 1", api.calls)
	}
}
