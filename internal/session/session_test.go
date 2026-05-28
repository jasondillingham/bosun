package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
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
	got := WorktreePath(root, cfg, 3, "")
	want := filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-3")
	if got != want {
		t.Fatalf("WorktreePath = %q, want %q (runtime=%s)", got, want, runtime.GOOS)
	}
	// Non-empty round timestamp produces the scheme-C UID-per-worktree form.
	gotTS := WorktreePath(root, cfg, 3, "20260518-115400")
	wantTS := filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-20260518-115400-3")
	if gotTS != wantTS {
		t.Fatalf("WorktreePath(ts) = %q, want %q", gotTS, wantTS)
	}
}

func TestWorktreePathForLabel(t *testing.T) {
	cfg := config.Defaults()
	root := filepath.Join(string(filepath.Separator)+"code", "myproj")
	got := WorktreePathForLabel(root, cfg, "auth", "")
	want := filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-auth")
	if got != want {
		t.Fatalf("WorktreePathForLabel(auth) = %q, want %q", got, want)
	}
	// Numeric and label form must agree for "session-N".
	if WorktreePath(root, cfg, 3, "") != WorktreePathForLabel(root, cfg, "session-3", "") {
		t.Errorf("WorktreePath wrapper drifted from WorktreePathForLabel")
	}
	// Same wrapper contract under a non-empty round timestamp.
	if WorktreePath(root, cfg, 3, "20260518-115400") != WorktreePathForLabel(root, cfg, "session-3", "20260518-115400") {
		t.Errorf("WorktreePath wrapper drifted from WorktreePathForLabel (with timestamp)")
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
		{"-1", "", true},            // negative number
		{"Auth", "", true},          // uppercase
		{"AUTH", "", true},          // all caps
		{"café", "", true},          // unicode
		{"1auth", "", true},         // mixed digits/letters; must start with letter
		{"-auth", "", true},         // leading dash
		{"auth-", "", true},         // trailing dash
		{"auth--storage", "", true}, // consecutive dashes
		{"auth_storage", "", true},  // underscore not allowed
		{"auth!", "", true},         // bang
		{"session-", "", true},      // numeric-looking but no integer
		{"session-x", "", true},     // Bughunt-1 F038: session- prefix is reserved for numeric sessions
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
		"",              // empty
		"Auth",          // uppercase
		"AUTH",          // all caps
		"1auth",         // starts with digit
		"-auth",         // leading dash
		"auth-",         // trailing dash
		"auth--storage", // consecutive dashes
		"session-",      // trailing dash via "session-" prefix
		"auth!",         // disallowed punctuation
		"auth_storage",  // underscore
		"5",             // bare number (route through ParseLabel)
		"0",
		"Ωmega",            // non-ASCII letter — survives on macOS/Linux but tangles git-for-windows
		"café",             // accented — same concern
		"path/with",        // slash — would split into a subdir
		"path\\with",       // backslash — Windows separator
		"with space",       // would need quoting in every shell call site
		"with:colon",       // Windows drive separator
		"emoji-\U0001F600", // grinning face — outside the label charset

		// Bughunt-1 F038: the `session-<word>` shape was silently
		// orphan-producing (init wrote it; list/status/show/remove
		// pretended it didn't exist). Reserve the prefix for numbered
		// sessions; reject everything else that wears it.
		"session-clean", // named with reserved prefix
		"session-foo",
		"session-FOO",     // (also fails labelRe casing — sanity check)
		"session-0",       // reserved prefix + zero is meaningless
		"session-1a",      // prefix + non-numeric tail
		"session-clean.x", // reserved prefix even when dotted
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

func TestLegacyWorktreePathForLabel(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"code", "myproj")
	cases := []struct {
		label string
		want  string
	}{
		{"session-3", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-3")},
		{"auth", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-auth")},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if got := LegacyWorktreePathForLabel(root, tc.label); got != tc.want {
				t.Fatalf("LegacyWorktreePathForLabel(%q) = %q, want %q", tc.label, got, tc.want)
			}
		})
	}
}

