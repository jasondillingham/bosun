package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// CheckRepoWriteable verifies bosun can write to the repository root.
// A read-only mount or a tightened umask would make `bosun init` fail
// with a confusing mid-loop error; we'd rather surface it up front.
func CheckRepoWriteable(_ context.Context, repoRoot string) Result {
	return checkDirWriteable(repoRoot, "repo-writeable", "repository root")
}

// CheckBosunDirWriteable verifies .bosun/ exists (or can be created)
// and is writable. The state + claims + briefs subdirs all hang off
// .bosun/; if it's not writable bosun can't record anything.
func CheckBosunDirWriteable(_ context.Context, repoRoot string) Result {
	bosunDir := filepath.Join(repoRoot, ".bosun")
	if err := os.MkdirAll(bosunDir, 0o755); err != nil {
		return Result{
			Name:    "bosun-dir-writeable",
			Status:  Fail,
			Message: fmt.Sprintf("cannot create %s: %v", bosunDir, err),
			Fix:     "check parent directory permissions",
		}
	}
	return checkDirWriteable(bosunDir, "bosun-dir-writeable", ".bosun directory")
}

// checkDirWriteable drops a sentinel file in dir, reads it back, then
// removes it. Any failure in that round-trip surfaces as the check's
// Fail. Sentinel name is deliberately conspicuous so an operator who
// finds it left behind (after a doctor crash) knows what dropped it.
func checkDirWriteable(dir, name, description string) Result {
	sentinel := filepath.Join(dir, ".bosun-doctor-write-probe")
	if err := os.WriteFile(sentinel, []byte("probe"), 0o644); err != nil {
		return Result{
			Name:    name,
			Status:  Fail,
			Message: fmt.Sprintf("%s is not writable: %v", description, err),
			Fix:     fmt.Sprintf("verify ownership and permissions on %s", dir),
		}
	}
	defer func() { _ = os.Remove(sentinel) }()
	if _, err := os.ReadFile(sentinel); err != nil {
		return Result{
			Name:    name,
			Status:  Fail,
			Message: fmt.Sprintf("%s wrote a probe file but can't read it back: %v", description, err),
			Fix:     "filesystem may be in a degraded state",
		}
	}
	return Result{
		Name:    name,
		Status:  Pass,
		Message: fmt.Sprintf("%s is writable", description),
	}
}
