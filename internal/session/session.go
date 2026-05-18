// Package session derives the list of bosun-managed sessions from the
// underlying git state plus the .bosun/ coordination directory.
package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/proc"
)

// State is the merge-readiness state of a session.
type State string

const (
	StateWorking State = "WORKING"
	StateDone    State = "DONE"
	StateStuck   State = "STUCK"
	// StateCrashed is derived during Derive — a WORKING session whose agent
	// process is no longer present in the worktree AND whose worktree has
	// uncommitted dirty files. Not persisted to any marker file; it's a
	// display-only state recomputed on each Derive call.
	StateCrashed State = "CRASHED"
)

// HeartbeatStaleAfter is the threshold past which a recorded heartbeat is
// considered stale. WORKING sessions whose heartbeat is older than this
// surface as Stale=true (a derived flag, not a separate State value).
const HeartbeatStaleAfter = 5 * time.Minute

// Session is the aggregated view of one bosun-managed session.
//
// Numbered sessions (e.g. `bosun init 3`): Number=1..N, Name="session-N",
// Label==Name. Named sessions (e.g. `bosun init auth`): Number=0,
// Name=="auth", Label=="auth". The two forms share the same Session struct
// so downstream callers can keep operating on a single Session slice.
type Session struct {
	Number     int           // 1-based session number; 0 for named sessions
	Name       string        // e.g. "session-1" or "auth"
	Label      string        // canonical label (matches Name)
	Branch     string        // e.g. "bosun/session-1" or "bosun/auth"
	Path       string        // absolute worktree path
	Ahead      int           // commits ahead of base
	Dirty      int           // count of tracked-file changes
	Claimed    int           // count of distinct claimed paths
	State      State         // WORKING / DONE / STUCK / CRASHED
	StateMsg   string        // optional body of the state file
	Last       *git.LogEntry // last commit ahead of base (nil if none)
	Running    bool          // true when an agent process (claude/claude-code/code-cli) is live in Path
	RunningPID int           // pid of that agent process; 0 when Running is false
	// RunningExternal is set when the liveness gate skipped its own
	// detection because the operator opted into external-driven workers
	// (config.LivenessGate == "external"). Renderers show "external" in
	// the RUNNING column in that case; CRASHED auto-transitions are
	// suppressed. The flag is independent of Running — a session can be
	// "external" with no observed PID and still not flicker CRASHED.
	RunningExternal bool
	// Stale is a derived flag — set when a WORKING (not CRASHED) session has
	// a recorded heartbeat older than HeartbeatStaleAfter. Kept off the
	// State enum so the wire-stable state values stay compact; UI surfaces
	// (status table, JSON) render it as a separate marker alongside State.
	Stale bool
	// HeartbeatAt is the most recent heartbeat timestamp on disk, or the
	// zero time when no heartbeat exists. Useful for the operator to see
	// how long it has been since the agent last checked in.
	HeartbeatAt time.Time
	// Parent is the label of the session that spawned this one via
	// `bosun_spawn` (v0.9+). Empty for top-level sessions. Renderers
	// use Parent + Depth to build the tree-shaped status output.
	Parent string
	// Children is the labels of sub-sessions this session has spawned.
	// Sorted alphabetically. Empty for leaf sessions.
	Children []string
	// Depth is 0 for top-level sessions, parent.Depth+1 for sub-
	// sessions. Populated from internal/spawntree when the caller
	// passes a non-nil SpawnTreeReader to Derive.
	Depth int
	// Subtasks is the count of currently-active (un-cancelled) sub-
	// tasks for this session, computed from .bosun/subtasks/<label>/.
	// Populated by an enrichment pass at the call site (mirrors the
	// spawn-tree pattern — Derive stays independent of the subtasks
	// package). Zero for sessions that have never run a sub-task.
	Subtasks int
}

// GetLabel + SetTreeInfo satisfy spawntree.SessionLike so a
// *Session can be passed to spawntree.Store.EnrichSessions without
// internal/session having to import internal/spawntree (avoids the
// cycle; v0.9 spawn-tree enrichment runs on the caller side).
func (s *Session) GetLabel() string { return s.Label }

