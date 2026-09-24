package profile

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDetectMissingVariants(t *testing.T) {
	cases := []struct {
		name    string
		input   json.RawMessage
		want    []string
		wantErr bool
	}{
		{
			name:  "missing_data and needs_human_reason",
			input: json.RawMessage(`{"score":0.8,"missing_data":["желаемая ЗП","уровень английского"],"needs_human_reason":"вилка не указана ни в резюме, ни в профиле"}`),
			want:  []string{"желаемая ЗП", "уровень английского", "вилка не указана ни в резюме, ни в профиле"},
		},
		{
			name:  "only missing_data",
			input: json.RawMessage(`{"missing_data":["формат работы"]}`),
			want:  []string{"формат работы"},
		},
		{
			name:  "only needs_human_reason",
			input: json.RawMessage(`{"needs_human":true,"needs_human_reason":"вопрос про запретные темы"}`),
			want:  []string{"вопрос про запретные темы"},
		},
		{
			name:  "neither field",
			input: json.RawMessage(`{"score":0.95,"needs_human":false}`),
			want:  nil,
		},
		{
			name:  "empty verdict object",
			input: json.RawMessage(`{}`),
			want:  nil,
		},
		{
			name:  "nil input is tolerated",
			input: nil,
			want:  nil,
		},
		{
			name:  "empty input is tolerated",
			input: json.RawMessage(""),
			want:  nil,
		},
		{
			name:  "dedupe and skip blanks",
			input: json.RawMessage(`{"missing_data":["a"," a ","","b"],"needs_human_reason":"b"}`),
			want:  []string{"a", "b"},
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
			got, err := DetectMissing(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DetectMissing(%s) = %v, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectMissing(%s): %v", tc.input, err)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DetectMissing(%s) = %#v, want %#v", tc.input, got, tc.want)
			}
		})
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
