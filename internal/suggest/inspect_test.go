package suggest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeRepo bootstraps a real git repo in a t.TempDir with the supplied
// files and a single commit. Returns the absolute repo root.
func fakeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "Test")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	writeFiles(t, dir, files)
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "--quiet", "-m", "initial")
	return dir
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestInspect_NonGitDirErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Inspect(dir); err == nil {
		t.Fatal("expected error inspecting non-git dir, got nil")
	}
}

func TestInspect_LanguageDetection(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		wantAny []string // languages that must be present
		wantNot []string // languages that must be absent
	}{
		{
			name:    "go only",
			files:   map[string]string{"go.mod": "module x\n\ngo 1.23\n", "main.go": "package main\n"},
			wantAny: []string{"go"},
			wantNot: []string{"node", "rust", "python"},
		},
		{
			name: "go + node",
			files: map[string]string{
				"go.mod":       "module x\n\ngo 1.23\n",
				"package.json": `{"name":"x","dependencies":{"lodash":"^4.0.0"}}`,
				"main.go":      "package main\n",
			},
			wantAny: []string{"go", "node"},
		},
		{
			name: "python via pyproject",
			files: map[string]string{
				"pyproject.toml": "[project]\nname='x'\n",
				"main.py":        "print('hi')\n",
			},
			wantAny: []string{"python"},
		},
		{
			name: "python via setup.py",
			files: map[string]string{
				"setup.py": "from setuptools import setup\nsetup()\n",
				"main.py":  "print('hi')\n",
			},
			wantAny: []string{"python"},
		},
		{
			name: "rust",
			files: map[string]string{
				"Cargo.toml": "[package]\nname='x'\nversion='0.1.0'\n",
				"src/lib.rs": "pub fn x() {}\n",
			},
			wantAny: []string{"rust"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := fakeRepo(t, tc.files)
			intel, err := Inspect(repo)
			if err != nil {
				t.Fatal(err)
			}
			set := map[string]bool{}
			for _, l := range intel.Languages {
				set[l] = true
			}
			for _, want := range tc.wantAny {
				if !set[want] {
					t.Errorf("language %q missing from %v", want, intel.Languages)
				}
			}
			for _, no := range tc.wantNot {
				if set[no] {
					t.Errorf("language %q should not be present in %v", no, intel.Languages)
				}
			}
			// stable order: alphabetical
			if !sort.StringsAreSorted(intel.Languages) {
				t.Errorf("languages not sorted: %v", intel.Languages)
			}
		})
	}
}

func TestInspect_FileCountAndHistogram(t *testing.T) {
	files := map[string]string{
		"go.mod":            "module x\n\ngo 1.23\n",
		"a.go":              "package x\n",
		"b.go":              "package x\n",
		"c.go":              "package x\n",
		"docs/readme.md":    "hi\n",
		"docs/CHANGES.md":   "hi\n",
		"Makefile":          "build:\n\techo ok\n",
		"internal/x/x.go":   "package x\n",
	}
	repo := fakeRepo(t, files)
	intel, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if intel.FileCount != len(files) {
		t.Errorf("FileCount = %d, want %d", intel.FileCount, len(files))
	}
	// Find .go bucket — should be 5 (a.go, b.go, c.go, internal/x/x.go) — wait, that's 4 .go files.
	// Actually: a.go, b.go, c.go, internal/x/x.go = 4. Plus go.mod which is ".mod".
	gotByExt := map[string]int{}
	for _, e := range intel.ExtensionHistogram {
		gotByExt[e.Ext] = e.Count
	}
	if gotByExt[".go"] != 4 {
		t.Errorf("histogram[.go] = %d, want 4", gotByExt[".go"])
	}
	if gotByExt[".md"] != 2 {
		t.Errorf("histogram[.md] = %d, want 2", gotByExt[".md"])
	}
	if gotByExt[".mod"] != 1 {
		t.Errorf("histogram[.mod] = %d, want 1", gotByExt[".mod"])
	}
	// "" for Makefile (no extension)
	if gotByExt[""] != 1 {
		t.Errorf("histogram[\"\"] = %d, want 1 (for Makefile)", gotByExt[""])
	}
}

