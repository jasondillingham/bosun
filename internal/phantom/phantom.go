// Package phantom detects macOS Spotlight / Time Machine / iCloud Drive
// duplicate-file artifacts that appear in `.bosun/` directories synced
// via Apple's file-provider stack.
//
// Two shapes show up:
//
//	session-1 2.json     // Spotlight / Time Machine
//	session-1 (1).json   // iCloud Drive
//
// When `bosun list` or `bosun status` enumerates a state/claims dir
// blindly, each phantom surfaces as an extra session — observed during
// the v0.7 round-1 kickoff. Callers run names through IsLikelyPhantom
// before treating them as real entries.
package phantom

import (
	"regexp"
	"strings"
)

// spotlightPattern matches `<name> <digits>.<ext>` — Spotlight/Time
// Machine duplicate shape. The leading `^.*` greedy capture intentionally
// allows arbitrary base names; the space + digit run + literal dot are
// the load-bearing constraints.
var spotlightPattern = regexp.MustCompile(`^.* \d+\.[^.]+$`)

// iCloudPattern matches `<name> (<digits>).<ext>` — iCloud Drive's
// conflict-resolution shape.
var iCloudPattern = regexp.MustCompile(`^.* \(\d+\)\.[^.]+$`)

// IsLikelyPhantom reports whether name looks like a Finder/Spotlight/
// iCloud duplicate. When at least one extension is passed, the match
// additionally requires the file extension to be in that allow-list —
// avoiding false positives for arbitrary worktree files like
// "Section 2.txt" that legitimately follow the same shape but aren't
// state/claim files.
//
// Pass no extensions to accept any extension. Pass a list (without
// leading dots) to restrict, e.g.:
//
//	IsLikelyPhantom("session-1 2.json", "json")               // true
//	IsLikelyPhantom("session-1 2.done", "done", "stuck")      // true
//	IsLikelyPhantom("section 2.txt", "json")                  // false
//	IsLikelyPhantom("section 2.txt")                          // true (no allow-list)
func IsLikelyPhantom(name string, exts ...string) bool {
	if !spotlightPattern.MatchString(name) && !iCloudPattern.MatchString(name) {
		return false
	}
	if len(exts) == 0 {
		return true
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return false
	}
	ext := name[dot+1:]
	for _, allowed := range exts {
		if ext == allowed {
			return true
		}
	}
	return false
}
