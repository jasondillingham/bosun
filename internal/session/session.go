// Package session derives the list of bosun-managed sessions from the
// underlying git state plus the .bosun/ coordination directory.
package session

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
)

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
	State      State         // WORKING / DONE / STUCK
	StateMsg   string        // optional body of the state file
	Last       *git.LogEntry // last commit ahead of base (nil if none)
	Running    bool          // true when an agent process (claude/claude-code/code-cli) is live in Path
	RunningPID int           // pid of that agent process; 0 when Running is false
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

		// proc.Running is best-effort: a permission error or transient
		// failure shouldn't keep `bosun status` from rendering. The
		// worst case is a false negative on the RUNNING column.
		runPID, running, _ := proc.Running(wt.Path)

		result = append(result, Session{
			Number:     number,
			Name:       name,
			Label:      label,
			Branch:     branch,
			Path:       wt.Path,
			Ahead:      ahead,
			Dirty:      dirty,
			Claimed:    claimed,
			State:      state,
			StateMsg:   msg,
			Last:       last,
			Running:    running,
			RunningPID: runPID,
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

// labelRe matches valid bosun session labels: lowercase ASCII alphanumerics
// optionally joined by single dashes. Must start with a letter and may not
// end with a dash or contain `--`. Branches derived from a label end up in
// `bosun/<label>`; trailing-dash and consecutive-dash forms are syntactically
// valid git refs but consistently bite operators (shell tab-completion eats
// the dash, brief headings ambiguous, no human types them on purpose).
var labelRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidateLabel returns nil if s is a valid bosun label. Used at init time
// to reject malformed named-session args before any branches are created.
// Also enforced for label-derived heading parsing in the brief package via
// its own regex (kept in sync deliberately — see internal/brief/brief.go).
func ValidateLabel(s string) error {
	if !labelRe.MatchString(s) {
		return fmt.Errorf("invalid session label %q (want lowercase letters/digits separated by single dashes, starting with a letter and not ending with a dash)", s)
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
// Example: WorktreePath("/code/myproj", cfg, 3) => "/code/myproj-bosun-3".
func WorktreePath(repoRoot string, cfg config.Config, n int) string {
	return WorktreePathForLabel(repoRoot, cfg, cfg.SessionName(n))
}

// WorktreePathForLabel returns the canonical worktree path for a session
// label. Numeric ("session-3") and named ("auth") labels share the same
// computation — only the suffix differs.
// Example: WorktreePathForLabel("/code/myproj", cfg, "auth") => "/code/myproj-bosun-auth".
func WorktreePathForLabel(repoRoot string, cfg config.Config, label string) string {
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)
	return filepath.Join(parent, base+cfg.WorktreeSuffixForLabel(label))
}
