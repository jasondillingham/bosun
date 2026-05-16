package session

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jasondillingham/bosun/internal/config"
)

func TestParseName(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"session-1", 1, false},
		{"3", 3, false},
		{" session-12 ", 12, false},
		{"", 0, true},
		{"session-0", 0, true},
		{"session-x", 0, true},
		{"foo", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseName(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseName(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ParseName(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestWorktreePath(t *testing.T) {
	cfg := config.Defaults()
	root := filepath.Join(string(filepath.Separator)+"code", "myproj")
	got := WorktreePath(root, cfg, 3)
	want := filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-3")
	if got != want {
		t.Fatalf("WorktreePath = %q, want %q (runtime=%s)", got, want, runtime.GOOS)
	}
}

func TestWorktreePathForLabel(t *testing.T) {
	cfg := config.Defaults()
	root := filepath.Join(string(filepath.Separator)+"code", "myproj")
	got := WorktreePathForLabel(root, cfg, "auth")
	want := filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-auth")
	if got != want {
		t.Fatalf("WorktreePathForLabel(auth) = %q, want %q", got, want)
	}
	// Numeric and label form must agree for "session-N".
	if WorktreePath(root, cfg, 3) != WorktreePathForLabel(root, cfg, "session-3") {
		t.Errorf("WorktreePath wrapper drifted from WorktreePathForLabel")
	}
}

func TestParseLabel(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"session-1", "session-1", false},
		{"3", "session-3", false},
		{"auth", "auth", false},
		{"http-storage", "http-storage", false},
		{"session-12", "session-12", false},
		{"", "", true},
		{"   ", "", true}, // whitespace-only collapses to empty
		{"0", "", true},
		{"-1", "", true},                  // negative number
		{"Auth", "", true},                // uppercase
		{"AUTH", "", true},                // all caps
		{"café", "", true},                // unicode
		{"1auth", "", true},               // mixed digits/letters; must start with letter
		{"-auth", "", true},               // leading dash
		{"auth-", "", true},               // trailing dash
		{"auth--storage", "", true},       // consecutive dashes
		{"auth_storage", "", true},        // underscore not allowed
		{"auth!", "", true},               // bang
		{"session-", "", true},            // numeric-looking but no integer
		{"session-x", "session-x", false}, // bare label is valid charset; not a numeric session
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseLabel(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseLabel(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("ParseLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateLabel(t *testing.T) {
	good := []string{"auth", "http", "storage", "session-1", "a", "auth-2", "a1b2c3", "a-b-c"}
	// Bad list covers structural issues (empty, leading/trailing/consecutive
	// dashes, bare "session-"), case issues, and shell- or filesystem-hostile
	// characters that would tangle the derived `<repo>-bosun-<label>` path
	// or any shell call site downstream.
	bad := []string{
		"",                 // empty
		"Auth",             // uppercase
		"AUTH",             // all caps
		"1auth",            // starts with digit
		"-auth",            // leading dash
		"auth-",            // trailing dash
		"auth--storage",    // consecutive dashes
		"session-",         // trailing dash via "session-" prefix
		"auth!",            // disallowed punctuation
		"auth_storage",     // underscore
		"5",                // bare number (route through ParseLabel)
		"0",
		"Ωmega",            // non-ASCII letter — survives on macOS/Linux but tangles git-for-windows
		"café",             // accented — same concern
		"path/with",        // slash — would split into a subdir
		"path\\with",       // backslash — Windows separator
		"with space",       // would need quoting in every shell call site
		"with:colon",       // Windows drive separator
		"emoji-\U0001F600", // grinning face — outside the label charset
	}
	for _, s := range good {
		if err := ValidateLabel(s); err != nil {
			t.Errorf("ValidateLabel(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateLabel(s); err == nil {
			t.Errorf("ValidateLabel(%q) = nil, want error", s)
		}
	}
}

func TestIsNumericLabel(t *testing.T) {
	cases := map[string]bool{
		"session-1":  true,
		"session-12": true,
		"session-0":  false,
		"auth":       false,
		"session-x":  false,
		"":           false,
	}
	for in, want := range cases {
		if got := IsNumericLabel(in); got != want {
			t.Errorf("IsNumericLabel(%q) = %v, want %v", in, got, want)
		}
	}
}
