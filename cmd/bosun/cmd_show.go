package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/jasondillingham/bosun/internal/status"
	"github.com/spf13/cobra"
)

// showJSON is the stable wire shape behind `bosun show <session> --json`.
//
// Schema (versioned by status.JSONSchemaVersion; additive changes keep the
// same version, key renames or removals are breaking and bump it):
//
//	{
//	  "version":        string,
//	  "name":           string,
//	  "branch":         string,
//	  "worktree":       string,    // absolute worktree path
//	  "state":          string,    // "WORKING" | "DONE" | "STUCK"
//	  "state_msg":      string,    // body of .done/.stuck marker, "" when blank
//	  "ahead":          int,       // commits ahead of base branch
//	  "dirty":          int,       // count of uncommitted tracked-file changes
//	  "claimed_paths":  []string,  // claim file's Paths, [] when none
//	  "recent_commits": string,    // raw output of `git log -10 --oneline --decorate`, "" when none
//	  "brief":          string     // full BOSUN_BRIEF.md contents, "" when missing
//	}
//
// `claimed_paths` is always a JSON array (never null). `recent_commits` and
// `brief` are raw strings — they may contain newlines and be large.
type showJSON struct {
	Version       string   `json:"version"`
	Name          string   `json:"name"`
	Branch        string   `json:"branch"`
	Worktree      string   `json:"worktree"`
	State         string   `json:"state"`
	StateMsg      string   `json:"state_msg"`
	Ahead         int      `json:"ahead"`
	Dirty         int      `json:"dirty"`
	ClaimedPaths  []string `json:"claimed_paths"`
	RecentCommits string   `json:"recent_commits"`
	Brief         string   `json:"brief"`
	// AgentCommand is the resolved agent for this session — the
	// per-session override from the brief / init --command flag, or
	// the empty string when the session uses config.AgentCommand.
	// Tooling that wants the effective command can OR with the
	// config default (also exposed in `bosun config get
	// agent_command`). Always present (per docs/json-schema.md F5,
	// matching brief / state_msg / recent_commits).
	AgentCommand string `json:"agent_command"`
}

func newShowCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "show <session>",
		Short: "Inspect one session's brief, claims, state, and recent activity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, args[0], jsonOut)
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")

	cmd.GroupID = "during"
	return cmd
}

func runShow(cmd *cobra.Command, sessionArg string, jsonOut bool) error {
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

	if jsonOut {
		return renderShowJSON(rc, s)
	}
	return renderShowText(rc, s)
}

func renderShowJSON(rc *runCtx, s *session.Session) error {
	payload := showJSON{
		Version:      status.JSONSchemaVersion,
		Name:         s.Name,
		Branch:       s.Branch,
		Worktree:     s.Path,
		State:        string(s.State),
		StateMsg:     s.StateMsg,
		Ahead:        s.Ahead,
		Dirty:        s.Dirty,
		ClaimedPaths: []string{},
		AgentCommand: s.AgentCommand,
	}

	c, err := rc.claims.Read(s.Name)
	if err != nil {
		return internalErr("read claims", err)
	}
	if c != nil && len(c.Paths) > 0 {
		payload.ClaimedPaths = append(payload.ClaimedPaths, c.Paths...)
	}

	briefBody, err := brief.ReadFromWorktree(s.Path)
	if err != nil {
		return internalErr("read brief", err)
	}
	payload.Brief = briefBody

	log, err := rc.git.LogN(rc.ctx, s.Path, 10)
	if err != nil {
		return gitErr("git log", err)
	}
	payload.RecentCommits = log

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return internalErr("encode json", err)
	}
	return nil
}

func renderShowText(rc *runCtx, s *session.Session) error {
	_, _ = fmt.Fprintf(os.Stdout, "Session:  %s\n", s.Name)
	_, _ = fmt.Fprintf(os.Stdout, "Branch:   %s\n", s.Branch)
	_, _ = fmt.Fprintf(os.Stdout, "Worktree: %s\n", s.Path)
	_, _ = fmt.Fprintf(os.Stdout, "State:    %s", s.State)
	if s.StateMsg != "" {
		_, _ = fmt.Fprintf(os.Stdout, "  (%s)", strings.ReplaceAll(s.StateMsg, "\n", " "))
	}
	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintf(os.Stdout, "Ahead:    %d\n", s.Ahead)
	_, _ = fmt.Fprintf(os.Stdout, "Dirty:    %d\n", s.Dirty)

	// Per-session agent override surfaces only when set. The vanilla
	// claude default stays out of the output so it doesn't add noise
	// for the common case.
	if s.AgentCommand != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Agent:    %s\n", s.AgentCommand)
	}

	// v0.9 spawn-tree info, if this session is part of one.
	if tree := spawntree.NewStore(rc.repoRoot); tree != nil {
		if parent, err := tree.ParentOf(s.Name); err == nil && parent != "" {
			_, _ = fmt.Fprintf(os.Stdout, "Parent:   %s\n", parent)
		}
		if children, err := tree.ChildrenOf(s.Name); err == nil && len(children) > 0 {
			_, _ = fmt.Fprintf(os.Stdout, "Children: %s\n", strings.Join(children, ", "))
		}
	}

	c, err := rc.claims.Read(s.Name)
	if err != nil {
		return internalErr("read claims", err)
	}
	if c != nil && len(c.Paths) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "\nClaimed paths (%d):\n", len(c.Paths))
		for _, p := range c.Paths {
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", p)
		}
	}

	if briefBody, err := brief.ReadFromWorktree(s.Path); err != nil {
		return internalErr("read brief", err)
	} else if briefBody != "" {
		_, _ = fmt.Fprintln(os.Stdout, "\n--- BOSUN_BRIEF.md ---")
		_, _ = fmt.Fprint(os.Stdout, briefBody)
		if !strings.HasSuffix(briefBody, "\n") {
			_, _ = fmt.Fprintln(os.Stdout)
		}
		_, _ = fmt.Fprintln(os.Stdout, "----------------------")
	}

	_, _ = fmt.Fprintln(os.Stdout, "\n--- last 10 commits ---")
	log, err := rc.git.LogN(rc.ctx, s.Path, 10)
	if err != nil {
		return gitErr("git log", err)
	}
	if log == "" {
		_, _ = fmt.Fprintln(os.Stdout, "(no commits)")
	} else {
		_, _ = fmt.Fprint(os.Stdout, log)
	}

	_, _ = fmt.Fprintln(os.Stdout, "\n--- git status ---")
	lines, err := rc.git.Status(rc.ctx, s.Path)
	if err != nil {
		return gitErr("git status", err)
	}
	if len(lines) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "(clean)")
	} else {
		for _, l := range lines {
			_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", l.XY, l.Path)
		}
	}
	return nil
}
