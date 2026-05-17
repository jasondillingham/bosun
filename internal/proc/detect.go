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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shirou/gopsutil/v3/process"
)

// debugEnv toggles per-call diagnostic logging to stderr. Operators
// who see RUNNING="—" for a session whose Ghostty window clearly has
// an active agent can `BOSUN_PROC_DEBUG=1 bosun status` to see which
// candidate processes were skipped and why. Off by default — too
// chatty for normal use.
const debugEnv = "BOSUN_PROC_DEBUG"

// ProcInfo is the minimal snapshot of a running process needed for matching.
//
// Cmdline is the kernel-level command line. We carry it because Name on
// macOS reports the executable basename, and Claude Code installs at
// `~/.local/share/claude/versions/<X.Y.Z>/` — making Name a version
// number ("2.1.143") rather than "claude". Falling back to the cmdline's
// first token recovers the recognizable agent name.
type ProcInfo struct {
	PID     int
	Name    string
	CWD     string
	Cmdline string
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
//
// When BOSUN_PROC_DEBUG=1 is set, skipped processes whose name looks like
// an agent are logged to stderr so operators can diagnose false-negative
// RUNNING detections (the v0.7 round-1 kickoff hit this on every session).
func (GopsutilLister) List() ([]ProcInfo, error) {
	ps, err := process.Processes()
	if err != nil {
		return nil, err
	}
	debug := os.Getenv(debugEnv) == "1"
	out := make([]ProcInfo, 0, len(ps))
	for _, p := range ps {
		name, err := p.Name()
		if err != nil {
			if debug {
				fmt.Fprintf(os.Stderr, "proc: pid %d: name unavailable: %v\n", p.Pid, err)
			}
			continue
		}
		cwd, err := p.Cwd()
		if err != nil {
			if debug && IsAgent(name) {
				fmt.Fprintf(os.Stderr, "proc: pid %d (%s): cwd unavailable: %v — agent candidate skipped\n", p.Pid, name, err)
			}
			continue
		}
		// Cmdline is best-effort; an error here doesn't disqualify the
		// process — the Name+CWD-only fallback still works for installs
		// whose binary is named "claude".
		cmdline, _ := p.Cmdline()
		out = append(out, ProcInfo{PID: int(p.Pid), Name: name, CWD: cwd, Cmdline: cmdline})
	}
	return out, nil
}

// Running returns the PID of an agent process whose working directory
// matches worktreePath. ok=false (err=nil) means no agent was found.
//
// Callers (session.Derive) treat any non-nil error as "not running" so the
// RUNNING column degrades to a false negative rather than poisoning the
// whole status render. Per-process Cwd/Name permission errors are filtered
// inside GopsutilLister.List; a non-nil error here means the entire
// process-table enumeration failed (e.g. /proc denied, ESRCH on a
// hardened jail).
func Running(worktreePath string) (pid int, ok bool, err error) {
	return RunningWith(GopsutilLister{}, IsAgent, worktreePath)
}

// RunningWith is the testable core of Running: callers can inject a custom
// Lister and a name predicate. The path-matching logic (absolute-path
// normalization, symlink resolution, first-hit return) is shared with
// Running.
//
// With BOSUN_PROC_DEBUG=1, name-matched candidates whose CWD does NOT
// match are emitted to stderr. The diagnostic surfaces the most common
// false-negative cause (a CWD that canonicalizes differently — e.g.
// /tmp vs /private/tmp on macOS, or a path the process inherited from
// a parent shell that has since cd'd elsewhere).
func RunningWith(l Lister, isAgent func(name string) bool, worktreePath string) (pid int, ok bool, err error) {
	target := canonicalize(worktreePath)
	procs, err := l.List()
	if err != nil {
		return 0, false, err
	}
	debug := os.Getenv(debugEnv) == "1"
	for _, p := range procs {
		if !matchesAgent(isAgent, p) {
			continue
		}
		got := canonicalize(p.CWD)
		if got == target {
			return p.PID, true, nil
		}
		if debug {
			fmt.Fprintf(os.Stderr, "proc: pid %d (%s) cwd=%s does not match target=%s\n", p.PID, p.Name, got, target)
		}
	}
	return 0, false, nil
}

// matchesAgent runs the predicate against multiple candidate names
// derived from the process. macOS-installed Claude Code reports its
// version number as Name (because the binary is `versions/2.1.143`);
// the cmdline's first token recovers the recognizable "claude" name.
func matchesAgent(isAgent func(name string) bool, p ProcInfo) bool {
	if isAgent(p.Name) {
		return true
	}
	if p.Cmdline != "" {
		if first := firstCmdlineToken(p.Cmdline); first != "" && isAgent(first) {
			return true
		}
	}
	return false
}

// firstCmdlineToken returns the basename of cmdline's first whitespace-
// or NUL-separated token. gopsutil joins argv with spaces on macOS;
// Linux's /proc/<pid>/cmdline uses NULs that gopsutil also reports as
// spaces in v3. A bare basename (no path) returns unchanged.
func firstCmdlineToken(cmdline string) string {
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return ""
	}
	return filepath.Base(fields[0])
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
