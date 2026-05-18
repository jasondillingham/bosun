package claudehook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasondillingham/bosun/internal/config"
)

// capturedClaim records a single Claim invocation so a test can
// assert exactly what label / paths the handler tried to claim.
type capturedClaim struct {
	repoRoot string
	label    string
	paths    []string
}

// stubOpts builds a HandleOptions whose Resolve answers with the
// supplied roots, LoadConfig returns defaults, and Claim records the
// call into the returned pointer. Returns the options + the
// capture-pointer so tests can inspect what happened.
func stubOpts(t *testing.T, mainRoot, worktreeRoot string) (HandleOptions, *capturedClaim) {
	t.Helper()
	captured := &capturedClaim{}
	opts := HandleOptions{
		Resolve: func(cwd string) (string, string, error) {
			return mainRoot, worktreeRoot, nil
		},
		LoadConfig: func(_ string) (config.Config, error) {
			return config.Defaults(), nil
		},
		Claim: func(repoRoot, label string, paths []string) error {
			captured.repoRoot = repoRoot
			captured.label = label
			captured.paths = append([]string(nil), paths...)
			return nil
		},
	}
	return opts, captured
}

// makeTreeWithDotBosun makes <parent>/<name> with a `.bosun/` dir so
// hasBosunAncestor returns true for paths under it. Returns the
// freshly-created path.
func makeTreeWithDotBosun(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(dir, ".bosun"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

func TestHandle_EditClaimsFilePath(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")
	worktreeRoot := makeTreeWithDotBosun(t, tmp, "myproj-bosun-3")

	opts, got := stubOpts(t, mainRoot, worktreeRoot)

	payload := map[string]any{
		"hook_event_name": "PreToolUse",
		"cwd":             worktreeRoot,
		"tool_name":       "Edit",
		"tool_input": map[string]any{
			"file_path":  filepath.Join(worktreeRoot, "internal/foo.go"),
			"old_string": "x",
			"new_string": "y",
		},
	}
	data, _ := json.Marshal(payload)

	var stderr bytes.Buffer
	if err := HandlePreToolUse(bytes.NewReader(data), &stderr, opts); err != nil {
		t.Fatalf("HandlePreToolUse returned err (should always be nil): %v", err)
	}
	if got.label != "session-3" {
		t.Fatalf("label = %q, want session-3 (stderr: %s)", got.label, stderr.String())
	}
	if len(got.paths) != 1 || got.paths[0] != "internal/foo.go" {
		t.Fatalf("paths = %v, want [internal/foo.go]", got.paths)
	}
	if got.repoRoot != mainRoot {
		t.Fatalf("claim wrote to %q, want %q", got.repoRoot, mainRoot)
	}
}

func TestHandle_NotebookEditUsesNotebookPath(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")
	worktreeRoot := makeTreeWithDotBosun(t, tmp, "myproj-bosun-1")

	opts, got := stubOpts(t, mainRoot, worktreeRoot)
	payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"NotebookEdit","tool_input":{"notebook_path":%q}}`,
		worktreeRoot, filepath.Join(worktreeRoot, "notebooks/x.ipynb"))

	var stderr bytes.Buffer
	_ = HandlePreToolUse(strings.NewReader(payload), &stderr, opts)
	if got.label != "session-1" || len(got.paths) != 1 || got.paths[0] != "notebooks/x.ipynb" {
		t.Fatalf("unexpected claim label=%q paths=%v (stderr: %s)", got.label, got.paths, stderr.String())
	}
}

func TestHandle_WriteAndMultiEdit(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")
	worktreeRoot := makeTreeWithDotBosun(t, tmp, "myproj-bosun-2")

	for _, tool := range []string{"Write", "MultiEdit"} {
		t.Run(tool, func(t *testing.T) {
			opts, got := stubOpts(t, mainRoot, worktreeRoot)
			payload := fmt.Sprintf(`{"cwd":%q,"tool_name":%q,"tool_input":{"file_path":%q}}`,
				worktreeRoot, tool, filepath.Join(worktreeRoot, "x.go"))
			var stderr bytes.Buffer
			_ = HandlePreToolUse(strings.NewReader(payload), &stderr, opts)
			if got.label != "session-2" {
				t.Fatalf("label = %q for tool=%s (stderr: %s)", got.label, tool, stderr.String())
			}
			if len(got.paths) != 1 || got.paths[0] != "x.go" {
				t.Fatalf("paths = %v for tool=%s", got.paths, tool)
			}
		})
	}
}

func TestHandle_UnknownToolName(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")
	worktreeRoot := makeTreeWithDotBosun(t, tmp, "myproj-bosun-1")

	opts, got := stubOpts(t, mainRoot, worktreeRoot)
	payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"Bash","tool_input":{"command":"rm x"}}`, worktreeRoot)
	var stderr bytes.Buffer
	if err := HandlePreToolUse(strings.NewReader(payload), &stderr, opts); err != nil {
		t.Fatalf("expected nil err for unknown tool, got %v", err)
	}
	if got.label != "" {
		t.Fatalf("expected no claim for unknown tool, got label=%q", got.label)
	}
}

