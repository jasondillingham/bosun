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
