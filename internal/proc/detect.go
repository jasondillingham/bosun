// Package proc detects whether a Claude Code agent is currently running
// inside a given worktree.
//
// Detection is best-effort: a false negative (no RUNNING indicator when an
// agent is in fact present) is acceptable, but a false positive (lighting
// up RUNNING for an unrelated process whose working directory happens to
// coincide with a worktree) is not. We therefore gate matches on both the
// process basename (claude / claude-code / code-cli) and the working
// directory.
package proc

import (
	"path/filepath"
	"strings"

	"github.com/shirou/gopsutil/v3/process"
)

// ProcInfo is the minimal snapshot of a running process needed for matching.
type ProcInfo struct {
	PID  int
	Name string
	CWD  string
}

// Lister enumerates running processes. Production code uses
// GopsutilLister; tests inject fakes to drive the detection logic
// deterministically (including the permission-error path).
type Lister interface {
	List() ([]ProcInfo, error)
}

// GopsutilLister is the default Lister, backed by gopsutil.
type GopsutilLister struct{}

// List returns every process the current user can introspect. Per-process
// errors (typical for processes owned by other users, or for kernel-only
// entries on Linux's /proc) are swallowed silently — the process is simply
// omitted. A non-nil error indicates a failure to enumerate processes at
// all.
func (GopsutilLister) List() ([]ProcInfo, error) {
	ps, err := process.Processes()
	if err != nil {
		return nil, err
	}
	out := make([]ProcInfo, 0, len(ps))
	for _, p := range ps {
		name, err := p.Name()
		if err != nil {
			continue
		}
		cwd, err := p.Cwd()
		if err != nil {
			continue
		}
		out = append(out, ProcInfo{PID: int(p.Pid), Name: name, CWD: cwd})
	}
	return out, nil
}

// Running returns the PID of an agent process whose working directory
// matches worktreePath. ok=false (err=nil) means no agent was found.
func Running(worktreePath string) (pid int, ok bool, err error) {
	return RunningWith(GopsutilLister{}, IsAgent, worktreePath)
}

// RunningWith is the testable core of Running: callers can inject a custom
// Lister and a name predicate. The path-matching logic (absolute-path
// normalization, symlink resolution, first-hit return) is shared with
// Running.
func RunningWith(l Lister, isAgent func(name string) bool, worktreePath string) (pid int, ok bool, err error) {
	target := canonicalize(worktreePath)
	procs, err := l.List()
	if err != nil {
		return 0, false, err
	}
	for _, p := range procs {
		if !isAgent(p.Name) {
			continue
		}
		if canonicalize(p.CWD) == target {
			return p.PID, true, nil
		}
	}
	return 0, false, nil
}

// canonicalize returns an absolute, symlink-resolved form of path so that
// comparisons aren't fooled by /tmp vs /private/tmp (macOS), trailing
// slashes, or "." segments. Each failure step falls back to the previous
// best-effort value rather than returning an error — a path that doesn't
// exist still has *some* canonical-ish form we can compare against.
func canonicalize(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return resolved
}

// IsAgent reports whether name looks like a Claude Code agent process. We
// match the basename (extension stripped) case-insensitively against a
// short allow-list. Gating by name is what keeps a stray shell or `git`
// invocation whose CWD happens to be a worktree from being reported as a
// live agent.
func IsAgent(name string) bool {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
	switch base {
	case "claude", "claude-code", "code-cli":
		return true
	}
	return false
}