func (s *Session) SetTreeInfo(parent string, children []string, depth int) {
	s.Parent = parent
	s.Children = children
	s.Depth = depth
}

// Derive computes the Session list for repoRoot. It calls into the git
// client for branch/commit info, but state and claims are read separately
// by the caller (this keeps session/ independent of those packages).
//
// The caller passes a StateReader and ClaimsReader so we don't import
// claims/ and state/ from here — they import session/ for the Session
// type and we'd get a cycle otherwise.
type StateReader interface {
	Read(repoRoot, sessionName string) (state State, msg string, err error)
	// Heartbeat returns the most recent heartbeat timestamp for sessionName.
	// `exists` is false when no heartbeat file has been written; in that
	// case `at` is the zero time. A missing heartbeat must not be confused
	// with a stale one — agents that never call bosun_heartbeat shouldn't
	// be flagged STALE.
	Heartbeat(repoRoot, sessionName string) (at time.Time, exists bool, err error)
	// Attached returns the pid recorded via `bosun attach` (or the MCP
	// equivalent) for sessionName. `ok` is false when no attached-pid
	// file has been written; in that case `pid` is 0. The liveness gate
	// consults this BEFORE scanning the process table so external
	// workers (Claude Code Task sub-agents, CI agents, hand-launched
	// terminals whose basename isn't `claude`) can register themselves
	// without proc-scan false-CRASHED churn.
	Attached(repoRoot, sessionName string) (pid int, ok bool, err error)
}

type ClaimsReader interface {
	CountFor(repoRoot, sessionName string) (int, error)
}

