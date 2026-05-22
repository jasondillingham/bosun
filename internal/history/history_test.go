package history

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// initGitRepo creates a temp git repo with one commit on a `bosun/<label>`
// branch so commits.log capture has something to log.
func initGitRepo(t *testing.T, label string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "initial commit")
	if label != "" {
		branch := "bosun/" + label
		run("checkout", "-q", "-b", branch)
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("session work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", "session-4: do the work")
	}
	return dir
}

func writeClaims(t *testing.T, repoRoot, label string, paths []string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".bosun", "claims")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := struct {
		Session   string    `json:"session"`
		Paths     []string  `json:"paths"`
		UpdatedAt time.Time `json:"updated_at"`
	}{label, paths, time.Now().UTC()}
	data, _ := json.MarshalIndent(body, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, label+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestArchive_CapturesBriefClaimsCommits(t *testing.T) {
	repo := initGitRepo(t, "session-4")
	worktree := repo // single-tree fixture is fine for these checks
	if err := os.WriteFile(filepath.Join(worktree, "BOSUN_BRIEF.md"), []byte("# brief for session-4\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeClaims(t, repo, "session-4", []string{"internal/history/"})

	ts := time.Date(2026, 5, 18, 20, 8, 23, 0, time.UTC)
	archDir, err := Archive(context.Background(), ArchiveInput{
		RepoRoot:     repo,
		Label:        "session-4",
		Branch:       "bosun/session-4",
		WorktreePath: worktree,
		EndReason:    ReasonMerged,
		MergeSHA:     "deadbeef",
		Detail:       "DONE",
		Now:          ts,
	})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	want := filepath.Join(repo, ".bosun/history/20260518T200823Z-session-4")
	if archDir != want {
		t.Fatalf("dir = %s, want %s", archDir, want)
	}
	for _, f := range []string{"brief.md", "claims.json", "commits.log", "merged.txt", "metadata.json"} {
		fi, err := os.Stat(filepath.Join(archDir, f))
		if err != nil {
			t.Errorf("missing %s: %v", f, err)
			continue
		}
		// History archives may contain operator-visible brief contents;
		// must be operator-only on multi-user hosts. (v0.12 M4 fix.)
		// Skip the mode-bit assertion on Windows — NTFS reports 0o666
		// regardless of what os.WriteFile / os.OpenFile passed, because
		// POSIX mode bits don't translate to the underlying ACL model.
		// Multi-user safety on Windows would need a Windows-specific
		// ACL grant via x/sys/windows/security; that's separate scope.
		if runtime.GOOS != "windows" {
			if mode := fi.Mode().Perm(); mode != 0o600 {
				t.Errorf("%s mode = %o, want 0600", f, mode)
			}
		}
	}
	commits, _ := os.ReadFile(filepath.Join(archDir, "commits.log"))
	if !strings.Contains(string(commits), "do the work") {
		t.Errorf("commits.log missing session commit; got %q", commits)
	}
	merged, _ := os.ReadFile(filepath.Join(archDir, "merged.txt"))
	if strings.TrimSpace(string(merged)) != "deadbeef" {
		t.Errorf("merged.txt = %q, want deadbeef", merged)
	}
	metaBytes, _ := os.ReadFile(filepath.Join(archDir, "metadata.json"))
	var meta Metadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if meta.Label != "session-4" || meta.EndReason != ReasonMerged || meta.MergeSHA != "deadbeef" {
		t.Errorf("metadata mismatch: %+v", meta)
	}
	if !meta.EndedAt.Equal(ts) {
		t.Errorf("ended_at = %v, want %v", meta.EndedAt, ts)
	}
}

func TestArchive_MissingFilesAreNotFatal(t *testing.T) {
	repo := t.TempDir() // no git, no brief, no claims
	archDir, err := Archive(context.Background(), ArchiveInput{
		RepoRoot:  repo,
		Label:     "session-1",
		EndReason: ReasonCleanup,
		// no Branch -> no git log attempt
		Now: time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archDir, "metadata.json")); err != nil {
		t.Errorf("metadata.json missing: %v", err)
	}
	for _, f := range []string{"brief.md", "claims.json", "commits.log", "merged.txt"} {
		if _, err := os.Stat(filepath.Join(archDir, f)); err == nil {
			t.Errorf("unexpected %s — no source for it existed", f)
		}
	}
}

func TestArchive_RequiresLabel(t *testing.T) {
	repo := t.TempDir()
	if _, err := Archive(context.Background(), ArchiveInput{RepoRoot: repo, EndReason: ReasonRemoved}); err == nil {
		t.Errorf("expected error for missing label")
	}
}

func TestArchive_CommitsLogOverride(t *testing.T) {
	repo := t.TempDir()
	override := "abc1234 manually provided log\n"
	archDir, err := Archive(context.Background(), ArchiveInput{
		RepoRoot:   repo,
		Label:      "session-4",
		EndReason:  ReasonMerged,
		CommitsLog: override,
		Now:        time.Date(2026, 5, 18, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(archDir, "commits.log"))
	if string(got) != override {
		t.Errorf("commits.log = %q, want %q", got, override)
	}
}

func TestList_SortsNewestFirst(t *testing.T) {
	repo := t.TempDir()
	mkArchive := func(ts time.Time, label string) {
		_, err := Archive(context.Background(), ArchiveInput{
			RepoRoot: repo, Label: label, EndReason: ReasonCleanup, Now: ts,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mkArchive(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), "session-1")
	mkArchive(time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC), "session-2")
	mkArchive(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC), "session-3")

	got, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	wantOrder := []string{"session-2", "session-3", "session-1"}
	for i, e := range got {
		if e.Label != wantOrder[i] {
			t.Errorf("entries[%d].Label = %s, want %s", i, e.Label, wantOrder[i])
		}
		if e.Metadata == nil {
			t.Errorf("entries[%d].Metadata is nil", i)
		}
	}
}

func TestList_NoHistoryDirReturnsNil(t *testing.T) {
	got, err := List(t.TempDir())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestLookup_ByLabelPicksNewest(t *testing.T) {
	repo := t.TempDir()
	mk := func(ts time.Time, label string) {
		if _, err := Archive(context.Background(), ArchiveInput{
			RepoRoot: repo, Label: label, EndReason: ReasonCleanup, Now: ts,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "session-1")
	mk(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), "session-1")
	mk(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "session-1")

	entry, _, err := Lookup(repo, "session-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !strings.HasPrefix(entry.DirName, "20260501") {
		t.Errorf("got dirName %q, want one starting 20260501", entry.DirName)
	}
}

func TestLookup_ByDirNameExact(t *testing.T) {
	repo := t.TempDir()
	if _, err := Archive(context.Background(), ArchiveInput{
		RepoRoot: repo, Label: "session-9", EndReason: ReasonCleanup,
		Now: time.Date(2026, 4, 2, 3, 4, 5, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	want := "20260402T030405Z-session-9"
	entry, _, err := Lookup(repo, want)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if entry.DirName != want {
		t.Errorf("got %s, want %s", entry.DirName, want)
	}
}

func TestLookup_AmbiguousPrefixReports(t *testing.T) {
	repo := t.TempDir()
	mk := func(ts time.Time, label string) {
		if _, err := Archive(context.Background(), ArchiveInput{
			RepoRoot: repo, Label: label, EndReason: ReasonCleanup, Now: ts,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(time.Date(2026, 5, 18, 1, 0, 0, 0, time.UTC), "session-a")
	mk(time.Date(2026, 5, 18, 2, 0, 0, 0, time.UTC), "session-b")

	entry, candidates, err := Lookup(repo, "20260518")
	if err == nil {
		t.Errorf("expected ambiguous error, got entry=%v", entry)
	}
	if len(candidates) != 2 {
		t.Errorf("expected 2 candidates, got %d", len(candidates))
	}
}

func TestGrep_FindsPatternAcrossFiles(t *testing.T) {
	repo := t.TempDir()
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "BOSUN_BRIEF.md"), []byte("# brief\nzephyr appears here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Archive(context.Background(), ArchiveInput{
		RepoRoot:     repo,
		Label:        "session-4",
		WorktreePath: wt,
		EndReason:    ReasonMerged,
		CommitsLog:   "abc1234 add a zephyr widget\n",
		Now:          time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := Grep(context.Background(), repo, "zephyr")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if len(hits) < 2 {
		t.Errorf("expected at least 2 hits across brief+commits, got %d: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if h.DirName == "" || h.File == "" || h.Line == 0 {
			t.Errorf("malformed hit: %+v", h)
		}
	}
}

func TestGrep_EmptyPatternReturnsNothing(t *testing.T) {
	repo := t.TempDir()
	_, _ = Archive(context.Background(), ArchiveInput{
		RepoRoot: repo, Label: "x", EndReason: ReasonCleanup,
		Now: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	})
	hits, err := Grep(context.Background(), repo, "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if hits != nil {
		t.Errorf("expected nil, got %v", hits)
	}
}

func TestGrep_NoHistoryDir(t *testing.T) {
	hits, err := Grep(context.Background(), t.TempDir(), "anything")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if hits != nil {
		t.Errorf("expected nil, got %v", hits)
	}
}

func TestPrune_RemovesOldArchives(t *testing.T) {
	repo := t.TempDir()
	mk := func(label string, ago time.Duration) string {
		ts := time.Now().UTC().Add(-ago).Truncate(time.Second)
		archDir, err := Archive(context.Background(), ArchiveInput{
			RepoRoot: repo, Label: label, EndReason: ReasonCleanup, Now: ts,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Set mtime so prune's mtime-based filter sees the staged age.
		past := time.Now().Add(-ago)
		if err := os.Chtimes(archDir, past, past); err != nil {
			t.Fatal(err)
		}
		return archDir
	}
	oldDir := mk("session-old", 40*24*time.Hour)
	freshDir := mk("session-fresh", 1*time.Hour)

	deleted, err := Prune(repo, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v, want 1", deleted)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("old dir survived prune")
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh dir was removed: %v", err)
	}
}

func TestPrune_RejectsZeroDuration(t *testing.T) {
	if _, err := Prune(t.TempDir(), 0); err == nil {
		t.Errorf("expected error for zero duration")
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"45m", 45 * time.Minute, false},
		{"", 0, true},
		{"banana", 0, true},
	}
	for _, c := range cases {
		got, err := ParseDuration(c.in)
		if (err != nil) != c.err {
			t.Errorf("%q: err = %v, want err=%v", c.in, err, c.err)
		}
		if err == nil && got != c.want {
			t.Errorf("%q: got %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseDirName(t *testing.T) {
	ts, label, ok := parseDirName("20260518T200823Z-session-4")
	if !ok {
		t.Fatalf("expected ok")
	}
	if label != "session-4" {
		t.Errorf("label = %s", label)
	}
	if ts.Year() != 2026 || ts.Month() != 5 || ts.Day() != 18 {
		t.Errorf("ts = %v", ts)
	}
	for _, bad := range []string{"", "session-4", "20260518T200823Z", "20260518T200823Z-", "garbage-session-4"} {
		if _, _, ok := parseDirName(bad); ok {
			t.Errorf("expected !ok for %q", bad)
		}
	}
}
