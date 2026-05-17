// Package doctor runs environmental health checks before bosun goes
// to work. The point is to catch the recurring "right in front of us"
// hazards the dogfood loop kept producing — iCloud/Spotlight indexing
// the repo, orphan worktree dirs from prior cleanups, stale lock
// files, phantom branch refs — before they derail a new user.
//
// Each check returns a Result. Run aggregates them so callers can
// render however they want; cmd_doctor.go produces a human report.
package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Status is the severity of a check's outcome.
type Status int

const (
	// Pass means the check found nothing concerning.
	Pass Status = iota
	// Warn means the check found something the operator should know
	// about but bosun can still proceed. Examples: iCloud Drive
	// indexing the repo (creates phantom files but the phantom filter
	// suppresses them); orphan worktree dirs (don't block init but
	// will accumulate).
	Warn
	// Fail means bosun cannot proceed cleanly. Examples: git not on
	// PATH, no write permissions on .bosun/, MCP daemon can't bind a
	// socket.
	Fail
)

func (s Status) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	}
	return "?"
}

// Result is one check's outcome.
type Result struct {
	// Name is a short identifier for the check (e.g. "git-version",
	// "filesync-icloud"). Used for grepping logs.
	Name string
	// Status is PASS/WARN/FAIL.
	Status Status
	// Message is the human-readable description of what was found.
	// Always populated, even on PASS (e.g. "git 2.45.0").
	Message string
	// Fix is an optional one-line hint pointing the operator at the
	// manual remediation. Empty on PASS.
	Fix string
	// FixFn applies an automated remediation when `bosun doctor --fix`
	// runs. Nil = no autofix; the operator has to follow the manual Fix
	// hint instead. Implementations must be idempotent and safe to call
	// when the issue has already been remediated (e.g. by a parallel
	// doctor run).
	FixFn func(repoRoot string) error
	// FixDescription names what FixFn would do, in past-tense imperative
	// ("removed init.lock", "renamed 2 orphan dirs to _orphan-*"). Used
	// by --dry-run output and by the final fix-summary line. Empty when
	// FixFn is nil.
	FixDescription string
}

// Check is one environmental probe. The function takes the repo root
// (always the main worktree, not a session worktree — passed by the
// caller) and returns its result. Checks must be cheap (sub-second)
// and side-effect free unless explicitly noted otherwise.
type Check func(ctx context.Context, repoRoot string) Result

// Run executes every check in order and returns the aggregated results.
// Order matches the order checks are registered, so output is
// deterministic across invocations. Failed checks do NOT short-circuit
// the run — the operator wants the full picture, not just the first
// thing that broke.
func Run(ctx context.Context, repoRoot string, checks []Check) []Result {
	out := make([]Result, 0, len(checks))
	for _, c := range checks {
		out = append(out, c(ctx, repoRoot))
	}
	return out
}

// DefaultChecks is the standard battery `bosun doctor` runs. Kept in
// one place so the order is reviewable and tests can drive a
// representative subset by name.
func DefaultChecks() []Check {
	return []Check{
		CheckGitVersion,
		CheckGitOnPath,
		CheckRepoWriteable,
		CheckBosunDirWriteable,
		CheckFileSync,
		CheckOrphanWorktrees,
		CheckStaleInitLock,
		CheckPhantomBranchRefs,
		CheckMCPDaemonStartup,
	}
}

// Worst returns the highest-severity Status in results. Used by the
// CLI to set the process exit code.
func Worst(results []Result) Status {
	worst := Pass
	for _, r := range results {
		if r.Status > worst {
			worst = r.Status
		}
	}
	return worst
}

