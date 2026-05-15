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
)

// State is the merge-readiness state of a session.
type State string

const (
	StateWorking State = "WORKING"
	StateDone    State = "DONE"
	StateStuck   State = "STUCK"
)

// Session is the aggregated view of one bosun-managed session.
type Session struct {
	Number      int           // 1-based session number
	Name        string        // e.g. "session-1"
	Branch      string        // e.g. "bosun/session-1"
	Path        string        // absolute worktree path
	Ahead       int           // commits ahead of base
	Dirty       int           // count of tracked-file changes
	Claimed     int           // count of distinct claimed paths
	State       State         // WORKING / DONE / STUCK
	StateMsg    string        // optional body of the state file
	Last        *git.LogEntry // last commit ahead of base (nil if none)
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

	branchRe := regexp.MustCompile(`^refs/heads/` + regexp.QuoteMeta(cfg.SessionPrefix) + `/session-(\d+)$`)

	var result []Session
	for _, wt := range worktrees {
		m := branchRe.FindStringSubmatch(wt.Branch)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 1 {
			continue
		}

		name := cfg.SessionName(n)
		branch := cfg.BranchFor(n)

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

		result = append(result, Session{
			Number:   n,
			Name:     name,
			Branch:   branch,
			Path:     wt.Path,
			Ahead:    ahead,
			Dirty:    dirty,
			Claimed:  claimed,
			State:    state,
			StateMsg: msg,
			Last:     last,
		})
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

// ParseName accepts either "session-N" or "N" and returns the integer N.
// Returns an error on anything else.
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

// WorktreePath returns the canonical worktree path for session N relative to
// the repo's parent dir. Example: WorktreePath("/code/myproj", cfg, 3) =>
// "/code/myproj-bosun-3".
func WorktreePath(repoRoot string, cfg config.Config, n int) string {
	parent := filepath.Dir(repoRoot)
	base := filepath.Base(repoRoot)
	return filepath.Join(parent, base+cfg.WorktreeSuffix(n))
}
