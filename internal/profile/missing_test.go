package profile

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDetectMissingVariants(t *testing.T) {
	cases := []struct {
		name        string
		input       json.RawMessage
		wantMissing []string
		wantFlags   []string
		wantErr     bool
	}{
		{
			name:        "missing_data and machine-code reason",
			input:       json.RawMessage(`{"score":0.8,"missing_data":["желаемая ЗП","уровень английского"],"needs_human_reason":"gray_band"}`),
			wantMissing: []string{"желаемая ЗП", "уровень английского"},
			wantFlags:   []string{"gray_band"},
		},
		{
			name:        "human-readable reason with spaces goes to missing",
			input:       json.RawMessage(`{"needs_human_reason":"вилка не указана ни в резюме, ни в профиле"}`),
			wantMissing: []string{"вилка не указана ни в резюме, ни в профиле"},
			wantFlags:   nil,
		},
		{
			name:        "pure machine code only",
			input:       json.RawMessage(`{"needs_human_reason":"budget_exhausted"}`),
			wantMissing: nil,
			wantFlags:   []string{"budget_exhausted"},
		},
		{
			name:        "real ai runner codes route to flags",
			input:       json.RawMessage(`{"needs_human_reason":"schema_invalid"}`),
			wantMissing: nil,
			wantFlags:   []string{"schema_invalid"},
		},
		{
			name:        "missing_data stays verbatim even when short (heuristic is reason-only)",
			input:       json.RawMessage(`{"missing_data":["формат","работы"]}`),
			wantMissing: []string{"формат", "работы"},
			wantFlags:   nil,
		},
		{
			name:        "mixed codes and phrases in reason",
			input:       json.RawMessage(`{"needs_human_reason":"injection_suspected"}`),
			wantMissing: nil,
			wantFlags:   []string{"injection_suspected"},
		},
		{
			name:        "neither field",
			input:       json.RawMessage(`{"score":0.95,"needs_human":false}`),
			wantMissing: nil,
			wantFlags:   nil,
		},
		{
			name:        "empty verdict object",
			input:       json.RawMessage(`{}`),
			wantMissing: nil,
			wantFlags:   nil,
		},
		{
			name:        "nil input is tolerated",
			input:       nil,
			wantMissing: nil,
			wantFlags:   nil,
		},
		{
			name:        "empty input is tolerated",
			input:       json.RawMessage(""),
			wantMissing: nil,
			wantFlags:   nil,
		},
		{
			name:        "dedupe and skip blanks in missing_data",
			input:       json.RawMessage(`{"missing_data":["a"," a ","","b"]}`),
			wantMissing: []string{"a", "b"},
			wantFlags:   nil,
		},
		{
			name:        "underscore-only token is a service flag",
			input:       json.RawMessage(`{"needs_human_reason":"offer_or_invitation"}`),
			wantMissing: nil,
			wantFlags:   []string{"offer_or_invitation"},
		},
		{
			name:        "digits or uppercase make it a phrase, not a code",
			input:       json.RawMessage(`{"needs_human_reason":"B2 подтвердите"}`),
			wantMissing: []string{"B2 подтвердите"},
			wantFlags:   nil,
		},
		{
			name:    "invalid json",
			input:   json.RawMessage(`{"missing_data": `),
			wantErr: true,
		},
		{
			name:    "wrong type for missing_data",
			input:   json.RawMessage(`{"missing_data":"not an array"}`),
			wantErr: true,
		},
		{
			name:    "wrong type for needs_human_reason",
			input:   json.RawMessage(`{"needs_human_reason":42}`),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotMissing, gotFlags, err := DetectMissing(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DetectMissing(%s) = (%v, %v, nil), want error", tc.input, gotMissing, gotFlags)
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectMissing(%s): %v", tc.input, err)
			}
			if len(gotMissing) == 0 && len(tc.wantMissing) == 0 {
				gotMissing = tc.wantMissing
			}
			if !reflect.DeepEqual(gotMissing, tc.wantMissing) {
				t.Fatalf("DetectMissing(%s) missing = %#v, want %#v", tc.input, gotMissing, tc.wantMissing)
			}
			if len(gotFlags) == 0 && len(tc.wantFlags) == 0 {
				gotFlags = tc.wantFlags
			}
			if !reflect.DeepEqual(gotFlags, tc.wantFlags) {
				t.Fatalf("DetectMissing(%s) flags = %#v, want %#v", tc.input, gotFlags, tc.wantFlags)
			}
		})
	}
}

func TestDetectMissingFlagsDoNotLeakIntoUserPrompts(t *testing.T) {
	// The original defect: a service code reached the user-facing prompt.
	missing, flags, err := DetectMissing(json.RawMessage(`{"missing_data":["Какая вилка?"],"needs_human_reason":"gray_band"}`))
	if err != nil {
		t.Fatalf("DetectMissing: %v", err)
	}
	prompts := MissingFactsPrompt(missing)
	if len(prompts) != 1 {
		t.Fatalf("prompts = %#v, want exactly one", prompts)
	}
	if strings.Contains(prompts[0], "gray_band") {
		t.Fatalf("service code leaked into user prompt: %q", prompts[0])
	}
	assertContains(t, prompts[0], "Какая вилка?")
	if !reflect.DeepEqual(flags, []string{"gray_band"}) {
		t.Fatalf("flags = %#v, want [gray_band]", flags)
	}
}

func TestMissingFactsPrompt(t *testing.T) {
	got := MissingFactsPrompt([]string{"Какая вилка?", "  ", "", "Какая вилка?", " Готов ли к релокации? "})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (dedupe + skip blanks + trim): %#v", len(got), got)
	}
	assertContains(t, got[0], "Какая вилка?", "вручную", "profile.md")
	assertContains(t, got[1], "Готов ли к релокации?")
}

func TestMissingFactsPromptEmpty(t *testing.T) {
	if got := MissingFactsPrompt(nil); len(got) != 0 {
		t.Fatalf("MissingFactsPrompt(nil) = %#v, want empty", got)
	}
	if got := MissingFactsPrompt([]string{"", "   "}); len(got) != 0 {
		t.Fatalf("blank-only input = %#v, want empty", got)
	}
}

func TestSuggestOnboardingQuestions(t *testing.T) {
	q := SuggestOnboardingQuestions()
	if len(q) != 6 {
		t.Fatalf("len = %d, want 6", len(q))
	}
	assertContains(t, strings.Join(q, "\n"),
		"рол",      // целевые роли
		"вилк",     // зарплатная вилка
		"запрещ",   // запретные темы
		"зык",      // языки
		"релокаци", // релокация
		"формат",   // формат работы
	)
	for i, item := range q {
		if strings.TrimSpace(item) == "" {
			t.Errorf("question %d is blank", i)
		}
	}

	// The caller must receive a copy: mutating the result must not leak.
	q[0] = "MUTATED"
	again := SuggestOnboardingQuestions()
	if again[0] == "MUTATED" {
		t.Fatal("SuggestOnboardingQuestions returned a shared slice, want a copy")
	}
}
