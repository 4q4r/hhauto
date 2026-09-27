package ai

import "testing"

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{
			name:    "already clean JSON",
			content: `{"a":1}`,
			want:    `{"a":1}`,
			ok:      true,
		},
		{
			name:    "markdown json fence",
			content: "```json\n{\"a\":1}\n```",
			want:    `{"a":1}`,
			ok:      true,
		},
		{
			name:    "plain fence",
			content: "```\n{\"a\":1}\n```",
			want:    `{"a":1}`,
			ok:      true,
		},
		{
			name:    "prose before JSON",
			content: "Here is the result: {\"a\":1}",
			want:    `{"a":1}`,
			ok:      true,
		},
		{
			name:    "prose after JSON",
			content: `{"a":1} hope that helps`,
			want:    `{"a":1}`,
			ok:      true,
		},
		{
			name:    "nested objects extracted whole",
			content: "result: {\"outer\":{\"inner\":{\"deep\":2}}} end",
			want:    `{"outer":{"inner":{"deep":2}}}`,
			ok:      true,
		},
		{
			name:    "array",
			content: "items: [{\"a\":1},{\"b\":2}] done",
			want:    `[{"a":1},{"b":2}]`,
			ok:      true,
		},
		{
			name:    "non-JSON fence falls through to braces",
			content: "Here ``` x ``` you go: {\"a\":1}",
			want:    `{"a":1}`,
			ok:      true,
		},
		{
			name:    "invalid JSON only",
			content: "not json at all {broken",
			want:    "",
			ok:      false,
		},
		{
			name:    "empty string",
			content: "",
			want:    "",
			ok:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractJSON(tt.content)
			if ok != tt.ok {
				t.Fatalf("ExtractJSON() ok = %v, want %v", ok, tt.ok)
			}
			if string(got) != tt.want {
				t.Errorf("ExtractJSON() = %q, want %q", string(got), tt.want)
			}
		})
	}
}