// Derive returns all bosun-managed sessions in repoRoot, sorted by number.
func Derive(ctx context.Context, c *git.Client, cfg config.Config, repoRoot string, sr StateReader, cr ClaimsReader) ([]Session, error) {
	worktrees, err := c.ListWorktrees(ctx, repoRoot)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}

	// Matches any bosun-managed branch: numeric session-N form or a bare
	// label. The label charset (lower ASCII, digits, dashes, must start
	// with a letter) is also enforced on init via ValidateLabel.
	branchRe := regexp.MustCompile(`^refs/heads/` + regexp.QuoteMeta(cfg.SessionPrefix) + `/([a-z][a-z0-9-]*)$`)

	var result []Session
	for _, wt := range worktrees {
		m := branchRe.FindStringSubmatch(wt.Branch)
		if m == nil {
			continue
		}
		// Skip worktrees whose on-disk directory is gone. Git keeps the
		// admin metadata until `git worktree prune` runs, but every
		// subsequent `git -C <path>` call would fail. Cleanup/remove
		// prune these explicitly; here we just keep `bosun status`
		// rendering instead of dying on the first missing dir.
		if wt.Prunable {
			continue
		}
		label := m[1]
		// Numbered sessions populate Number; named sessions leave it at 0
		// (and ParseLabel rejects "session-0"/"session-" forms upstream).
		number := 0
		if rest, ok := strings.CutPrefix(label, "session-"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n >= 1 {
				number = n
			} else {
				// "session-foo" or "session-0" — not a bosun-managed branch.
				continue
			}
		}
		name := label
		branch := cfg.BranchForLabel(label)

		ahead, err := c.RevListCount(ctx, wt.Path, cfg.BaseBranch)
		if err != nil {
			return nil, fmt.Errorf("rev-list %s: %w", name, err)
		}
		dirty, err := c.DirtyCount(ctx, wt.Path)
		if err != nil {
			return nil, fmt.Errorf("status %s: %w", name, err)
		}
		var last *git.LogEntry
		if ahead > 0 {
			last, err = c.LastCommit(ctx, wt.Path, cfg.BaseBranch)
			if err != nil {
				return nil, fmt.Errorf("log %s: %w", name, err)
			}
		}

		state, msg, err := sr.Read(repoRoot, name)
		if err != nil {
			return nil, fmt.Errorf("read state %s: %w", name, err)
		}
		claimed, err := cr.CountFor(repoRoot, name)
		if err != nil {
			return nil, fmt.Errorf("read claims %s: %w", name, err)
		}

		// Heartbeat is best-effort: a missing or unreadable file is
		// treated as "no heartbeat recorded", not an error. Surfacing
		// a status render failure because an agent never called
		// bosun_heartbeat would be worse than rendering without the
		// stale flag.
		hbAt, hbExists, _ := sr.Heartbeat(repoRoot, name)

		// Liveness gate: in "external" mode the operator has declared
		// they're driving workers from outside the proc-scan's view
		// (Claude Code Task sub-agents, CI agents, …). Skip the entire
		// detection path — Running stays false, RunningExternal flags
		// the column, and CRASHED auto-transitions are suppressed.
		// Otherwise fall through the attach-then-proc-scan ladder.
		var (
			running         bool
			runPID          int
			runningExternal bool
			attachedDead    bool
		)
		if cfg.LivenessGate == config.LivenessGateExternal {
			runningExternal = true
		} else {
			// Attached PID check runs FIRST: an explicit registration
			// beats the proc table. If the file is present but the PID
			// is dead, the explicit "I was here" makes the
			// disappearance meaningful, so we flip CRASHED (with
			// dirty=0 still excluded — see below).
			if pid, ok, _ := sr.Attached(repoRoot, name); ok {
				if proc.IsAlive(pid) {
					running = true
					runPID = pid
				} else {
					attachedDead = true
				}
			} else {
				// proc.Running is best-effort: a permission error or
				// transient failure shouldn't keep `bosun status` from
				// rendering. Worst case: a false negative on RUNNING.
				runPID, running, _ = proc.Running(wt.Path)
			}
		}

		// CRASHED is a derived display state: a WORKING session whose
		// agent is gone but whose worktree has uncommitted dirty files.
		// DONE/STUCK sessions are never crashed — they declared their
		// own terminal state. External mode suppresses CRASHED entirely.
		// The attached-but-dead case is the v0.11 "recoverable crash":
		// the registration says "I was here" and the disappearance is
		// meaningful, so we flip CRASHED regardless of dirty-count.
		if cfg.LivenessGate != config.LivenessGateExternal && state == StateWorking {
			if attachedDead {
				state = StateCrashed
			} else if !running && dirty > 0 {
				state = StateCrashed
			}
		}

		// STALE is a derived flag, not a State value: a WORKING (not
		// CRASHED) session that recorded a heartbeat which has since
		// gone older than HeartbeatStaleAfter. No heartbeat ever
		// recorded → not stale (we can't distinguish "agent doesn't
		// emit heartbeats" from "agent is hung", so we avoid the false
		// positive).
		stale := false
		if state == StateWorking && hbExists && time.Since(hbAt) > HeartbeatStaleAfter {
			stale = true
		}

		result = append(result, Session{
			Number:          number,
			Name:            name,
			Label:           label,
			Branch:          branch,
			Path:            wt.Path,
			Ahead:           ahead,
			Dirty:           dirty,
			Claimed:         claimed,
			State:           state,
			StateMsg:        msg,
			Last:            last,
			Running:         running,
			RunningPID:      runPID,
			RunningExternal: runningExternal,
			Stale:           stale,
			HeartbeatAt:     hbAt,
		})
	}

	// Numeric sessions sort by number; named sessions land after numerics in
	// label-alphabetical order so the operator gets a stable, scannable list.
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Number != 0 && result[j].Number != 0 {
			return result[i].Number < result[j].Number
		}
		if result[i].Number != 0 {
			return true
		}
		if result[j].Number != 0 {
			return false
		}
		return result[i].Label < result[j].Label
	})
	return result, nil
}

// ParseName accepts either "session-N" or "N" and returns the integer N.
// Returns an error on anything else — named labels go through ParseLabel.
func ParseName(s string) (int, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "session-") {
		s = strings.TrimPrefix(s, "session-")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid session reference %q (want N or session-N)", s)
	}
	return n, nil
}

