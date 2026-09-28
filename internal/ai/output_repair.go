package ai

// T41 output repair layer: GLM-5.3 (max reasoning) drifts JSON types even
// when the reply is fenced valid JSON — string confidence ("0.85"), null
// arrays (missing_data/used_facts), null booleans, string booleans
// ("true"), scalar-wrapped arrays, object-wrapped numbers
// ({"value":250000}) and extra or missing top-level keys. RepairOutput
// repairs that drift schema-driven BEFORE validation, so one provider call
// succeeds instead of burning re-asks and eventually synthesizing a
// needs_human verdict.
//
// Contract: repair is an optimization, never a trust boundary — the strict
// validator still runs after it, and everything it cannot repair (raw not
// JSON at all, structurally wrong values) falls through to the existing
// re-ask loop. Direction of every default is fail-closed where it matters:
// a dropped/unparseable confidence re-defaults to 0 and a missing
// needs_human re-defaults to true.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"hhauto/internal/ai/schemas"
)

// repairDefaults fills REQUIRED keys the model omitted entirely. The
// missing case is different from an explicit null: an omitted key means a
// broken response, so the conservative direction wins (confidence 0,
// needs_human true); an explicit null is repaired to the JSON-falsey
// default of the target type, matching how the downstream decoders
// (commonBool/commonFloat) already read such verdicts.
var repairDefaults = map[string]any{
	"confidence":          json.Number("0"), // fail-closed: low confidence routes to a human
	"needs_human":         true,             // fail-closed: a broken verdict parks for review
	"injection_suspected": false,
	"missing_data":        []any{},
	"used_facts":          []any{},
}

// repairProp is the repair-relevant projection of one schema property.
type repairProp struct {
	types   map[string]bool // JSON type names declared by the property
	minimum *float64
	maximum *float64
}

// repairSchema is the repair-relevant projection of one task schema.
type repairSchema struct {
	props    map[string]repairProp
	required map[string]bool
	// closed marks additionalProperties:false — extra keys break
	// validation there and must be stripped. Tolerant schemas
	// (resume_improve) let extras ride along (golden-pinned).
	closed bool
}

// repairSchemas caches the parsed projection per schema path (the embedded
// FS never changes at runtime).
var repairSchemas sync.Map // string → *repairSchema

