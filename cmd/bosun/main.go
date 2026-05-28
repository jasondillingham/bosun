package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

const (
	exitOK       = 0
	exitUserErr  = 1
	exitGitErr   = 2
	exitInternal = 3
	// exitConflict signals a workflow that halted because a session
	// couldn't be applied automatically — most commonly `bosun merge`
	// hitting an unresolved git conflict that needs the operator to
	// pick a resolution. Distinct from exitUserErr so CI scripts can
	// branch on "needs hands" vs "the invocation was wrong." Added per
	// Bughunt-1 F032: pre-fix `bosun merge` returned exit 0 on conflict
	// and CI scripts marched on top of a wedged working tree.
	exitConflict = 4
)

// errKind lets a command return a sentinel error class so main can map it to
// an exit code. Most user errors come out of Cobra's RunE; we wrap them.
type errKind int

const (
	kindUser errKind = iota
	kindGit
	kindInternal
	kindConflict
)

type bosunError struct {
	kind errKind
	msg  string
	wrap error
}

func (e *bosunError) Error() string {
	if e.wrap != nil {
		return fmt.Sprintf("bosun: %s: %v", e.msg, e.wrap)
	}
	return "bosun: " + e.msg
}

func (e *bosunError) Unwrap() error { return e.wrap }

func userErr(msg string, args ...any) error {
	return &bosunError{kind: kindUser, msg: fmt.Sprintf(msg, args...)}
}

func gitErr(msg string, err error) error {
	return &bosunError{kind: kindGit, msg: msg, wrap: err}
}

func internalErr(msg string, err error) error {
	return &bosunError{kind: kindInternal, msg: msg, wrap: err}
}

// conflictErr signals that a workflow couldn't proceed because of an
// unresolved conflict that needs operator intervention (typically a
// merge conflict in `bosun merge`). The command that returns this is
// expected to have already printed the operator-visible recovery
// message — main translates it to exitConflict (4) and the command
// silences Cobra's err-print to avoid duplicating that message.
func conflictErr(msg string) error {
	return &bosunError{kind: kindConflict, msg: msg}
}

func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	var be *bosunError
	if errors.As(err, &be) {
		switch be.kind {
		case kindGit:
			return exitGitErr
		case kindInternal:
			return exitInternal
		case kindConflict:
			return exitConflict
		}
	}
	return exitUserErr
}

func main() {
	if _, err := exec.LookPath("git"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "bosun: git binary not found on PATH")
		os.Exit(exitUserErr)
	}

	cmd := newRootCmd()
	if err := cmd.Execute(); err != nil {
		// Cobra already prints the error; we just translate to an exit code.
		os.Exit(exitCodeFor(err))
	}
}
