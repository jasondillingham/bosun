package session

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateDoneable_DirtyFails(t *testing.T) {
	s := Session{Name: "session-1", Dirty: 2, Ahead: 1}
	err := ValidateDoneable(s, "main")
	if err == nil {
		t.Fatal("expected dirty session to fail validation")
	}
	var de *DoneableError
	if !errors.As(err, &de) || de.Reason != DoneableReasonDirty {
		t.Fatalf("want DoneableError(dirty), got %v", err)
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("message should mention uncommitted: %q", err.Error())
	}
}

func TestValidateDoneable_NoCommitsFails(t *testing.T) {
	s := Session{Name: "session-1", Ahead: 0}
	err := ValidateDoneable(s, "main")
	var de *DoneableError
	if !errors.As(err, &de) || de.Reason != DoneableReasonNoCommits {
		t.Fatalf("want DoneableError(no_commits), got %v", err)
	}
	if !strings.Contains(err.Error(), "main") {
		t.Errorf("message should name the base branch: %q", err.Error())
	}
}

func TestValidateDoneable_Passes(t *testing.T) {
	s := Session{Name: "session-1", Ahead: 1, Dirty: 0}
	if err := ValidateDoneable(s, "main"); err != nil {
		t.Fatalf("clean+ahead should pass, got %v", err)
	}
}
