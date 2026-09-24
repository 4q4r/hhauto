// Package profile assembles the user-context facts available to the AI:
// the account resume snapshot (stored in the store, refreshed through the
// hhapi client write-through), the free-form profile.md text, and the
// accumulated manual Q&A facts from the review flow.
//
// Principle (owner decision #5): the AI answers ONLY from the resume
// snapshot + profile.md; anything missing is surfaced as a
// "answer manually or add it to profile.md" flag — never invented.
package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/store"
)

// DefaultResumeTTL is the maximum age of the resume snapshot in the store
// before LoadUserContext refreshes it through the API (write-through).
const DefaultResumeTTL = 24 * time.Hour

// Profile is the per-account fact source for AI prompts.
type Profile struct {
	AccountID    string
	ResumeDigest string
	ProfileText  string
	Facts        map[string]string
}

// LoadUserContext builds the Profile for accountID: a fresh-enough resume
// digest, the profile.md text (per-account override or global) and the
// stored facts. It uses DefaultResumeTTL, the real clock and the default
// slog logger.
func LoadUserContext(ctx context.Context, cfg config.Config, accountID string, db *store.DB, api hhapi.HHAPI) (Profile, error) {
	return loadUserContext(ctx, cfg, accountID, db, api, DefaultResumeTTL, time.Now, slog.Default())
}

// loadUserContext is the injectable core of LoadUserContext: ttl, clock and
// logger are parameters so tests can pin them.
func loadUserContext(ctx context.Context, cfg config.Config, accountID string, db *store.DB, api hhapi.HHAPI, ttl time.Duration, now func() time.Time, log *slog.Logger) (Profile, error) {
	if accountID == "" {
		return Profile{}, errors.New("profile: accountID is empty")
	}
	if db == nil {
		return Profile{}, errors.New("profile: db is nil")
	}

	digest, err := resumeDigest(ctx, accountID, db, api, ttl, now, log)
	if err != nil {
		return Profile{}, err
	}
	text, err := readProfileText(cfg, accountID, log)
	if err != nil {
		return Profile{}, err
	}

	facts := make(map[string]string)
	rows, err := db.ProfileFactsAll(ctx, accountID)
	if err != nil {
		return Profile{}, err
	}
	for _, f := range rows {
		facts[f.Question] = f.Answer
	}

	return Profile{
		AccountID:    accountID,
		ResumeDigest: digest,
		ProfileText:  text,
		Facts:        facts,
	}, nil
}

// resumeDigest returns a compact digest of the account resume snapshot.
// The snapshot is refreshed through the API (write-through) when the store
// is empty or any stored snapshot is older than ttl. On refresh failure a
// stale snapshot (if present) is used with a WARN: resume facts stay true
// even when stale, and an empty store fails loudly — there is nothing to
// fall back to.
func resumeDigest(ctx context.Context, accountID string, db *store.DB, api hhapi.HHAPI, ttl time.Duration, now func() time.Time, log *slog.Logger) (string, error) {
	rows, err := db.ResumesListByAccount(ctx, accountID)
	if err != nil {
		return "", err
	}
	if !snapshotFresh(rows, now(), ttl) {
		refreshed, err := refreshResumes(ctx, accountID, db, api, now, log)
		switch {
		case err == nil:
			rows = refreshed
		case len(rows) > 0:
			log.Warn("profile: resume refresh failed; using stale snapshot", "account", accountID, "err", err)
		default:
			return "", fmt.Errorf("profile: refresh resumes for account %s: %w", accountID, err)
		}
	}

	// Newest first, stable tie-break by hh id: the freshest resume leads
	// the digest regardless of store insertion order.
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].UpdatedAt.Equal(rows[j].UpdatedAt) {
			return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
		}
		return rows[i].HhResumeID < rows[j].HhResumeID
	})

	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.Raw) == "" {
			parts = append(parts, fallbackResumeLine(r))
			continue
		}
		d, err := BuildResumeDigest(json.RawMessage(r.Raw))
		if err != nil {
			return "", fmt.Errorf("profile: digest resume %s of account %s: %w", r.HhResumeID, accountID, err)
		}
		if d != "" {
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// snapshotFresh reports whether every stored resume snapshot is present and
// within ttl of now (a zero updated_at counts as stale).
func snapshotFresh(rows []store.Resume, now time.Time, ttl time.Duration) bool {
	if len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if r.UpdatedAt.IsZero() || now.Sub(r.UpdatedAt) > ttl {
			return false
		}
	}
	return true
}

// refreshResumes replaces the store snapshot with api.ListResumes output
// (write-through) and returns the re-read store rows: the store is the
// single source of truth for the digest.
func refreshResumes(ctx context.Context, accountID string, db *store.DB, api hhapi.HHAPI, now func() time.Time, log *slog.Logger) ([]store.Resume, error) {
	if api == nil {
		return nil, errors.New("no resume snapshot in store and no API client to refresh")
	}
	got, err := api.ListResumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("api.ListResumes: %w", err)
	}
	for _, r := range got {
		if r.HHID == "" {
			log.Warn("profile: ListResumes returned resume without hh id; skipped", "account", accountID)
			continue
		}
		row := store.Resume{
			AccountID:  accountID,
			HhResumeID: r.HHID,
			Title:      r.Title,
			Status:     r.Status,
			Raw:        string(r.Raw),
			UpdatedAt:  r.UpdatedAt,
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = now()
		}
		if err := db.ResumesUpsert(ctx, &row); err != nil {
			return nil, err
		}
	}
	rows, err := db.ResumesListByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		log.Info("profile: account has no resumes after refresh", "account", accountID)
	}
	return rows, nil
}

// fallbackResumeLine renders a stored resume row whose raw JSON is missing:
// only the structured fields already in the store are surfaced as facts.
func fallbackResumeLine(r store.Resume) string {
	title := strings.TrimSpace(r.Title)
	status := strings.TrimSpace(r.Status)
	var b strings.Builder
	b.WriteString("Резюме")
	if title != "" {
		b.WriteString(" «" + title + "»")
	}
	if status != "" {
		b.WriteString(" (статус: " + status + ")")
	}
	b.WriteString(" (raw недоступен)")
	return b.String()
}

// profileFilePath resolves the profile.md path: the per-account override
// (accounts.<id>.profile_file) wins over the global data_dir/profile.md.
// An empty result means nothing is configured.
func profileFilePath(cfg config.Config, accountID string) string {
	if acc, ok := cfg.Accounts[accountID]; ok && acc.ProfileFile != "" {
		return acc.ProfileFile
	}
	if cfg.General.DataDir == "" {
		return ""
	}
	return filepath.Join(cfg.General.DataDir, "profile.md")
}

// readProfileText returns the profile.md content as-is (free-form Markdown
// in the user's language). A missing file is INFO + empty text, not an
// error; other read failures are real errors.
func readProfileText(cfg config.Config, accountID string, log *slog.Logger) (string, error) {
	path := profileFilePath(cfg, accountID)
	if path == "" {
		log.Info("profile: profile.md path is empty; using empty profile text", "account", accountID)
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Info("profile: profile.md not found; using empty profile text", "account", accountID, "path", path)
			return "", nil
		}
		return "", fmt.Errorf("profile: read %s: %w", path, err)
	}
	return string(b), nil
}
