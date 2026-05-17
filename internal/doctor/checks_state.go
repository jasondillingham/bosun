package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleInitLockAge is the threshold above which a leftover init.lock
// is considered stale. POSIX flock auto-releases on process death, so
// the lock file itself is just a marker — but a file that's hours
// old is a useful signal that a prior init died (sleep / crash /
// Ctrl-C). The actual flock state isn't queryable cheaply from
// outside the holder; mtime is the proxy.
const staleInitLockAge = 1 * time.Hour

// CheckStaleInitLock looks for a `.bosun/init.lock` that hasn't been
// touched in a while. The bytes-on-disk lock is harmless (the next
// `bosun init` will re-flock cleanly), but a stale one is a hint
// that something interesting happened to the prior init that the
// operator may want to investigate. Surfaces the phantom-state-file
// count too, since both live under .bosun/.
func CheckStaleInitLock(_ context.Context, repoRoot string) Result {
	lockPath := filepath.Join(repoRoot, ".bosun", "init.lock")
	info, err := statResult(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{
				Name:    "init-lock",
				Status:  Pass,
				Message: "no leftover init.lock",
			}
		}
		return Result{
			Name:    "init-lock",
			Status:  Warn,
			Message: fmt.Sprintf("stat %s: %v", lockPath, err),
		}
	}
	age := time.Since(info.ModTime())
	if age < staleInitLockAge {
		return Result{
			Name:    "init-lock",
			Status:  Pass,
			Message: fmt.Sprintf("init.lock present but fresh (touched %s ago)", roundDuration(age)),
		}
	}
	return Result{
		Name:    "init-lock",
		Status:  Warn,
		Message: fmt.Sprintf("init.lock is %s old (prior init may have died)", roundDuration(age)),
		Fix:     "if no `bosun init` is currently running, `rm " + lockPath + "` is safe",
		FixFn: func(repoRoot string) error {
			// POSIX flock auto-releases on process death so the bytes-on-disk
			// lock file alone is harmless to remove. Idempotent: rm of a
			// missing file is a no-op for our purposes.
			path := filepath.Join(repoRoot, ".bosun", "init.lock")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		},
		FixDescription: "removed stale .bosun/init.lock",
	}
}

// CheckPhantomBranchRefs scans `.git/refs/heads/bosun/` for refs whose
// names contain Spotlight/iCloud duplicate shapes (`session-1 2`,
// `session-1 (1)`). Phantoms here aren't actively harmful — bosun's
// `--clean-phantoms` flag in init removes them — but they're a sign
// the working tree is iCloud-managed (corroborates filesync check) and
// they clutter `git branch --list` output.
func CheckPhantomBranchRefs(_ context.Context, repoRoot string) Result {
	// Mirror cmd_init.go's findPhantomBranchRefs but inline; we don't
	// want doctor depending on cmd/bosun.
	dir := filepath.Join(repoRoot, ".git", "refs", "heads", "bosun")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{
				Name:    "phantom-branch-refs",
				Status:  Pass,
				Message: "no bosun branches yet (nothing to check)",
			}
		}
		return Result{
			Name:    "phantom-branch-refs",
			Status:  Warn,
			Message: fmt.Sprintf("read %s: %v", dir, err),
		}
	}
	var phantoms []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Spotlight/iCloud duplicate shapes: `name N` or `name (N)`.
		if hasDigitSuffix(name, " ") || hasParenDigitSuffix(name) {
			phantoms = append(phantoms, name)
		}
	}
	if len(phantoms) == 0 {
		return Result{
			Name:    "phantom-branch-refs",
			Status:  Pass,
			Message: "no phantom branch refs",
		}
	}
	shown := phantoms
	suffix := ""
	if len(shown) > 3 {
		shown = shown[:3]
		suffix = fmt.Sprintf(" (+ %d more)", len(phantoms)-3)
	}
	return Result{
		Name:    "phantom-branch-refs",
		Status:  Warn,
		Message: fmt.Sprintf("%d phantom branch ref(s): %s%s", len(phantoms), strings.Join(shown, ", "), suffix),
		Fix:     "run `bosun init --clean-phantoms` to remove them",
		FixFn: func(repoRoot string) error {
			// Re-scan rather than capturing the phantoms slice — the
			// set may have shifted (Spotlight is the source; it's
			// non-deterministic) between Run() and ApplyFixes(). Same
			// pattern cmd_init.go --clean-phantoms uses.
			refsDir := filepath.Join(repoRoot, ".git", "refs", "heads", "bosun")
			entries, err := os.ReadDir(refsDir)
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if hasDigitSuffix(name, " ") || hasParenDigitSuffix(name) {
					if rmErr := os.Remove(filepath.Join(refsDir, name)); rmErr != nil && !os.IsNotExist(rmErr) {
						return rmErr
					}
				}
			}
			return nil
		},
		FixDescription: fmt.Sprintf("removed %d phantom branch ref(s)", len(phantoms)),
	}
}

// hasDigitSuffix returns true when s ends with `<sep>N` where N is one
// or more digits. We don't import the phantom package to keep this
// check honest about what it's looking at (branch refs are not state
// files; the extension whitelist phantom.IsLikelyPhantom enforces
// would mis-fit here).
func hasDigitSuffix(s, sep string) bool {
	i := strings.LastIndex(s, sep)
	if i < 0 || i == len(s)-len(sep) {
		return false
	}
	rest := s[i+len(sep):]
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hasParenDigitSuffix returns true for shapes like "name (1)" or
// "name (12)". The iCloud duplicate format.
func hasParenDigitSuffix(s string) bool {
	if !strings.HasSuffix(s, ")") {
		return false
	}
	open := strings.LastIndex(s, "(")
	if open < 0 || open == len(s)-2 { // no chars between parens
		return false
	}
	inside := s[open+1 : len(s)-1]
	for _, r := range inside {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// roundDuration trims sub-second noise from a duration for human
// display ("1h23m" rather than "1h23m45.678901s").
func roundDuration(d time.Duration) time.Duration {
	switch {
	case d > time.Hour:
		return d.Round(time.Minute)
	case d > time.Minute:
		return d.Round(time.Second)
	default:
		return d.Round(time.Second)
	}
}
