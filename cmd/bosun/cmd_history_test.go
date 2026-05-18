package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasondillingham/bosun/internal/history"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// whatever was written. The cmd_history.go runners use `printf` from
// context.go which always writes to os.Stdout, so swapping that file
// descriptor is enough to inspect what they emit.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = prev })

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()

	_ = w.Close()
	<-done
	return buf.String()
}

// initBosunRepo creates a minimal git repo that loadCtx() will accept as
// the main worktree (one commit on `main`), and returns its path.
func initBosunRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "commit.gpgsign", "false")
	mustWrite(t, filepath.Join(dir, "README.md"), "hello\n")
	runGit("add", "README.md")
	runGit("commit", "-q", "-m", "init")
	return dir
}

// stageArchive seeds one archive directly via history.Archive so the
// runners have something to read. The archive's commits.log is supplied
// explicitly so we don't depend on the temp repo having a session branch.
func stageArchive(t *testing.T, repo, label string, ago time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-ago).Truncate(time.Second)
	if _, err := history.Archive(context.Background(), history.ArchiveInput{
		RepoRoot:   repo,
		Label:      label,
		Branch:     "bosun/" + label,
		EndReason:  history.ReasonCleanup,
		CommitsLog: "abc1234 " + label + " did the work\n",
		Now:        ts,
	}); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	// Match mtime to the staged timestamp so prune tests see it.
	dirName := ts.Format("20060102T150405Z") + "-" + label
	archDir := filepath.Join(repo, ".bosun", "history", dirName)
	past := time.Now().Add(-ago)
	if err := os.Chtimes(archDir, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// chdir changes to dir for the test and restores on cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

func TestRunHistoryList_EmptyAndPopulated(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)

	out := captureStdout(t, func() {
		if err := runHistoryList(); err != nil {
			t.Errorf("runHistoryList: %v", err)
		}
	})
	if !strings.Contains(out, "no archived history") {
		t.Errorf("empty-case output didn't mention 'no archived history': %q", out)
	}

	stageArchive(t, repo, "session-1", 1*time.Hour)
	stageArchive(t, repo, "session-2", 5*time.Minute)

	out = captureStdout(t, func() {
		if err := runHistoryList(); err != nil {
			t.Errorf("runHistoryList: %v", err)
		}
	})
	// Newest first: session-2 should appear before session-1.
	idx2 := strings.Index(out, "session-2")
	idx1 := strings.Index(out, "session-1")
	if idx2 < 0 || idx1 < 0 {
		t.Fatalf("missing session labels in output: %q", out)
	}
	if idx2 > idx1 {
		t.Errorf("session-2 should come before session-1 (newest first); got:\n%s", out)
	}
}

func TestRunHistoryShow_PrintsFilesAndMetadata(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	stageArchive(t, repo, "session-7", 30*time.Minute)

	out := captureStdout(t, func() {
		if err := runHistoryShow("session-7"); err != nil {
			t.Errorf("runHistoryShow: %v", err)
		}
	})
	for _, want := range []string{
		"archive:",
		"label:   session-7",
		"reason:  " + history.ReasonCleanup,
		"----- commits.log -----",
		"did the work",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunHistoryShow_NotFound(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	err := runHistoryShow("does-not-exist")
	if err == nil {
		t.Fatalf("expected error for unknown identifier")
	}
	if !strings.Contains(err.Error(), "no archive matching") {
		t.Errorf("got error %v; want 'no archive matching'", err)
	}
}

func TestRunHistoryGrep_FindsAcrossArchives(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	stageArchive(t, repo, "session-a", 2*time.Hour)
	stageArchive(t, repo, "session-b", 1*time.Hour)

	out := captureStdout(t, func() {
		if err := runHistoryGrep("did the work"); err != nil {
			t.Errorf("runHistoryGrep: %v", err)
		}
	})
	for _, want := range []string{"session-a", "session-b", "commits.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("grep output missing %q:\n%s", want, out)
		}
	}
}

func TestRunHistoryGrep_NoMatchesPrintsNothing(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	stageArchive(t, repo, "session-a", 1*time.Hour)

	out := captureStdout(t, func() {
		if err := runHistoryGrep("definitely-no-match-here-zzzzz"); err != nil {
			t.Errorf("runHistoryGrep: %v", err)
		}
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected empty output for no matches, got %q", out)
	}
}

func TestRunHistoryPrune_RemovesOldOnly(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	stageArchive(t, repo, "session-old", 40*24*time.Hour)
	stageArchive(t, repo, "session-fresh", 1*time.Hour)

	out := captureStdout(t, func() {
		if err := runHistoryPrune("30d"); err != nil {
			t.Errorf("runHistoryPrune: %v", err)
		}
	})
	if !strings.Contains(out, "session-old") {
		t.Errorf("prune output didn't list removed session-old: %s", out)
	}
	if strings.Contains(out, "session-fresh") {
		t.Errorf("prune output incorrectly listed session-fresh: %s", out)
	}

	// Verify on-disk state too.
	entries, err := history.List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Label != "session-fresh" {
		t.Errorf("after prune, want only session-fresh; got %+v", entries)
	}
}

func TestRunHistoryPrune_RejectsBadDuration(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	if err := runHistoryPrune("banana"); err == nil {
		t.Errorf("expected error for bad duration")
	}
}

func TestRunHistoryPrune_NoMatches(t *testing.T) {
	repo := initBosunRepo(t)
	chdir(t, repo)
	stageArchive(t, repo, "session-x", 1*time.Hour)

	out := captureStdout(t, func() {
		if err := runHistoryPrune("30d"); err != nil {
			t.Errorf("runHistoryPrune: %v", err)
		}
	})
	if !strings.Contains(out, "no archives older than") {
		t.Errorf("expected 'no archives older than' message; got %q", out)
	}
}
