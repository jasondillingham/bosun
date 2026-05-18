package claudehook

import (
	"testing"

	"github.com/jasondillingham/bosun/internal/config"
)

func TestLabelFromWorktreePath(t *testing.T) {
	cfg := config.Defaults()
	tests := []struct {
		name string
		repo string
		wt   string
		want string
	}{
		{"numeric session 3", "/code/myproj", "/code/myproj-bosun-3", "session-3"},
		{"numeric session 12", "/code/myproj", "/code/myproj-bosun-12", "session-12"},
		{"named session", "/code/myproj", "/code/myproj-bosun-auth", "auth"},
		{"main worktree", "/code/myproj", "/code/myproj", ""},
		{"main worktree (trailing slash)", "/code/myproj", "/code/myproj/", ""},
		{"sibling unrelated dir", "/code/myproj", "/code/otherproj", ""},
		{"basename prefix shared but no pattern", "/code/myproj", "/code/myproj-staging", ""},
		{"basename suffix prefix mismatch", "/code/myproj", "/code/myproj-bogus-3", ""},
		{"named with dashes", "/code/myproj", "/code/myproj-bosun-feature-x", "feature-x"},
		{"named with dot (sub-session)", "/code/myproj", "/code/myproj-bosun-session-1.auth", "session-1.auth"},
		{"empty suffix substitution", "/code/myproj", "/code/myproj-bosun-", ""},
		{"numeric zero invalid", "/code/myproj", "/code/myproj-bosun-0", ""},
		{"invalid named label", "/code/myproj", "/code/myproj-bosun-Bad_Label", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LabelFromWorktreePath(tc.repo, tc.wt, cfg)
			if err != nil {
				t.Fatalf("LabelFromWorktreePath: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLabelFromWorktreePath_CustomPattern(t *testing.T) {
	cfg := config.Defaults()
	cfg.WorktreeSuffixPattern = "_wt{N}"
	got, err := LabelFromWorktreePath("/code/myproj", "/code/myproj_wt5", cfg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "session-5" {
		t.Fatalf("got %q, want session-5", got)
	}
}

func TestLabelFromWorktreePath_InvalidPattern(t *testing.T) {
	cfg := config.Defaults()
	cfg.WorktreeSuffixPattern = "no-substitution-here"
	_, err := LabelFromWorktreePath("/code/myproj", "/code/myproj-anything", cfg)
	if err == nil {
		t.Fatal("expected error for pattern missing {N}")
	}
}
