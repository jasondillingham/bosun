package claudehook

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/session"
)

// LabelFromWorktreePath derives the bosun session label that owns
// worktreePath, given the main worktree's repoRoot and the configured
// worktree-suffix pattern.
//
// Returns ("", nil) when worktreePath is the main worktree itself or
// when the basename doesn't match the configured `<repo>+<pattern>`
// shape — those are legitimate "this edit doesn't belong to a bosun
// session" cases and the hook should silently no-op rather than claim
// a wrong label.
//
// Numeric session worktrees (`<repo>-bosun-3`) resolve to the
// canonical "session-3" label; named worktrees (`<repo>-bosun-auth`)
// resolve to "auth". Errors are reserved for invalid configuration
// (pattern without `{N}`); callers should fall back to silent exit-0
// in that case too.
func LabelFromWorktreePath(repoRoot, worktreePath string, cfg config.Config) (string, error) {
	if filepath.Clean(worktreePath) == filepath.Clean(repoRoot) {
		return "", nil
	}
	repoBase := filepath.Base(repoRoot)
	wtBase := filepath.Base(worktreePath)
	if !strings.HasPrefix(wtBase, repoBase) {
		return "", nil
	}
	rest := wtBase[len(repoBase):]

	pattern := cfg.WorktreeSuffixPattern
	idx := strings.Index(pattern, "{N}")
	if idx < 0 {
		return "", fmt.Errorf("worktree_suffix_pattern missing {N}: %q", pattern)
	}
	prefix := pattern[:idx]
	suffix := pattern[idx+len("{N}"):]
	if !strings.HasPrefix(rest, prefix) || !strings.HasSuffix(rest, suffix) {
		return "", nil
	}
	sub := rest[len(prefix) : len(rest)-len(suffix)]
	if sub == "" {
		return "", nil
	}
	// Scheme-C UID-per-worktree form (docs/uid-worktree-design.md): the
	// substituted value is `<YYYYMMDD-HHMMSS>-<label_or_N>`. Strip the
	// timestamp prefix so the remainder is the legacy session/label
	// substring the rest of this function already handles.
	if tail, ok := stripRoundTimestampPrefix(sub); ok {
		sub = tail
	}
	if sub == "" {
		return "", nil
	}
	// Bare-integer substitution is the canonical numeric form: a
	// "-bosun-3" suffix means session-3, matching cfg.SessionName(N).
	if n, err := strconv.Atoi(sub); err == nil {
		if n < 1 {
			return "", nil
		}
		return cfg.SessionName(n), nil
	}
	if err := session.ValidateLabel(sub); err != nil {
		return "", nil
	}
	return sub, nil
}

// stripRoundTimestampPrefix returns the substring after a leading
// `YYYYMMDD-HHMMSS-` token (optionally followed by `-<PID>-`), plus
// true when the prefix was found. The format matches what
// cmd_init.go's newRoundTimestamp() produces.
//
// 2026-05 bug-hunt pass-2 #4 added the PID to the timestamp so
// same-second parallel inits no longer collide; this function now
// strips both the date-time AND the optional PID component so the
// downstream session/label lookup still works.
//
// A non-matching input is returned unchanged with false.
func stripRoundTimestampPrefix(s string) (string, bool) {
	// Need at least "YYYYMMDD-HHMMSS-" (16 chars) plus at least 1 char of tail.
	if len(s) < 17 || s[8] != '-' || s[15] != '-' {
		return s, false
	}
	for i := 0; i < 15; i++ {
		if i == 8 {
			continue
		}
		ch := s[i]
		if ch < '0' || ch > '9' {
			return s, false
		}
	}
	tail := s[16:]
	// Check for the optional `<PID>-<rest>` shape — all digits up to
	// the next dash. If we find it, strip; otherwise the tail is
	// already the legacy `<N>` or `<label>` substring and the caller
	// handles it. Empty PID (consecutive dashes) is not stripped — a
	// `-bosun-20260520-115400--3` literal isn't valid output.
	if dash := strings.IndexByte(tail, '-'); dash > 0 {
		allDigits := true
		for i := 0; i < dash; i++ {
			ch := tail[i]
			if ch < '0' || ch > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return tail[dash+1:], true
		}
	}
	return tail, true
}