// RepairOutput repairs the observed GLM type drift against the task schema
// named by schemaPath (e.g. "resume_improve.json", resolved from the
// embedded schema FS). It returns raw unchanged whenever repair is not
// applicable (raw is not a JSON object, or the schema cannot be loaded) —
// the validator and the re-ask loop stay the guards. On success it returns
// the re-marshaled object with:
//
//   - string numbers parsed ("0.85" → 0.85, clamped to the declared
//     minimum/maximum when both exist);
//   - null arrays → [], null booleans → false, string booleans → bool;
//   - scalar-wrapped arrays wrapped ([]any{scalar}; an empty string
//     becomes an empty array);
//   - object-wrapped numbers unwrapped via their .value/.amount field;
//   - top-level keys outside the schema stripped (only where the schema is
//     closed — tolerant schemas keep extras);
//   - omitted required keys from repairDefaults filled (confidence 0,
//     needs_human true, injection_suspected false, missing_data [],
//     used_facts []).
func RepairOutput(raw json.RawMessage, schemaPath string) json.RawMessage {
	schema := loadRepairSchema(schemaPath)
	if schema == nil {
		return raw
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || m == nil {
		return raw
	}
	for key, val := range m {
		prop, known := schema.props[key]
		if !known {
			if schema.closed {
				delete(m, key)
			}
			continue
		}
		if repaired, ok := repairValue(key, val, prop); ok {
			m[key] = repaired
		} else {
			delete(m, key)
		}
	}
	for key := range schema.required {
		if _, present := m[key]; !present {
			if def, has := repairDefaults[key]; has {
				m[key] = def
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// nullCoercedArrays lists array properties whose explicit null still
// coerces to [] — the schema tolerates GLM's null on the wire (T40), but
// the canonical verdict carries an array because every downstream
// consumer (post-checks, artifact slots, cached decisions) reads one.
// Optional value slots (value_strings) keep their null: it IS their
// canonical empty form.
var nullCoercedArrays = map[string]bool{"missing_data": true, "used_facts": true}

// repairValue converts one drifted value to a schema-conformant one.
// Conversion targets the CANONICAL type of the property, not merely an
// accepted one: the tolerant schema forms (string confidence, null
// missing_data) exist only to keep GLM's wire drift validatable, while
// the runner and consumers read numbers, booleans and arrays. ok reports
// whether the value survived repair at all; a value that cannot be
// repaired is dropped so the fail-closed defaults (or the re-ask loop,
// for keys without defaults) take over.
func repairValue(key string, val any, prop repairProp) (any, bool) {
	switch v := val.(type) {
	case nil:
		switch {
		case prop.types["array"] && nullCoercedArrays[key]:
			return []any{}, true
		case prop.types["null"]:
			return nil, true // nullable: null is already canonical
		case prop.types["boolean"]:
			return false, true // explicit null ≈ the falsey default (matches commonBool)
		case prop.types["array"]:
			return []any{}, true
		default:
			return nil, false
		}
	case bool:
		if prop.types["boolean"] {
			return v, true
		}
		return nil, false
	case string:
		return repairString(v, prop)
	case json.Number:
		if prop.types["number"] || (prop.types["integer"] && isIntegral(v)) {
			return v, true
		}
		if prop.types["string"] {
			return v.String(), true
		}
		return nil, false
	case []any:
		if prop.types["array"] {
			return v, true
		}
		return nil, false
	case map[string]any:
		return repairObject(v, prop)
	}
	return val, true
}

// repairString canonicalizes a string toward the property's strongest
// accepted type: number first (string confidence "0.85"), then array
// (scalar-wrapped missing_data/used_facts), then boolean ("true"),
// keeping the string only when string is the canonical destination
// (needs_human_reason, value_string).
func repairString(s string, prop repairProp) (any, bool) {
	switch {
	case prop.types["number"], prop.types["integer"]:
		trimmed := strings.TrimSpace(s)
		n, ok := parseClampedNumber(trimmed, prop)
		if !ok {
			return nil, false
		}
		if prop.types["integer"] && !prop.types["number"] && !isIntegral(n) {
			return nil, false
		}
		return n, true
	case prop.types["array"]:
		// An empty/blank string means "no items" (GLM's missing_data:"").
		if strings.TrimSpace(s) == "" {
			return []any{}, true
		}
		return []any{s}, true
	case prop.types["boolean"]:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return nil, false
	case prop.types["string"]:
		return s, true
	}
	return nil, false
}

// repairObject unwraps an object-wrapped number via its numeric .value or
// .amount field (GLM's value_amount:{"value":250000,"currency":"RUR"}).
func repairObject(o map[string]any, prop repairProp) (any, bool) {
	if prop.types["number"] || prop.types["integer"] {
		for _, key := range []string{"value", "amount"} {
			switch inner := o[key].(type) {
			case json.Number:
				if prop.types["integer"] && !prop.types["number"] && !isIntegral(inner) {
					continue
				}
				return inner, true
			case string:
				if n, ok := parseClampedNumber(strings.TrimSpace(inner), prop); ok {
					if prop.types["integer"] && !prop.types["number"] && !isIntegral(n) {
						continue
					}
					return n, true
				}
			}
		}
	}
	return nil, false
}

// parseClampedNumber parses s as a JSON number and clamps it into the
// declared [minimum,maximum] when both exist (the same policy as the
// string-confidence normalization: out-of-range collapses to the nearest
// bound instead of inventing a value).
func parseClampedNumber(s string, prop repairProp) (json.Number, bool) {
	if s == "" {
		return "", false
	}
	var f float64
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return "", false
	}
	if prop.minimum != nil && prop.maximum != nil {
		if f < *prop.minimum {
			f = *prop.minimum
		}
		if f > *prop.maximum {
			f = *prop.maximum
		}
		return json.Number(fmt.Sprintf("%v", f)), true
	}
	return json.Number(s), true
}

// isIntegral reports whether the JSON number has no fractional part.
func isIntegral(n json.Number) bool {
	if _, err := n.Int64(); err == nil {
		return true
	}
	f, err := n.Float64()
	return err == nil && f == float64(int64(f))
}

// loadRepairSchema parses (once) the repair projection of one embedded
// schema; nil means "no repair possible" (unknown path, unreadable or
// malformed schema — the validator remains the guard).
func loadRepairSchema(schemaPath string) *repairSchema {
	if cached, ok := repairSchemas.Load(schemaPath); ok {
		return cached.(*repairSchema)
	}
	schema := parseRepairSchema(schemaPath)
	repairSchemas.Store(schemaPath, schema)
	return schema
}

// repairTypes decodes a JSON Schema "type" that is either a plain string
// ("number") or an array (["number","string"]).
type repairTypes []string

func (t *repairTypes) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*t = repairTypes{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*t = many
	return nil
}

func parseRepairSchema(schemaPath string) *repairSchema {
	raw, err := schemas.FS.ReadFile(schemaPath)
	if err != nil {
		return nil
	}
	var doc struct {
		Required             []string `json:"required"`
		AdditionalProperties *bool    `json:"additionalProperties"`
		Properties           map[string]struct {
			Type    repairTypes `json:"type"`
			Minimum *float64    `json:"minimum"`
			Maximum *float64    `json:"maximum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	rs := &repairSchema{
		props:    make(map[string]repairProp, len(doc.Properties)),
		required: make(map[string]bool, len(doc.Required)),
		closed:   doc.AdditionalProperties != nil && !*doc.AdditionalProperties,
	}
	for _, name := range doc.Required {
		rs.required[name] = true
	}
	for name, p := range doc.Properties {
		types := make(map[string]bool, len(p.Type))
		for _, t := range p.Type {
			types[t] = true
		}
		rs.props[name] = repairProp{types: types, minimum: p.Minimum, maximum: p.Maximum}
	}
	return rs
}
