// T57 key-skill level policy: the improve pipeline must leave NO key skill
// on «Уровень не указан» without inventing self-assessment. The rules, per
// the owner:
//
//   - a skill that ALREADY carries a level on hh is preserved byte-for-byte
//     — it never enters a levels PUT (no downgrades, no upgrades);
//   - a skill without a level gets the level the AI verdict proposes in
//     key_skill_levels (enum Продвинутый/Средний/Базовый, captured ids in
//     hhapi — see internal/hhapi/resume_keyskills.go);
//   - a refused/garbage/absent verdict level defaults to Средний;
//   - the flat legacy verdict (value_strings only) stays accepted — every
//     new skill then defaults to Средний.
//
// The level ids and the PUT contract were captured live on 2026-09-29
// (cloakbrowser, clone 4f48f357…): PUT
// /shards/applicant/key_skills/levels/update, level 8/9/10 = Базовый/
// Средний/Продвинутый.

package autoclone

import (
	"encoding/json"
	"strings"

	"hhauto/internal/hhapi"
)

// keySkillDefaultLevel is the fallback for refused/garbage/absent verdict
// levels (owner decision: never guess a self-assessment upward).
const keySkillDefaultLevel = "Средний"

// keySkillLevelVerdictEntry is one key_skill_levels verdict item.
type keySkillLevelVerdictEntry struct {
	Name  string `json:"name"`
	Level string `json:"level"`
}

// keySkillVerdictLevels extracts the optional key_skill_levels entries of a
// key_skills verdict into normalized-name → level-name. Backward
// compatible: a flat legacy verdict (no key_skill_levels, malformed array,
// non-string entries) yields an empty map — the caller then defaults every
// new skill to keySkillDefaultLevel. A level outside the captured enum is
// replaced by the default right here (garbage never travels further).
func keySkillVerdictLevels(verdict json.RawMessage) map[string]string {
	var out struct {
		Levels []keySkillLevelVerdictEntry `json:"key_skill_levels"`
	}
	if err := json.Unmarshal(verdict, &out); err != nil || len(out.Levels) == 0 {
		return nil
	}
	m := make(map[string]string, len(out.Levels))
	for _, e := range out.Levels {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			continue
		}
		level := strings.TrimSpace(e.Level)
		if _, ok := hhapi.KeySkillLevelID(level); !ok {
			level = keySkillDefaultLevel
		}
		m[skillNameKey(name)] = level
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// skillNameKey normalizes a skill name for verdict→resume matching (hh
// canonizes dictionary names: case and dash/space variants are equivalent,
// same surface as the applied-value comparison).
func skillNameKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	r := strings.NewReplacer("-", " ", "–", " ", "—", " ", "−", " ")
	s = r.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// keySkillLevelSets builds the levels PUT list under the preserve policy:
// every resume key skill WITHOUT a level (LevelID 0 = «Уровень не указан»)
// gets verdictLevels[name] or the default; skills that already carry a
// level are never sent. nil when there is nothing to set.
func keySkillLevelSets(skills []hhapi.UserSkill, verdictLevels map[string]string) []hhapi.KeySkillLevel {
	var sets []hhapi.KeySkillLevel
	for _, s := range skills {
		if s.LevelID != 0 {
			continue // already has a level on hh — preserve byte-for-byte
		}
		level := keySkillDefaultLevel
		if v, ok := verdictLevels[skillNameKey(s.Name)]; ok {
			level = v
		}
		id, ok := hhapi.KeySkillLevelID(level)
		if !ok {
			id, _ = hhapi.KeySkillLevelID(keySkillDefaultLevel)
		}
		sets = append(sets, hhapi.KeySkillLevel{SkillID: s.ID, LevelID: id})
	}
	return sets
}