func TestInspect_TopDirsSkipsHiddenAndVendor(t *testing.T) {
	files := map[string]string{
		"go.mod":                        "module x\n\ngo 1.23\n",
		"cmd/foo/main.go":               "package main\n",
		"cmd/bar/main.go":               "package main\n",
		"internal/a/a.go":               "package a\n",
		"internal/b/b.go":               "package b\n",
		"internal/c/c.go":               "package c\n",
		"docs/readme.md":                "hi\n",
		"vendor/dep/file.go":            "package dep\n",
		"node_modules/x/index.js":       "x\n",
		".github/workflows/ci.yml":      "ci\n",
		"top.go":                        "package x\n",
	}
	repo := fakeRepo(t, files)
	intel, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]int{}
	for _, d := range intel.TopDirs {
		dirs[d.Dir] = d.Count
	}
	if _, ok := dirs["vendor"]; ok {
		t.Error("vendor should be skipped from TopDirs")
	}
	if _, ok := dirs["node_modules"]; ok {
		t.Error("node_modules should be skipped from TopDirs")
	}
	if _, ok := dirs[".github"]; ok {
		t.Error(".github (dot-prefixed) should be skipped from TopDirs")
	}
	if dirs["internal"] != 3 {
		t.Errorf("internal count = %d, want 3", dirs["internal"])
	}
	if dirs["cmd"] != 2 {
		t.Errorf("cmd count = %d, want 2", dirs["cmd"])
	}
	// Top dirs are sorted by count desc.
	for i := 1; i < len(intel.TopDirs); i++ {
		if intel.TopDirs[i-1].Count < intel.TopDirs[i].Count {
			t.Errorf("TopDirs not sorted by count desc at %d: %+v", i, intel.TopDirs)
		}
	}
}

func TestInspect_RecentCommits(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main")
	mustGit(t, dir, "config", "user.email", "t@e.com")
	mustGit(t, dir, "config", "user.name", "T")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	for i := 1; i <= 5; i++ {
		writeFiles(t, dir, map[string]string{
			fmt.Sprintf("f%d.txt", i): fmt.Sprintf("v%d", i),
		})
		mustGit(t, dir, "add", ".")
		mustGit(t, dir, "commit", "--quiet", "-m", fmt.Sprintf("commit %d", i))
	}
	intel, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(intel.RecentCommits) != 5 {
		t.Fatalf("RecentCommits len = %d, want 5", len(intel.RecentCommits))
	}
	// git log default order is newest first.
	if intel.RecentCommits[0] != "commit 5" || intel.RecentCommits[4] != "commit 1" {
		t.Errorf("commit ordering wrong: %v", intel.RecentCommits)
	}
}

func TestInspect_DependenciesGoMod(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/x\n\n" +
			"go 1.23\n\n" +
			"require github.com/spf13/cobra v1.10.2\n\n" +
			"require (\n" +
			"\tgithub.com/charmbracelet/bubbletea v1.3.10\n" +
			"\tgolang.org/x/term v0.27.0 // indirect\n" +
			")\n",
		"main.go": "package main\nfunc main() {}\n",
	}
	repo := fakeRepo(t, files)
	intel, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"github.com/spf13/cobra",
		"github.com/charmbracelet/bubbletea",
		"golang.org/x/term",
	}
	for _, w := range want {
		found := false
		for _, d := range intel.Dependencies {
			if d == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("dep %q missing from %v", w, intel.Dependencies)
		}
	}
}

func TestInspect_DependenciesPackageJSON(t *testing.T) {
	files := map[string]string{
		"package.json": `{
			"name": "x",
			"dependencies": {"react": "^18.0.0", "lodash": "^4.0.0"},
			"devDependencies": {"jest": "^29.0.0"}
		}`,
		"index.js": "console.log('x')\n",
	}
	repo := fakeRepo(t, files)
	intel, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range intel.Dependencies {
		got[d] = true
	}
	for _, w := range []string{"react", "lodash", "jest"} {
		if !got[w] {
			t.Errorf("dep %q missing from %v", w, intel.Dependencies)
		}
	}
}

func TestInspect_TestLayoutHints(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "go co-located",
			files: map[string]string{
				"go.mod":     "module x\n\ngo 1.23\n",
				"foo.go":     "package x\n",
				"foo_test.go": "package x\n",
			},
			want: []string{"Go tests co-located"},
		},
		{
			name: "python tests dir",
			files: map[string]string{
				"pyproject.toml":   "[project]\nname='x'\n",
				"src/x.py":         "x = 1\n",
				"tests/test_x.py":  "def test_x(): pass\n",
			},
			want: []string{"Python-style tests dir"},
		},
		{
			name: "jest __tests__",
			files: map[string]string{
				"package.json":              `{"name":"x"}`,
				"src/foo.js":                "export {}\n",
				"src/__tests__/foo.test.js": "test()\n",
			},
			want: []string{"Jest-style __tests__/", "Co-located *.test.ts/js"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := fakeRepo(t, tc.files)
			intel, err := Inspect(repo)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, h := range intel.TestLayoutHints {
				got[h] = true
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("hint %q missing from %v", w, intel.TestLayoutHints)
				}
			}
		})
	}
}

