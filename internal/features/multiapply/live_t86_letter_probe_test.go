//go:build live

package multiapply

// T86 LIVE PROBE (manual, network + owner session + AI key required; never
// in the default gate):
//
//	go test -tags live -run TestLiveT86LetterLanguage -v ./internal/features/multiapply/
//
// Verifies the T86 letter path against the live hh + AI for vacancy
// 136803802 («Инженер по инфраструктуре», the vacancy from the owner's
// symptom log):
//  1. VacancyCard parses the nested vacancyFull card with the real title
//     (the letter line must never render «» again).
//  2. One cover_letter AI call on that card returns a non-empty letter
//     under the T86 policy (formal requirement gaps do not escalate).
//
// Read-only GETs plus ONE AI call; no popup POST, no apply. The decision
// ledger is a no-op: the owner's ai_decisions table stays untouched.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hhauto/internal/ai"
	"hhauto/internal/config"
	"hhauto/internal/hhapi"
	"hhauto/internal/profile"
	"hhauto/internal/store"
	"hhauto/internal/tlsfp"
)

const (
	t86Dir       = "/tmp/opencode/t86"
	t86VacancyID = int64(136803802)
	t86Title     = "Инженер по инфраструктуре"
)

// t86NoopLedger keeps the probe off the owner's ai_decisions table.
type t86NoopLedger struct{}

func (t86NoopLedger) Add(*ai.DecisionRecord) error       { return nil }
func (t86NoopLedger) SumTokensDay(string) (int, float64) { return 0, 0 }

func TestLiveT86LetterLanguage(t *testing.T) {
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acct config.Account
	for id, a := range cfg.Accounts {
		acct, _ = cfg.Accounts[id]
		_ = a
		break
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	if spec.UserAgent != "" {
		acct.UserAgent = spec.UserAgent
	}
	api, err := hhapi.NewClient(acct, cfg.API.Endpoints, nil, nil, hhapi.WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	db, err := store.Open(context.Background(), cfg.Storage.Path, store.Options{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer db.Close()

	// 1) The card: nested vacancyFull must carry the real title.
	card, err := api.VacancyCard(context.Background(), t86VacancyID)
	if err != nil {
		t.Fatalf("vacancy card: %v", err)
	}
	if card.Name != t86Title {
		t.Fatalf("card name = %q, want %q (empty titles were the «Письмо на проверку: «»» bug)", card.Name, t86Title)
	}
	t.Logf("card title OK: %q — employer %q, published %s", card.Name, card.EmployerName, card.PublishedAt)

	// 2) One cover_letter AI call under the T86 policy.
	providers := make(map[string]ai.Provider, len(cfg.AI.Providers))
	for id, p := range cfg.AI.Providers {
		var prov ai.Provider
		var perr error
		switch p.Dialect {
		case ai.DialectOpenAIChat:
			prov, perr = ai.NewOpenAIChatProvider(id, p)
		case ai.DialectOpenAIResponses:
			prov, perr = ai.NewOpenAIResponsesProvider(id, p)
		case ai.DialectAnthropicMessages:
			prov, perr = ai.NewAnthropicProvider(id, p)
		default:
			t.Fatalf("provider %s: unknown dialect %q", id, p.Dialect)
		}
		if perr != nil {
			t.Fatalf("provider %s: %v", id, perr)
		}
		providers[id] = prov
	}
	runner, err := ai.New(providers, cfg.AI.Models, cfg.AI.Tasks, t86NoopLedger{}, ai.NewMemoryCache(), slog.Default())
	if err != nil {
		t.Fatalf("ai runner: %v", err)
	}
	var accID string
	for id := range cfg.Accounts {
		accID = id
		break
	}
	prof, err := profile.LoadUserContext(context.Background(), *cfg, accID, db, api)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	raw, dec, err := runner.Run(context.Background(), accID, "vacancy",
		fmt.Sprint(t86VacancyID), "t86-probe", ai.TaskCoverLetter, letterInput{
			Vacancy:      func() hhapi.Vacancy { v := *card; v.Raw = nil; v.Description = ""; return v }(),
			Details:      buildVacancyDetails(*card),
			ResumeDigest: prof.ResumeDigest,
			ProfileText:  prof.ProfileText,
			Facts:        prof.Facts,
			LetterMode:   config.LetterModeAICompany,
		})
	if err != nil {
		t.Fatalf("cover letter call: %v", err)
	}
	var lv struct {
		Letter     string `json:"letter"`
		NeedsHuman bool   `json:"needs_human"`
		Injection  bool   `json:"injection_suspected"`
		Reason     string `json:"needs_human_reason"`
	}
	if jerr := json.Unmarshal(raw, &lv); jerr != nil {
		t.Fatalf("verdict: %v", jerr)
	}
	t.Logf("verdict: needs_human=%v injection=%v model=%s prompt=%s",
		lv.NeedsHuman, lv.Injection, dec.Model, dec.PromptVer)
	if lv.Injection {
		t.Fatalf("injection flagged on a benign vacancy card — investigate before shipping")
	}
	if strings.TrimSpace(lv.Letter) == "" {
		t.Fatalf("empty letter (needs_human=%v reason=%q) — T86 expects a written letter", lv.NeedsHuman, lv.Reason)
	}
	if err := os.MkdirAll(t86Dir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	dump := filepath.Join(t86Dir, "letter_136803802.txt")
	if werr := os.WriteFile(dump, []byte(lv.Letter), 0o600); werr != nil {
		t.Fatalf("dump: %v", werr)
	}
	t.Logf("LETTER OK (%d chars) → %s", len(lv.Letter), dump)
	t.Logf("letter: %s", lv.Letter)
}