func TestIsLegacyWorktreePath(t *testing.T) {
	root := filepath.Join(string(filepath.Separator)+"code", "myproj")
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"legacy numeric", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-1"), true},
		{"legacy named", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-auth"), true},
		// New-shape `<timestamp>-<sub>` MUST NOT register as legacy — the
		// digits-then-dash discriminator is what keeps `bosun migrate`
		// idempotent (a second run finds nothing left to do).
		{"new-shape numeric", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-20260518143025-1"), false},
		{"new-shape named", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-20260518143025-auth"), false},
		{"repo root itself", root, false},
		{"unrelated sibling", filepath.Join(string(filepath.Separator)+"code", "myproj-other"), false},
		{"different parent dir", filepath.Join(string(filepath.Separator)+"elsewhere", "myproj-bosun-1"), false},
		{"empty suffix", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-"), false},
		// Hyphenated label without a digits prefix is still legitimate
		// (`http-storage` etc. — operator-chosen named session).
		{"hyphenated named", filepath.Join(string(filepath.Separator)+"code", "myproj-bosun-http-storage"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLegacyWorktreePath(root, tc.path); got != tc.want {
				t.Fatalf("IsLegacyWorktreePath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestResolveWorktreePath pins the read-only-compat contract: when the
// canonical path doesn't exist on disk but the legacy shape does, the
// resolver returns the legacy path. Once the naming-scheme lane lands
// (and WorktreePathForLabel starts producing the new shape), this is
// what keeps cmd_rescue/cmd_remove/etc. pointing at the right dir.
func TestResolveWorktreePath(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "myproj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()

	// With nothing on disk, the resolver returns the canonical path so
	// init can create at it.
	got := ResolveWorktreePath(repo, cfg, "session-1", "")
	if want := WorktreePathForLabel(repo, cfg, "session-1", ""); got != want {
		t.Fatalf("nothing on disk: got %q, want canonical %q", got, want)
	}

	// Create the legacy path on disk. ResolveWorktreePath should return
	// it when the canonical doesn't exist AND the legacy path differs
	// from the canonical (today it doesn't differ, so this exercises the
	// legacy-fallback branch by making the canonical absent).
	legacy := LegacyWorktreePathForLabel(repo, "session-1")
	canonical := WorktreePathForLabel(repo, cfg, "session-1", "")
	if legacy != canonical {
		if err := os.MkdirAll(legacy, 0o755); err != nil {
			t.Fatal(err)
		}
		got = ResolveWorktreePath(repo, cfg, "session-1", "")
		if got != legacy {
			t.Fatalf("legacy on disk: got %q, want legacy %q", got, legacy)
		}
	}

	// When the canonical path exists, it wins regardless of legacy.
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	got = ResolveWorktreePath(repo, cfg, "session-1", "")
	if got != canonical {
		t.Fatalf("canonical on disk: got %q, want canonical %q", got, canonical)
	}
}

// TestResolveWorktreePathByBranch_SchemeC is the Bughunt-1 F009 regression
// test: every session created by the canonical `bosun init` (scheme-C
// UID-per-worktree, the default since v0.11) sits at a timestamped
// directory like `<repo>-bosun-<timestamp>-<sub>`. The pre-fix gate
// computed `WorktreePathForLabel(.., "")` (legacy `<repo>-bosun-<sub>`
// shape) and never found the live worktree — `bosun_spawn` refused every
// default-init session with "no live agent detected." This test pins
// the by-branch resolver as the durable path that doesn't need the round
// timestamp threaded in.
func TestResolveWorktreePathByBranch_SchemeC(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "myproj")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q", "-b", "main")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "Test User")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "a.txt")
	mustGit(t, repo, "commit", "-q", "-m", "initial")

	// Create a scheme-C-shaped worktree: <repo>-bosun-<timestamp>-1
	// at a path that is NOT what WorktreePathForLabel(.., "") would
	// produce. The branch is the canonical bosun branch shape so
	// session.Derive / ResolveWorktreePathByBranch can both find it.
	cfg := config.Defaults()
	branch := cfg.BranchForLabel("session-1")
	schemeC := WorktreePathForLabel(repo, cfg, "session-1", "20260528-190606-12345")
	if schemeC == WorktreePathForLabel(repo, cfg, "session-1", "") {
		t.Fatalf("test bug: scheme-C path %q is identical to empty-timestamp path; the test depends on these differing", schemeC)
	}
	mustGit(t, repo, "worktree", "add", "-q", "-b", branch, schemeC)

	c := git.New()
	got, found, err := ResolveWorktreePathByBranch(context.Background(), c, repo, cfg, "session-1")
	if err != nil {
		t.Fatalf("ResolveWorktreePathByBranch: %v", err)
	}
	if !found {
		t.Fatalf("expected to find worktree for branch %q; got found=false", branch)
	}
	// On macOS t.TempDir() can sit under /var (a /private/var symlink),
	// so the path git reports may have the /private prefix git resolved
	// while schemeC retains the symlinked form. Compare via filepath.EvalSymlinks.
	if !samePath(got, schemeC) {
		t.Errorf("ResolveWorktreePathByBranch returned %q, want %q", got, schemeC)
	}

	// Negative case: a branch that doesn't exist returns ok=false, no error.
	_, found, err = ResolveWorktreePathByBranch(context.Background(), c, repo, cfg, "session-does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error for missing branch: %v", err)
	}
	if found {
		t.Errorf("expected found=false for missing branch")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ea, errA := filepath.EvalSymlinks(a)
	eb, errB := filepath.EvalSymlinks(b)
	if errA == nil && errB == nil && ea == eb {
		return true
	}
	return false
}

// TestValidateLabel_BughuntF038_SessionWordIsReserved pins the Bughunt-1
// F038 fix narrowly: `session-<word>` (the non-numeric "session-" form)
// must be refused with an error message that points the operator at the
// drop-the-prefix fix. Pre-fix init accepted these, created a worktree
// and branch, and Derive then silently excluded them from every read —
// producing orphan worktrees that doctor reported as healthy.
func TestValidateLabel_BughuntF038_SessionWordIsReserved(t *testing.T) {
	cases := []string{
		"session-clean",
		"session-foo",
		"session-1a",
		"session-clean.frontend", // dotted, but parent segment still non-numeric
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			err := ValidateLabel(s)
			if err == nil {
				t.Fatalf("ValidateLabel(%q) accepted the reserved-prefix form; want error", s)
			}
			// The fix message names the recovery path explicitly so an
			// operator who hit the bug knows what to do.
			msg := err.Error()
			for _, want := range []string{"session-", "reserved", "named sessions"} {
				if !strings.Contains(msg, want) {
					t.Errorf("ValidateLabel(%q) error missing %q\n  got: %s", s, want, msg)
				}
			}
		})
	}
}

// TestValidateLabel_NumberedAndNamedStillWork pins that the F038 gate
// doesn't accidentally over-reject — numbered sessions (session-1,
// session-1.auth) and bare named labels (auth, http) keep passing.
func TestValidateLabel_NumberedAndNamedStillWork(t *testing.T) {
	for _, s := range []string{
		"session-1",
		"session-42",
		"session-1.auth",
		"session-1.http-handler",
		"auth",
		"http",
		"frontend.backend", // dotted bare labels still legal
	} {
		t.Run(s, func(t *testing.T) {
			if err := ValidateLabel(s); err != nil {
				t.Errorf("ValidateLabel(%q) = %v, want nil", s, err)
			}
		})
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