// WriteReport renders results to w in the format `bosun doctor` shows
// by default. Kept in the doctor package (not cmd_doctor.go) so any
// future consumer — TUI, MCP tool, JSON output mode — can call the
// same renderer.
func WriteReport(w io.Writer, repoRoot string, results []Result) {
	fmt.Fprintf(w, "Bosun health check — %s\n\n", repoRoot)
	var warns, fails int
	for _, r := range results {
		mark := "✓"
		switch r.Status {
		case Warn:
			mark = "⚠"
			warns++
		case Fail:
			mark = "✗"
			fails++
		}
		fmt.Fprintf(w, "  %s %s: %s\n", mark, r.Name, r.Message)
		if r.Fix != "" {
			fmt.Fprintf(w, "      fix: %s\n", r.Fix)
		}
	}
	fmt.Fprintln(w)
	switch {
	case fails > 0:
		fmt.Fprintf(w, "%d failure(s), %d warning(s) — bosun may not work cleanly until these are addressed.\n", fails, warns)
	case warns > 0:
		fmt.Fprintf(w, "%d warning(s) — bosun should work but the operator should be aware.\n", warns)
	default:
		fmt.Fprintln(w, "All checks passed.")
	}
}

// FixOutcome records one remediation attempt for the --fix path.
type FixOutcome struct {
	Name        string // matches the Result.Name that owned the fixer
	Description string // copied from Result.FixDescription
	Applied     bool   // true on successful execution; false on no-fix-available or dry-run
	DryRun      bool   // true when this outcome was produced by a dry-run pass
	Err         error  // non-nil when FixFn ran but errored
}

// ApplyFixes invokes each Result's FixFn (in result order) for results
// that have one. When dryRun is true, FixFn is NOT called and outcomes
// are marked DryRun=true; the operator sees what would happen without
// any state mutation. Results without a FixFn are silently skipped —
// they're reported in WriteReport's manual-fix hint already.
func ApplyFixes(repoRoot string, results []Result, dryRun bool) []FixOutcome {
	var out []FixOutcome
	for _, r := range results {
		if r.FixFn == nil {
			continue
		}
		if r.Status == Pass {
			// Already clean; nothing to fix even though a fixer exists.
			continue
		}
		oc := FixOutcome{Name: r.Name, Description: r.FixDescription, DryRun: dryRun}
		if !dryRun {
			if err := r.FixFn(repoRoot); err != nil {
				oc.Err = err
			} else {
				oc.Applied = true
			}
		}
		out = append(out, oc)
	}
	return out
}

// WriteFixReport renders the outcome of ApplyFixes for human consumption.
// Renders nothing when outcomes is empty (no fixers ran). Always called
// AFTER WriteReport so the operator sees both the diagnosis and the
// remediation.
func WriteFixReport(w io.Writer, outcomes []FixOutcome) {
	if len(outcomes) == 0 {
		return
	}
	if outcomes[0].DryRun {
		fmt.Fprintln(w, "Auto-fixes (dry-run; nothing applied):")
	} else {
		fmt.Fprintln(w, "Auto-fixes:")
	}
	var applied, failed int
	for _, oc := range outcomes {
		switch {
		case oc.DryRun:
			fmt.Fprintf(w, "  → would fix %s: %s\n", oc.Name, oc.Description)
		case oc.Err != nil:
			fmt.Fprintf(w, "  ✗ %s: fix failed — %v\n", oc.Name, oc.Err)
			failed++
		case oc.Applied:
			fmt.Fprintf(w, "  ✓ %s: %s\n", oc.Name, oc.Description)
			applied++
		}
	}
	fmt.Fprintln(w)
	switch {
	case outcomes[0].DryRun:
		fmt.Fprintf(w, "%d fix(es) would be applied — re-run without --dry-run to execute.\n", len(outcomes))
	case failed > 0:
		fmt.Fprintf(w, "%d fix(es) applied, %d failed — see errors above.\n", applied, failed)
	default:
		fmt.Fprintf(w, "%d fix(es) applied.\n", applied)
	}
}

// repoRootName returns the basename of repoRoot. Used by checks that
// need to construct sibling-worktree path predicates (e.g.
// `<reporoot>-bosun-<session>`).
func repoRootName(repoRoot string) string {
	return filepath.Base(filepath.Clean(repoRoot))
}

// statResult is the (info, error) pair from os.Stat, hoisted to a
// helper so individual checks don't repeat the existence-error pattern.
func statResult(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
