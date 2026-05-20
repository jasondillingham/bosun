package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo bootstraps a minimal git repo with a single commit on
// the named branch. Returns the repo root.
func initTestRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "--initial-branch="+branch)
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "Test")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-m", "init")
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestPreparePushable_CreatesBareAndPushes is the happy-path check:
// against a real temp git repo, calling PreparePushable creates the
// bare sibling at .bosun/remote/repo.git and the branch ref lands in
// it. Verifies the contract that an SSH-cloning remote container would
// actually find the branch.
func TestPreparePushable_CreatesBareAndPushes(t *testing.T) {
	repoRoot := initTestRepo(t, "main")

	uri, err := PreparePushable(repoRoot, "main")
	if err != nil {
		t.Fatalf("PreparePushable: %v", err)
	}
	if !strings.HasPrefix(uri, "ssh://") {
		t.Errorf("expected ssh:// URI, got %q", uri)
	}

	barePath := filepath.Join(repoRoot, ".bosun", "remote", "repo.git")
	if _, err := os.Stat(barePath); err != nil {
		t.Fatalf("bare repo not created at %s: %v", barePath, err)
	}

	// Verify the branch ref is present in the bare repo.
	cmd := exec.Command("git", "--git-dir", barePath, "rev-parse", "main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bare repo missing main ref: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Errorf("expected SHA for main in bare repo, got empty")
	}
}

// TestPreparePushable_Idempotent: a second call with the same args is
// a no-op shape (no error, same URI). Matters for restart / resume
// scenarios where bosun init re-runs and pushes again.
func TestPreparePushable_Idempotent(t *testing.T) {
	repoRoot := initTestRepo(t, "main")

	uri1, err := PreparePushable(repoRoot, "main")
	if err != nil {
		t.Fatalf("first PreparePushable: %v", err)
	}
	uri2, err := PreparePushable(repoRoot, "main")
	if err != nil {
		t.Fatalf("second PreparePushable: %v", err)
	}
	if uri1 != uri2 {
		t.Errorf("expected idempotent URI, got %q then %q", uri1, uri2)
	}
}

// TestPreparePushable_HonoursBosunRemoteOriginOverride: when
// BOSUN_REMOTE_ORIGIN is set, the returned URI is the override
// verbatim AND the session branch is pushed to that URI directly
// (skipping the local-bare-repo dance). Documented escape hatch
// for NAT-bound bosun hosts; the in-container clone needs the
// override URI to actually contain the branch, so PreparePushable
// has to push there.
func TestPreparePushable_HonoursBosunRemoteOriginOverride(t *testing.T) {
	repoRoot := initTestRepo(t, "main")

	// Stand up a separate bare repo as the "remote" target so the
	// override push has somewhere real to land. file:// URIs are
	// honoured by git push and side-step the SSH dependency.
	remoteBare := filepath.Join(t.TempDir(), "override.git")
	if _, err := runGit("", "init", "--bare", remoteBare); err != nil {
		t.Fatalf("init override bare: %v", err)
	}
	overrideURI := "file://" + remoteBare

	t.Setenv(remoteOriginEnv, overrideURI)

	uri, err := PreparePushable(repoRoot, "main")
	if err != nil {
		t.Fatalf("PreparePushable: %v", err)
	}
	if uri != overrideURI {
		t.Errorf("expected override URI %q, got %q", overrideURI, uri)
	}

	// Confirm the branch actually landed in the override bare repo —
	// the whole point of the push in the override path is that the
	// container's `git clone` finds the session branch waiting for it.
	if out, err := runGit(remoteBare, "branch", "--list", "main"); err != nil {
		t.Fatalf("list branches on override: %v\n%s", err, out)
	} else if !strings.Contains(out, "main") {
		t.Errorf("override bare missing the pushed branch:\n%s", out)
	}

	// And the local bare repo should NOT exist — when an override is
	// set, the local-bare-repo dance is skipped entirely.
	if _, err := os.Stat(filepath.Join(repoRoot, barePathRel)); err == nil {
		t.Errorf("local bare repo should NOT exist when BOSUN_REMOTE_ORIGIN is set")
	}
}

// TestPreparePushable_OverridePushFailureSurfaces: when the
// override URI doesn't accept pushes (unreachable host, wrong
// path, etc.), PreparePushable returns the wrapped git error
// rather than swallowing it. Operators see the misconfig at init
// time, not at the container's startup-clone step where the
// failure is harder to diagnose.
func TestPreparePushable_OverridePushFailureSurfaces(t *testing.T) {
	repoRoot := initTestRepo(t, "main")
	t.Setenv(remoteOriginEnv, "ssh://nonexistent.example.invalid:/no/such/path.git")

	_, err := PreparePushable(repoRoot, "main")
	if err == nil {
		t.Fatal("expected error pushing to bogus override URI, got nil")
	}
	if !strings.Contains(err.Error(), "BOSUN_REMOTE_ORIGIN") {
		t.Errorf("error should mention BOSUN_REMOTE_ORIGIN for operator clarity, got: %v", err)
	}
}

// TestPreparePushable_RejectsEmptyArgs surfaces operator-side bugs
// (forgetting to thread the branch through) before a nil-arg git
// command produces a confusing "bad refspec" error.
func TestPreparePushable_RejectsEmptyArgs(t *testing.T) {
	if _, err := PreparePushable("", "main"); err == nil {
		t.Errorf("expected error for empty repoRoot, got nil")
	}
	if _, err := PreparePushable(t.TempDir(), ""); err == nil {
		t.Errorf("expected error for empty branch, got nil")
	}
}

// TestComposeSSHURI_Auto: with no override, the URI is composed from
// $USER + os.Hostname() + abs bare path. Pinning the shape avoids
// silent drift in the auto-derivation logic.
func TestComposeSSHURI_Auto(t *testing.T) {
	t.Setenv(remoteOriginEnv, "")
	t.Setenv("USER", "alice")

	dir := t.TempDir()
	uri, err := composeSSHURI(filepath.Join(dir, "bare.git"))
	if err != nil {
		t.Fatalf("composeSSHURI: %v", err)
	}
	if !strings.HasPrefix(uri, "ssh://alice@") {
		t.Errorf("expected ssh://alice@... prefix, got %q", uri)
	}
	if !strings.Contains(uri, ":"+filepath.Join(dir, "bare.git")) {
		t.Errorf("expected URI to embed absolute bare path, got %q", uri)
	}
}

// TestComposeSSHURI_EmptyUserFallback: $USER unset is rare but happens
// (cron, systemd). The composed URI should omit the user@ prefix
// rather than emit `ssh://@host:…` which is invalid.
func TestComposeSSHURI_EmptyUserFallback(t *testing.T) {
	t.Setenv(remoteOriginEnv, "")
	t.Setenv("USER", "")

	dir := t.TempDir()
	uri, err := composeSSHURI(filepath.Join(dir, "bare.git"))
	if err != nil {
		t.Fatalf("composeSSHURI: %v", err)
	}
	if strings.Contains(uri, "@") {
		t.Errorf("expected URI without user prefix when $USER is empty, got %q", uri)
	}
	if !strings.HasPrefix(uri, "ssh://") {
		t.Errorf("expected ssh:// prefix, got %q", uri)
	}
}
