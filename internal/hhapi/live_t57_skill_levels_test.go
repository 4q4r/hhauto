//go:build live57

package hhapi

// T57 LIVE PROBE (manual, network + owner session required; never in the
// default gate):
//
//	go test -tags live57 -run TestLive57KeySkillsDump -count=1 -v ./internal/hhapi/
//
// Phase 1 (dump): read-only builder-state dump of the clone's keySkills
// segment BEFORE the levels fix — the before/after evidence for the level
// policy (existing levels preserved byte-for-byte, «Уровень не указан»
// filled). The dump lands in /tmp/opencode/t57-keyskills-before.json.
//
// T57 live rules: this probe touches ONLY the keySkills segment of the
// clone; another process owns the experience segment concurrently.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"hhauto/internal/config"
	"hhauto/internal/tlsfp"
)

const live57DefaultHash = "4f48f357ff112c2fcc0039ed1f6c7555414349"

// live57Client builds an authenticated client exactly like the live51
// probe (config + TLS fingerprint + plain cookie store).
func live57Client(t *testing.T) *Client {
	t.Helper()
	cfg, err := config.Load("/home/whoami/.config/hhauto/hhauto.toml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	var acct config.Account
	for _, a := range cfg.Accounts {
		acct = a
	}
	spec, err := tlsfp.Load(tlsfp.DefaultPath(cfg.General.DataDir))
	if err != nil {
		t.Fatalf("tls spec: %v", err)
	}
	if spec.UserAgent != "" {
		acct.UserAgent = spec.UserAgent
	}
	c, err := NewClient(acct, cfg.API.Endpoints, nil, nil, WithBrowserTLSSpec(spec))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	raw, err := os.ReadFile("/home/whoami/.local/share/hhauto/cookies/main.bin")
	if err != nil {
		t.Fatalf("cookie store: %v", err)
	}
	var blob struct {
		Cookies []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("cookie store decode: %v", err)
	}
	cks := make([]*http.Cookie, 0, len(blob.Cookies))
	for _, ck := range blob.Cookies {
		cks = append(cks, &http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := c.LoadCookies(ctx, cks); err != nil {
		t.Fatalf("load cookies: %v", err)
	}
	return c
}

func live57Hash() string {
	if h := os.Getenv("HHAUTO_T57_HASH"); h != "" {
		return h
	}
	return live57DefaultHash
}

func TestLive57KeySkillsDump(t *testing.T) {
	c := live57Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	st, err := c.fetchBuilderState(ctx, live57Hash())
	if err != nil {
		t.Fatalf("builder state: %v", err)
	}
	t.Logf("live state: title=%q screens=%v status=%q", st.Title(), st.Screens, st.Status())
	dump := map[string]json.RawMessage{}
	for k, v := range st.Resume {
		dump[k] = v
	}
	b, _ := json.MarshalIndent(dump, "", "  ")
	out := "/tmp/opencode/t57-keyskills-before.json"
	if err := os.WriteFile(out, b, 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	t.Logf("keySkills raw: %s", string(st.Resume["keySkills"]))
	t.Logf("dump written: %s (%d resume keys)", out, len(dump))
}

// TestLive57ApplySkillLevels runs the T57 targeted key_skills apply on the
// clone: set the captured Средний level (id 9) on the key skills that sit
// on «Уровень не указан» (owner spec for this clone: Go, HashiCorp Vault,
// CrowdSec — Go was already levelled by the capture save), then verify by
// re-read: no null levels remain and every pre-existing level is preserved
// byte-for-byte against /tmp/opencode/t57-keyskills-before.json +
// /tmp/opencode/t57-skilllevels-before.json.
//
//	go test -tags live57 -run TestLive57ApplySkillLevels -count=1 -v ./internal/hhapi/
func TestLive57ApplySkillLevels(t *testing.T) {
	c := live57Client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	hash := live57Hash()

	// The ДО state must exist (the dump ran first).
	beforeRaw, err := os.ReadFile("/tmp/opencode/t57-skilllevels-before.json")
	if err != nil {
		t.Fatalf("before dump: %v", err)
	}
	var before struct {
		Skills []struct {
			ID    int64  `json:"id"`
			Name  string `json:"name"`
			Level *int64 `json:"level"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(beforeRaw, &before); err != nil {
		t.Fatalf("before dump decode: %v", err)
	}
	keySkillsBefore, err := os.ReadFile("/tmp/opencode/t57-keyskills-before.json")
	if err != nil {
		t.Fatalf("before keySkills dump: %v", err)
	}

	skills, err := c.UserSkills(ctx, hash)
	if err != nil {
		t.Fatalf("UserSkills (live SSR parse): %v", err)
	}
	t.Logf("live UserSkills: %d skills", len(skills))
	for _, s := range skills {
		t.Logf("  skill id=%d level=%d name=%q", s.ID, s.LevelID, s.Name)
	}

	// Policy: null-level skills → Средний (owner spec); leveled → untouched.
	verdict := map[string]int64{} // explicit owner mapping for this fix
	for _, name := range []string{"Go", "HashiCorp Vault", "CrowdSec"} {
		id, ok := hhKeySkillLevelIDs["Средний"]
		if !ok {
			t.Fatalf("captured enum lost Средний")
		}
		verdict[name] = id
	}
	var sets []KeySkillLevel
	for _, s := range skills {
		if s.LevelID != 0 {
			continue // preserve byte-for-byte
		}
		lvl, ok := verdict[s.Name]
		if !ok {
			lvl = hhKeySkillLevelIDs["Средний"] // default policy
		}
		sets = append(sets, KeySkillLevel{SkillID: s.ID, LevelID: lvl})
	}
	t.Logf("sets: %+v", sets)
	if len(sets) == 0 {
		t.Log("nothing to set: no key skill sits on «Уровень не указан» (already fixed?)")
		return
	}
	if err := c.SetKeySkillLevels(ctx, hash, sets); err != nil {
		t.Fatalf("SetKeySkillLevels: %v", err)
	}
	t.Log("PUT accepted, verifying by re-read")

	after, err := c.UserSkills(ctx, hash)
	if err != nil {
		t.Fatalf("UserSkills after: %v", err)
	}
	byID := map[int64]int64{}
	for _, s := range after {
		byID[s.ID] = s.LevelID
	}
	want := map[int64]int64{}
	for _, s := range sets {
		want[s.SkillID] = s.LevelID
	}
	fails := 0
	for _, s := range after {
		if s.LevelID == 0 {
			t.Errorf("skill id=%d name=%q still on «Уровень не указан»", s.ID, s.Name)
			fails++
		}
		if w, ok := want[s.ID]; ok && s.LevelID != w {
			t.Errorf("skill id=%d name=%q level=%d, want %d", s.ID, s.Name, s.LevelID, w)
			fails++
		}
	}
	// Existing levels preserved byte-for-byte against the ДО dump.
	for _, b := range before.Skills {
		if b.Level == nil {
			continue
		}
		if byID[b.ID] != *b.Level {
			t.Errorf("existing level drifted: id=%d name=%q was %d now %d", b.ID, b.Name, *b.Level, byID[b.ID])
			fails++
		}
	}
	// The resume keySkills list itself is untouched by the levels PUT
	// (compared as a normalized set — hh canonizes and may reorder
	// dictionary-driven lists).
	st, err := c.fetchBuilderState(ctx, hash)
	if err != nil {
		t.Fatalf("builder state after: %v", err)
	}
	beforeList := conditionStringList(jsonKeySkills(keySkillsBefore))
	afterList := conditionStringList(st.Resume["keySkills"])
	if len(beforeList) != len(afterList) {
		t.Errorf("keySkills list size drifted: before %d after %d", len(beforeList), len(afterList))
		fails++
	}
	for _, b := range beforeList {
		found := false
		for _, a := range afterList {
			if normAppliedText(a) == normAppliedText(b) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("keySkills list lost %q", b)
			fails++
		}
	}
	if fails > 0 {
		t.Fatalf("%d verification failures", fails)
	}
	afterDump, _ := json.MarshalIndent(after, "", "  ")
	if err := os.WriteFile("/tmp/opencode/t57-skilllevels-after.json", afterDump, 0o600); err != nil {
		t.Fatalf("write after dump: %v", err)
	}
	t.Logf("AFTER: no «Уровень не указан» left; after-dump written (%d skills)", len(after))
}

// jsonKeySkills extracts the keySkills array from the before-dump JSON (it
// is the full resume-key map; the array bytes are compared canonically).
func jsonKeySkills(dumpRaw []byte) json.RawMessage {
	var dump map[string]json.RawMessage
	if err := json.Unmarshal(dumpRaw, &dump); err != nil {
		return nil
	}
	return dump["keySkills"]
}

// conditionStringList unwraps a condition-format string list (["Go",…]).
func conditionStringList(raw json.RawMessage) []string {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		var s string
		if json.Unmarshal(p["string"], &s) == nil {
			out = append(out, s)
		}
	}
	return out
}