// segmentRe matches one dot-separated segment of a label: lowercase
// ASCII alphanumerics optionally joined by single dashes. Must start
// with a letter and may not end with a dash or contain `--`. Branches
// derived from a label end up in `bosun/<label>`; trailing-dash and
// consecutive-dash forms are syntactically valid git refs but
// consistently bite operators.
var segmentRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// labelRe matches a full label — one or more segments joined by single
// dots. The dot is the v0.9 separator for sub-session labels spawned
// via the bosun_spawn MCP tool: a parent `session-1` spawns
// `session-1.auth` and `session-1.http`. No leading/trailing dots, no
// `..`, each segment matches segmentRe.
var labelRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*(\.[a-z][a-z0-9]*(-[a-z0-9]+)*)*$`)

// ValidateLabel returns nil if s is a valid bosun label. Used at init time
// to reject malformed named-session args before any branches are created.
// Also enforced for label-derived heading parsing in the brief package via
// its own regex (kept in sync deliberately — see internal/brief/brief.go).
//
// v0.9 added the dotted-suffix form for sub-sessions: a parent's
// AddChild spawn yields labels like `session-1.auth`. Each dot-
// separated segment must independently match the historical label
// charset (see segmentRe).
func ValidateLabel(s string) error {
	if !labelRe.MatchString(s) {
		return fmt.Errorf("invalid session label %q (want lowercase letters/digits separated by single dashes, optionally joined by dots for sub-sessions; no leading/trailing dot, no `..`, no `--`)", s)
	}
	return nil
}

// ParseLabel canonicalizes a session reference into its label form. It
// accepts numeric input ("3" → "session-3"), the "session-N" form
// (unchanged), or a bare label ("auth" → "auth"). Returns an error only
// for empty input or strings that don't match the label charset.
func ParseLabel(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("invalid session reference %q (empty)", s)
	}
	// Numeric ("3") → "session-3". A bare integer always means the numeric
	// session form; callers preserving numeric behavior route through
	// ParseName, which rejects everything else.
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 {
			return "", fmt.Errorf("invalid session reference %q (want N >= 1 or a label)", s)
		}
		return fmt.Sprintf("session-%d", n), nil
	}
	if err := ValidateLabel(s); err != nil {
		return "", err
	}
	return s, nil
}

// IsNumericLabel reports whether label is the "session-N" form. Callers
// that need to switch behavior between numeric and named sessions (e.g.
// cleanup --orphans) use this rather than re-parsing the label themselves.
func IsNumericLabel(label string) bool {
	if rest, ok := strings.CutPrefix(label, "session-"); ok {
		n, err := strconv.Atoi(rest)
		return err == nil && n >= 1
	}
	return false
}

// WorktreePath returns the canonical worktree path for numeric session N
// relative to the repo's parent dir.
// Example: WorktreePath("/code/myproj", cfg, 3, "") => "/code/myproj-bosun-3".
// A non-empty roundTimestamp produces the scheme-C UID-per-worktree form
// (see docs/uid-worktree-design.md).
func WorktreePath(repoRoot string, cfg config.Config, n int, roundTimestamp string) string {
	return WorktreePathForLabel(repoRoot, cfg, cfg.SessionName(n), roundTimestamp)
}

// WorktreePathForLabel returns the canonical worktree path for a session
// label. Numeric ("session-3") and named ("auth") labels share the same
// computation — only the suffix differs.
// Example: WorktreePathForLabel("/code/myproj", cfg, "auth", "") => "/code/myproj-bosun-auth".
// Non-empty roundTimestamp ("20260518-115400") yields
// "/code/myproj-bosun-20260518-115400-auth" per scheme C
// (docs/uid-worktree-design.md).
func WorktreePathForLabel(repoRoot string, cfg config.Config, label, roundTimestamp string) string {
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)
	return filepath.Join(parent, base+cfg.WorktreeSuffixForLabel(label, roundTimestamp))
}

// LegacyWorktreePathForLabel returns the worktree path under the pre-v0.11
// naming convention (`<repo>-bosun-<sub>`), regardless of what config
// currently produces. Used by the migration path so the doctor check and
// `bosun migrate` agree on what shapes count as "legacy" — without
// referencing the live cfg.WorktreeSuffixPattern, which the naming-scheme
// lane may rewrite to produce the new shape going forward.
//
// Example: LegacyWorktreePathForLabel("/code/myproj", "session-3")
// =>      "/code/myproj-bosun-3"
//
// (Numeric labels strip the "session-" prefix to stay byte-identical with
// what v0.10 and earlier wrote.)
func LegacyWorktreePathForLabel(repoRoot, label string) string {
	sub := label
	if rest, ok := strings.CutPrefix(label, "session-"); ok {
		sub = rest
	}
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)
	return filepath.Join(parent, base+"-bosun-"+sub)
}

// ResolveWorktreePath returns the on-disk worktree path bosun should use
// for label. Preference order:
//
//  1. The canonical path WorktreePathForLabel produces (post-naming-lane
//     this is the new `<repo>-bosun-<timestamp>-<sub>` shape; today it's
//     still the legacy shape).
//  2. The legacy `<repo>-bosun-<sub>` shape, if it exists on disk and the
//     canonical does not.
//  3. The canonical path (used for new-creation by callers).
//
// This is the "read-only compatibility" hook described in
// `docs/uid-worktree-migration.md` — once the naming lane lands and
// `WorktreePathForLabel` starts returning new-shape paths, every existing
// caller that stats the canonical path falls back to the legacy shape
// instead of silently mis-resolving to a non-existent dir.
func ResolveWorktreePath(repoRoot string, cfg config.Config, label, roundTimestamp string) string {
	canonical := WorktreePathForLabel(repoRoot, cfg, label, roundTimestamp)
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	legacy := LegacyWorktreePathForLabel(repoRoot, label)
	if legacy != canonical {
		if _, err := os.Stat(legacy); err == nil {
			return legacy
		}
	}
	return canonical
}

// IsLegacyWorktreePath reports whether path matches the pre-v0.11
// `<repo>-bosun-<sub>` shape for any plausible label, anchored to the
// repo's parent directory.
//
// "Plausible label" means: the suffix after `-bosun-` decodes back to a
// valid bosun label via ParseLabel (numeric "1" → "session-1" or a bare
// label charset). Random non-bosun siblings whose names happen to start
// with `<repo>-bosun-` but carry junk suffixes are NOT classified as
// legacy worktrees — those are orphan-dir territory, handled separately.
//
// Parent-directory matching evaluates symlinks so macOS callers passing
// a `/var/folders/...` tempdir don't trip on git's canonicalized
// `/private/var/...` answer.
func IsLegacyWorktreePath(repoRoot, path string) bool {
	repoParent := resolveSymlinks(filepath.Dir(filepath.Clean(repoRoot)))
	pathParent := resolveSymlinks(filepath.Dir(filepath.Clean(path)))
	if repoParent != pathParent {
		return false
	}
	base := filepath.Base(filepath.Clean(repoRoot))
	prefix := base + "-bosun-"
	name := filepath.Base(filepath.Clean(path))
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return false
	}
	// Reject anything that looks like the new <timestamp>-<sub> shape —
	// a digits-only first segment followed by a dash means we're looking
	// at a post-migration dir (e.g. `myproj-bosun-20260518143025-1`).
	if idx := strings.IndexByte(rest, '-'); idx > 0 {
		first := rest[:idx]
		if isAllDigits(first) {
			return false
		}
	}
	if _, err := ParseLabel(rest); err != nil {
		return false
	}
	return true
}

// resolveSymlinks is a best-effort wrapper around filepath.EvalSymlinks
// that falls back to filepath.Clean when the path doesn't exist (or when
// symlink resolution otherwise fails). The fallback matters during
// classification of paths bosun is about to create or has just renamed
// out from under itself.
func resolveSymlinks(p string) string {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return resolved
}

// isAllDigits reports whether s is non-empty and consists entirely of
// ASCII digits. Used by IsLegacyWorktreePath to discriminate new-shape
// `<timestamp>-<sub>` suffixes from legacy bare-suffix forms.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
