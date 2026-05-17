package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFsckWorktree_CleanRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fsck test uses POSIX shell setup")
	}
	dir := initFsckTestRepo(t)
	c := New()
	if err := c.FsckWorktree(context.Background(), dir); err != nil {
		t.Fatalf("FsckWorktree on clean repo: %v", err)
	}
}

func TestFsckWorktree_CorruptLooseObject(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fsck test uses POSIX shell setup")
	}
	dir := initFsckTestRepo(t)

	// Walk .git/objects/<xx>/ to find a loose object and overwrite it
	// with garbage. Loose objects live under a 2-char prefix dir; pack
	// files live under .git/objects/pack/ and don't fit our match.
	objRoot := filepath.Join(dir, ".git", "objects")
	var victim string
	_ = filepath.WalkDir(objRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || victim != "" {
			return nil
		}
		parent := filepath.Base(filepath.Dir(p))
		if len(parent) == 2 {
			victim = p
		}
		return nil
	})
	if victim == "" {
		t.Skip("no loose objects to corrupt (repo already packed)")
	}
	// Loose objects are created mode 0444 — bump u+w before we can write.
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatalf("chmod victim: %v", err)
	}
	if err := os.WriteFile(victim, []byte("not a real git object"), 0o644); err != nil {
		t.Fatalf("corrupt loose object: %v", err)
	}

	c := New()
	err := c.FsckWorktree(context.Background(), dir)
	if err == nil {
		t.Fatal("FsckWorktree on corrupted repo: want error, got nil")
	}
	var fe *FsckError
	if !errors.As(err, &fe) {
		t.Fatalf("FsckWorktree: want *FsckError, got %T (%v)", err, err)
	}
	if fe.Output == "" {
		t.Fatal("FsckError.Output should embed fsck's diagnostic text, got empty")
	}
}

func TestFsckWorktree_MissingDir(t *testing.T) {
	// Running fsck against a path that isn't a git repo should surface
	// as a non-nil error (so cmd_merge can refuse), but it doesn't need
	// to be an *FsckError — the failure mode is "fsck couldn't run",
	// not "fsck found corruption".
	c := New()
	err := c.FsckWorktree(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("FsckWorktree on missing dir: want error, got nil")
	}
}

// TestFsckWorktree_TimeoutFiresWithRecoveryHint is the v0.6.2 regression
// test: pre-fix, FsckWorktree was a free function that called
// exec.CommandContext directly with the caller's ctx, bypassing the
// Client's configured per-op timeout. Under fsync pressure that produced
// a multi-minute hang despite the operator's configured cap. The fix routes
// fsck through c.run so the timeout applies uniformly.
//
// Against pre-fix code this test fails to compile (FsckWorktree was a free
// function, not a Client method) — the compile failure IS the regression
// signal we want before Step 2's migration.
func TestFsckWorktree_TimeoutFiresWithRecoveryHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim uses POSIX sh; Windows skipped")
	}

	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "git")
	// Sleep forever on any invocation — the test should hit Client.Timeout
	// regardless of what subcommand fsck delegates to under the hood.
	shim := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	c := &Client{Runner: ExecRunner{}, Timeout: 2 * time.Second}

	start := time.Now()
	err := c.FsckWorktree(context.Background(), t.TempDir())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error from sleep-60s shim, got nil")
	}
	// 4s ceiling: 2s timeout + headroom for SIGKILL delivery + pipe drain.
	// A regression that drops the timeout would push this to ~60s.
	if elapsed > 4*time.Second {
		t.Fatalf("FsckWorktree returned in %v; expected ~2s — fsck timeout did not fire", elapsed)
	}
	var to *TimeoutError
	if !errors.As(err, &to) {
		t.Fatalf("error is not *TimeoutError: %v (type %T)", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "timed out") {
		t.Errorf("error message %q missing 'timed out'", msg)
	}
	// Operator-actionable recovery: fsck-timeout points at retry/cleanup,
	// not the worktree-add hint reused everywhere. Without this assertion
	// the same message would survive any future copy-paste regression.
	if !strings.Contains(msg, "bosun cleanup") {
		t.Errorf("error message must point at retry/cleanup recovery; got:\n  %s", msg)
	}
}

func initFsckTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustRunGit(t, dir, "init", "-q", "-b", "main")
	mustRunGit(t, dir, "config", "user.email", "test@example.com")
	mustRunGit(t, dir, "config", "user.name", "Test User")
	mustRunGit(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", "a.txt")
	mustRunGit(t, dir, "commit", "-q", "-m", "initial")
	// A second commit so corrupting any one loose blob still leaves
	// other objects intact for fsck to walk past.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, dir, "add", "b.txt")
	mustRunGit(t, dir, "commit", "-q", "-m", "second")
	return dir
}

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}
