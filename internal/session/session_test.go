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
		{"0", "", true},
		{"Auth", "", true},                // uppercase
		{"1auth", "", true},               // mixed digits/letters; must start with letter
		{"-auth", "", true},               // leading dash
		{"auth!", "", true},               // bang
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
	good := []string{"auth", "http", "storage", "session-1", "a", "auth-2", "a1b2c3"}
	bad := []string{"", "Auth", "1auth", "-auth", "auth!", "auth_storage", "AUTH", "5", "0"}
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