func TestHandle_MainWorktreeIsNoop(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")

	opts, got := stubOpts(t, mainRoot, mainRoot)
	payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":%q}}`,
		mainRoot, filepath.Join(mainRoot, "x.go"))

	var stderr bytes.Buffer
	_ = HandlePreToolUse(strings.NewReader(payload), &stderr, opts)
	if got.label != "" {
		t.Fatalf("expected no claim for main-worktree edit, got label=%q paths=%v", got.label, got.paths)
	}
}

func TestHandle_NotInBosunTree(t *testing.T) {
	tmp := t.TempDir()
	notBosun := filepath.Join(tmp, "elsewhere")
	if err := os.MkdirAll(notBosun, 0o755); err != nil {
		t.Fatal(err)
	}

	opts, got := stubOpts(t, "", "")
	// The Resolve func shouldn't even fire when the fast path
	// catches "no .bosun/ ancestor". Wire it to fail loudly so we
	// notice if the fast path regresses.
	opts.Resolve = func(string) (string, string, error) {
		return "", "", errors.New("should not reach git resolve")
	}
	payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":%q}}`,
		notBosun, filepath.Join(notBosun, "x.go"))

	var stderr bytes.Buffer
	if err := HandlePreToolUse(strings.NewReader(payload), &stderr, opts); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.label != "" {
		t.Fatalf("expected no claim for non-bosun tree, got %q", got.label)
	}
	if strings.Contains(stderr.String(), "should not reach") {
		t.Fatalf("fast path skipped, hit Resolve: %s", stderr.String())
	}
}

func TestHandle_EditOutsideWorktree(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")
	worktreeRoot := makeTreeWithDotBosun(t, tmp, "myproj-bosun-1")
	outside := filepath.Join(tmp, "outside.txt")

	opts, got := stubOpts(t, mainRoot, worktreeRoot)
	payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":%q}}`,
		worktreeRoot, outside)
	var stderr bytes.Buffer
	_ = HandlePreToolUse(strings.NewReader(payload), &stderr, opts)
	if got.label != "" {
		t.Fatalf("expected no claim for outside-worktree edit, got label=%q paths=%v", got.label, got.paths)
	}
}

func TestHandle_ParseErrorIsSilent(t *testing.T) {
	opts, got := stubOpts(t, "", "")
	var stderr bytes.Buffer
	if err := HandlePreToolUse(strings.NewReader("not json"), &stderr, opts); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.label != "" {
		t.Fatalf("got unexpected claim on parse error: %q", got.label)
	}
	if !strings.Contains(stderr.String(), "parse error") {
		t.Fatalf("expected parse-error log on stderr, got %q", stderr.String())
	}
}

func TestHandle_RelativeFilePath(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")
	worktreeRoot := makeTreeWithDotBosun(t, tmp, "myproj-bosun-5")

	opts, got := stubOpts(t, mainRoot, worktreeRoot)
	// Some agents emit relative paths; we should still resolve them.
	payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":"cmd/foo.go"}}`,
		worktreeRoot)
	var stderr bytes.Buffer
	_ = HandlePreToolUse(strings.NewReader(payload), &stderr, opts)
	if got.label != "session-5" || len(got.paths) != 1 || got.paths[0] != "cmd/foo.go" {
		t.Fatalf("got label=%q paths=%v (stderr: %s)", got.label, got.paths, stderr.String())
	}
}

func TestHandle_ClaimWriteFailureIsSilentToCaller(t *testing.T) {
	tmp := t.TempDir()
	mainRoot := makeTreeWithDotBosun(t, tmp, "myproj")
	worktreeRoot := makeTreeWithDotBosun(t, tmp, "myproj-bosun-1")

	opts, _ := stubOpts(t, mainRoot, worktreeRoot)
	opts.Claim = func(string, string, []string) error {
		return errors.New("boom")
	}
	payload := fmt.Sprintf(`{"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":"x.go"}}`,
		worktreeRoot)
	var stderr bytes.Buffer
	if err := HandlePreToolUse(strings.NewReader(payload), &stderr, opts); err != nil {
		t.Fatalf("HandlePreToolUse must never return non-nil; got %v", err)
	}
	if !strings.Contains(stderr.String(), "claim write failed") {
		t.Fatalf("expected claim-write-failure log, got %q", stderr.String())
	}
}
