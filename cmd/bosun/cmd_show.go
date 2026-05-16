package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <session>",
		Short: "Inspect one session's brief, claims, state, and recent activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, args[0])
		},
	}
	return cmd
}

func runShow(cmd *cobra.Command, sessionArg string) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	label, err := session.ParseLabel(sessionArg)
	if err != nil {
		return userErr("%v", err)
	}
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	s := findSessionByLabel(sessions, label)
	if s == nil {
		return userErr("%s not found (use `bosun list` to see active sessions)", label)
	}

	fmt.Fprintf(os.Stdout, "Session:  %s\n", s.Name)
	fmt.Fprintf(os.Stdout, "Branch:   %s\n", s.Branch)
	fmt.Fprintf(os.Stdout, "Worktree: %s\n", s.Path)
	fmt.Fprintf(os.Stdout, "State:    %s", s.State)
	if s.StateMsg != "" {
		fmt.Fprintf(os.Stdout, "  (%s)", strings.ReplaceAll(s.StateMsg, "\n", " "))
	}
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "Ahead:    %d\n", s.Ahead)
	fmt.Fprintf(os.Stdout, "Dirty:    %d\n", s.Dirty)

	c, err := rc.claims.Read(s.Name)
	if err != nil {
		return internalErr("read claims", err)
	}
	if c != nil && len(c.Paths) > 0 {
		fmt.Fprintf(os.Stdout, "\nClaimed paths (%d):\n", len(c.Paths))
		for _, p := range c.Paths {
			fmt.Fprintf(os.Stdout, "  %s\n", p)
		}
	}

	if briefBody, err := brief.ReadFromWorktree(s.Path); err != nil {
		return internalErr("read brief", err)
	} else if briefBody != "" {
		fmt.Fprintln(os.Stdout, "\n--- BOSUN_BRIEF.md ---")
		fmt.Fprint(os.Stdout, briefBody)
		if !strings.HasSuffix(briefBody, "\n") {
			fmt.Fprintln(os.Stdout)
		}
		fmt.Fprintln(os.Stdout, "----------------------")
	}

	fmt.Fprintln(os.Stdout, "\n--- last 10 commits ---")
	log, err := rc.git.LogN(rc.ctx, s.Path, 10)
	if err != nil {
		return gitErr("git log", err)
	}
	if log == "" {
		fmt.Fprintln(os.Stdout, "(no commits)")
	} else {
		fmt.Fprint(os.Stdout, log)
	}

	fmt.Fprintln(os.Stdout, "\n--- git status ---")
	lines, err := rc.git.Status(rc.ctx, s.Path)
	if err != nil {
		return gitErr("git status", err)
	}
	if len(lines) == 0 {
		fmt.Fprintln(os.Stdout, "(clean)")
	} else {
		for _, l := range lines {
			fmt.Fprintf(os.Stdout, "%s %s\n", l.XY, l.Path)
		}
	}
	return nil
}