func TestInspect_FileSampleDeterministic(t *testing.T) {
	// Build > fileSampleCap files; verify two Inspect calls return
	// the exact same FileSample (same HEAD → same seed).
	files := map[string]string{"go.mod": "module x\n\ngo 1.23\n"}
	for i := 0; i < 250; i++ {
		files[fmt.Sprintf("pkg%03d/f.go", i)] = "package x\n"
	}
	repo := fakeRepo(t, files)
	a, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.FileSample) != fileSampleCap {
		t.Fatalf("sample len = %d, want %d", len(a.FileSample), fileSampleCap)
	}
	if !reflect.DeepEqual(a.FileSample, b.FileSample) {
		t.Error("FileSample not deterministic across calls on same repo state")
	}
	// FileCount should be the full count, not the sample size.
	if a.FileCount != 251 {
		t.Errorf("FileCount = %d, want 251", a.FileCount)
	}
}

func TestInspect_FileSampleUnderCapReturnsAll(t *testing.T) {
	files := map[string]string{"go.mod": "module x\n\ngo 1.23\n"}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("f%d.go", i)] = "package x\n"
	}
	repo := fakeRepo(t, files)
	intel, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(intel.FileSample) != intel.FileCount {
		t.Errorf("FileSample len = %d, FileCount = %d — under-cap repos should sample everything",
			len(intel.FileSample), intel.FileCount)
	}
}

func TestInspect_RespectsByteBudget(t *testing.T) {
	// Make file paths long enough that the raw sample blows past 6KB.
	files := map[string]string{"go.mod": "module x\n\ngo 1.23\n"}
	longSeg := strings.Repeat("x", 60)
	for i := 0; i < 300; i++ {
		files[fmt.Sprintf("%s/%s/file%03d.go", longSeg, longSeg, i)] = "package x\n"
	}
	repo := fakeRepo(t, files)
	intel, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(intel)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > intelByteBudget {
		t.Errorf("serialized intel = %d bytes, exceeds budget %d", len(b), intelByteBudget)
	}
}

func TestInspect_DependencyCap(t *testing.T) {
	// Generate >50 deps to make sure capStrings trims.
	var depsBlock strings.Builder
	depsBlock.WriteString("module x\n\ngo 1.23\n\nrequire (\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&depsBlock, "\texample.com/dep%03d v1.0.0\n", i)
	}
	depsBlock.WriteString(")\n")
	files := map[string]string{
		"go.mod":  depsBlock.String(),
		"main.go": "package main\n",
	}
	repo := fakeRepo(t, files)
	intel, err := Inspect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(intel.Dependencies) > dependencyCap {
		t.Errorf("Dependencies len = %d, want ≤ %d", len(intel.Dependencies), dependencyCap)
	}
}

func TestInspect_EmptyRepoSurvives(t *testing.T) {
	// init a repo but never commit. ls-files returns nothing;
	// recentCommits errors and we swallow it.
	dir := t.TempDir()
	mustGit(t, dir, "init", "--quiet", "-b", "main")
	intel, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if intel.FileCount != 0 {
		t.Errorf("FileCount = %d, want 0", intel.FileCount)
	}
	if len(intel.RecentCommits) != 0 {
		t.Errorf("RecentCommits len = %d, want 0", len(intel.RecentCommits))
	}
	if len(intel.FileSample) != 0 {
		t.Errorf("FileSample len = %d, want 0", len(intel.FileSample))
	}
}

func TestExtensionHistogram_TieBreakAlphabetical(t *testing.T) {
	// Equal counts → alphabetical order is stable.
	got := extensionHistogram([]string{"a.go", "b.go", "c.md", "d.md"})
	if len(got) != 2 || got[0].Ext != ".go" || got[1].Ext != ".md" {
		t.Fatalf("unexpected histogram: %+v", got)
	}
}

func TestSampleFiles_PreservesInputOrder(t *testing.T) {
	in := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	got := sampleFiles(in, 4, "")
	// Sample is a subsequence — every entry of got must appear in in
	// in the same order.
	j := 0
	for _, x := range got {
		for j < len(in) && in[j] != x {
			j++
		}
		if j == len(in) {
			t.Fatalf("sample %v not in input order of %v", got, in)
		}
	}
}
