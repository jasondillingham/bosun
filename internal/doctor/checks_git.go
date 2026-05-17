package doctor

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// minGitMajor + minGitMinor define the floor bosun supports. 2.40
// shipped April 2023; older versions miss worktree-list --porcelain
// flags we rely on. Below this we emit Fail rather than risk silent
// behavior diffs.
const (
	minGitMajor = 2
	minGitMinor = 40
)

// CheckGitOnPath verifies the `git` executable is reachable.
func CheckGitOnPath(ctx context.Context, _ string) Result {
	if _, err := exec.LookPath("git"); err != nil {
		return Result{
			Name:    "git-on-path",
			Status:  Fail,
			Message: "git not found on PATH",
			Fix:     "install git (https://git-scm.com/downloads) or fix your PATH",
		}
	}
	return Result{Name: "git-on-path", Status: Pass, Message: "git found on PATH"}
}

// gitVersionRe captures major.minor from `git --version` output.
// Form: "git version X.Y.Z" on all platforms we care about.
var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// CheckGitVersion confirms the installed git is at or above minGitMajor.minGitMinor.
// The 2s context bound is paranoid but cheap; a hung `git --version` is
// real if the binary itself is wedged (rare but observed once during
// the bug-hunt).
func CheckGitVersion(ctx context.Context, _ string) Result {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "git", "--version").Output()
	if err != nil {
		return Result{
			Name:    "git-version",
			Status:  Fail,
			Message: fmt.Sprintf("could not run `git --version`: %v", err),
			Fix:     "verify git is installed and on PATH",
		}
	}
	m := gitVersionRe.FindStringSubmatch(strings.TrimSpace(string(out)))
	if len(m) != 3 {
		return Result{
			Name:    "git-version",
			Status:  Warn,
			Message: fmt.Sprintf("git version output unrecognized: %q", strings.TrimSpace(string(out))),
			Fix:     "report this output to bosun maintainers",
		}
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	if maj < minGitMajor || (maj == minGitMajor && min < minGitMinor) {
		return Result{
			Name:    "git-version",
			Status:  Fail,
			Message: fmt.Sprintf("git %d.%d (need >= %d.%d)", maj, min, minGitMajor, minGitMinor),
			Fix:     fmt.Sprintf("upgrade git to %d.%d or newer", minGitMajor, minGitMinor),
		}
	}
	return Result{
		Name:    "git-version",
		Status:  Pass,
		Message: fmt.Sprintf("git %d.%d (supported)", maj, min),
	}
}
