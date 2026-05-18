package debug

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantHas  []string // substrings that must appear in the output
		wantMiss []string // substrings that must NOT appear
	}{
		{
			name:     "json-style api_key",
			input:    `{"api_key": "sk-abc123def456"}`,
			wantHas:  []string{`"api_key": "<redacted>"`},
			wantMiss: []string{"sk-abc123def456"},
		},
		{
			name:     "env-style API_TOKEN",
			input:    `API_TOKEN=ghp_supersecret`,
			wantHas:  []string{`API_TOKEN=<redacted>`},
			wantMiss: []string{"ghp_supersecret"},
		},
		{
			name:     "yaml-style password with space",
			input:    `password : letmein`,
			wantHas:  []string{`<redacted>`},
			wantMiss: []string{"letmein"},
		},
		{
			name:     "secret with dash separator",
			input:    `secret-token = "hunter2"`,
			wantHas:  []string{`<redacted>`},
			wantMiss: []string{"hunter2"},
		},
		{
			name:     "single-quoted value",
			input:    `key = 'private-value'`,
			wantHas:  []string{`'<redacted>'`},
			wantMiss: []string{"private-value"},
		},
		{
			name:     "multiple secrets on different lines",
			input:    "api_key=abc\npassword=def\n",
			wantHas:  []string{"api_key=<redacted>", "password=<redacted>"},
			wantMiss: []string{"abc", "def"},
		},
		{
			name:     "non-secret key passes through",
			input:    `base_branch = "main"`,
			wantHas:  []string{`base_branch = "main"`},
			wantMiss: []string{"<redacted>"},
		},
		{
			name:     "key in larger sentence",
			input:    `Set api_key="value" in config.`,
			wantHas:  []string{`api_key="<redacted>"`, "Set ", " in config."},
			wantMiss: []string{`"value"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.input)
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("Redact(%q) = %q\n  want substring %q", tc.input, got, want)
				}
			}
			for _, miss := range tc.wantMiss {
				if strings.Contains(got, miss) {
					t.Errorf("Redact(%q) = %q\n  must NOT contain %q", tc.input, got, miss)
				}
			}
		})
	}
}

func TestRedactBriefFixture(t *testing.T) {
	// Mirrors the brief's `api_key = "sk-..."` example to lock in the
	// done-criteria explicitly.
	in := `api_key = "sk-1234567890"`
	got := Redact(in)
	if strings.Contains(got, "sk-1234567890") {
		t.Errorf("redaction failed: %q still contains secret value", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("redaction failed: %q has no <redacted> token", got)
	}
}

func TestRedactPreservesNonMatchingText(t *testing.T) {
	in := "no secrets here\njust prose\n"
	if got := Redact(in); got != in {
		t.Errorf("Redact mutated non-secret text: got %q want %q", got, in)
	}
}
