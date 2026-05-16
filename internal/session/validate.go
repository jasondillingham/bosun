package session

import "fmt"

// DoneableError describes why a session can't be marked DONE. Callers (the
// CLI and the MCP tool) translate it into their own error surface — exit
// codes for the former, structured tool errors for the latter — but the
// validation rules live in one place.
type DoneableError struct {
	Session    string
	Reason     DoneableReason
	Dirty      int
	BaseBranch string
}

// DoneableReason enumerates the failure modes returned from ValidateDoneable.
type DoneableReason string

const (
	// DoneableReasonDirty: the worktree has uncommitted tracked-file changes.
	DoneableReasonDirty DoneableReason = "dirty"
	// DoneableReasonNoCommits: no commits ahead of the base branch.
	DoneableReasonNoCommits DoneableReason = "no_commits"
)

func (e *DoneableError) Error() string {
	switch e.Reason {
	case DoneableReasonDirty:
		return fmt.Sprintf("%s has %d uncommitted change(s); commit them first or pass --force", e.Session, e.Dirty)
	case DoneableReasonNoCommits:
		return fmt.Sprintf("%s has no commits ahead of %s; use `bosun remove` instead, or pass --force", e.Session, e.BaseBranch)
	}
	return fmt.Sprintf("%s is not doneable", e.Session)
}

// ValidateDoneable reports whether a session passes the gate that `bosun
// done` and the bosun_done MCP tool enforce when --force is not set:
// no uncommitted changes AND at least one commit ahead of base.
func ValidateDoneable(s Session, baseBranch string) error {
	if s.Dirty > 0 {
		return &DoneableError{Session: s.Name, Reason: DoneableReasonDirty, Dirty: s.Dirty, BaseBranch: baseBranch}
	}
	if s.Ahead == 0 {
		return &DoneableError{Session: s.Name, Reason: DoneableReasonNoCommits, BaseBranch: baseBranch}
	}
	return nil
}
