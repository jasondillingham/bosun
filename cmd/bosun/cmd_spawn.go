package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawn"
	"github.com/jasondillingham/bosun/internal/spawntree"
	"github.com/spf13/cobra"
)

// newSpawnCmd builds `bosun spawn` — the operator-facing equivalent
// of the bosun_spawn MCP tool. Lets the operator test the spawn
// pipeline without going through an agent, or kick off a multi-lane
// sub-round from the keyboard when the parent agent is paused.
//
// The depth + quota gates that govern WHETHER to spawn still apply.
// What this command skips relative to the MCP tool is the "calling
// agent must be running inside the parent worktree" liveness check —
// the operator is the parent's authority by definition; if they want
// to spawn from the CLI, they get to.
//
// 2026-05 follow-up grind #99 — closes the "spawn is invisible
// without an agent" UX gap that made bosun_spawn hard to evaluate.
func newSpawnCmd() *cobra.Command {
	var (
		briefPath string
		launch    bool
	)
	cmd := &cobra.Command{
		Use:   "spawn <parent>",
		Short: "Spawn sub-sessions under <parent> from a brief markdown",
		Long: `Operator-side analogue of the bosun_spawn MCP tool. Reads a brief
file containing one or more ` + "`## <suffix>`" + ` headings and creates a
sub-session per heading under the given parent.

Parent must be an existing bosun-managed session. Each new sub-
session lands at <parent>.<suffix> with its own worktree + branch +
brief, recorded in .bosun/spawn-tree.json. Depth and per-parent
concurrent-spawn quotas from config.agent_spawn still apply.

Mirrors bosun_spawn semantics — same audit log, same spawn-tree
mutations, same per-sub-session pipeline — except the liveness gate
that requires the calling agent to be running inside the parent
worktree is skipped (the operator running this is the parent's
authority).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSpawn(args[0], briefPath, launch)
		},
	}
	cmd.Flags().StringVar(&briefPath, "brief", "", "path to the sub-session brief markdown (required)")
	cmd.Flags().BoolVar(&launch, "launch", false, "spawn an agent in each new sub-session's worktree (mirrors bosun init --launch)")
	_ = cmd.MarkFlagRequired("brief")
	cmd.GroupID = "during"
	return cmd
}

func runSpawn(parentArg, briefPath string, launch bool) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}
	parentLabel, err := session.ParseLabel(parentArg)
	if err != nil {
		return userErr("%v", err)
	}

	// Config gate. agent_spawn.enabled=false (the default) means the
	// operator hasn't opted into spawn for this repo. Refuse with the
	// exact one-liner they need to flip it.
	if !rc.cfg.AgentSpawn.Enabled {
		return userErr("agent_spawn is disabled in this repo.\n"+
			"  Enable it: bosun config set agent_spawn.enabled true\n"+
			"  Tune defaults: agent_spawn.max_depth (current %d), agent_spawn.max_concurrent_sub_sessions (current %d)",
			rc.cfg.AgentSpawn.MaxDepth, rc.cfg.AgentSpawn.MaxConcurrentSubSessions)
	}

	// Parent must exist + be in the allowed-for-sessions list (if set).
	sessions, err := session.Derive(rc.ctx, rc.git, rc.cfg, rc.repoRoot, rc.state, rc.claims)
	if err != nil {
		return gitErr("derive sessions", err)
	}
	if findSessionByLabel(sessions, parentLabel) == nil {
		return userErr("%s not found (use `bosun list` to see active sessions)", parentLabel)
	}
	if len(rc.cfg.AgentSpawn.AllowedForSessions) > 0 {
		allowed := false
		for _, l := range rc.cfg.AgentSpawn.AllowedForSessions {
			if l == parentLabel {
				allowed = true
				break
			}
		}
		if !allowed {
			return userErr("%s is not in agent_spawn.allowed_for_sessions (currently: %v)",
				parentLabel, rc.cfg.AgentSpawn.AllowedForSessions)
		}
	}

	// Depth + quota gates. Same checks the MCP tool runs — kept here so
	// the CLI path doesn't bypass the safety contract.
	tree := spawntree.NewStore(rc.repoRoot)
	parentDepth, err := tree.DepthOf(parentLabel)
	if err != nil {
		return internalErr("read spawn tree", err)
	}
	if parentDepth+1 > rc.cfg.AgentSpawn.MaxDepth {
		return userErr("spawn depth %d would exceed agent_spawn.max_depth=%d (parent is at depth %d)",
			parentDepth+1, rc.cfg.AgentSpawn.MaxDepth, parentDepth)
	}
	existing, err := tree.CountChildren(parentLabel)
	if err != nil {
		return internalErr("count children", err)
	}
	if existing >= rc.cfg.AgentSpawn.MaxConcurrentSubSessions {
		return userErr("%s already has %d live sub-session(s); agent_spawn.max_concurrent_sub_sessions=%d",
			parentLabel, existing, rc.cfg.AgentSpawn.MaxConcurrentSubSessions)
	}

	// Read the brief from disk. ParseString validation runs inside
	// spawn.Run so we don't double-parse here — but we do need to slurp
	// the bytes so they're inline by the time Run gets the request.
	briefBody, err := os.ReadFile(briefPath)
	if err != nil {
		return userErr("read brief %s: %v", briefPath, err)
	}
	// Sanity-pre-parse so we can fail fast with a clear error before
	// any branch/worktree machinery runs.
	if _, err := brief.ParseString(string(briefBody)); err != nil {
		return userErr("brief %s: %v", briefPath, err)
	}

	res, runErr := spawn.Run(context.Background(), rc.git, tree, spawn.Request{
		RepoRoot:      rc.repoRoot,
		ParentLabel:   parentLabel,
		BriefMarkdown: string(briefBody),
		Launch:        launch,
		Cfg:           rc.cfg,
	})
	if runErr != nil && len(res.Created) == 0 {
		return userErr("spawn: %v", runErr)
	}

	if len(res.Created) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "Created %d sub-session(s) under %s:\n", len(res.Created), parentLabel)
		for _, label := range res.Created {
			_, _ = fmt.Fprintf(os.Stdout, "  ✓ %s\n", label)
		}
	}
	if len(res.Failed) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "\n%d failure(s):\n", len(res.Failed))
		for _, f := range res.Failed {
			_, _ = fmt.Fprintf(os.Stdout, "  ✗ %s: %s\n", f.Label, f.Reason)
		}
		if len(res.Created) == 0 {
			return userErr("no sub-sessions created")
		}
	}
	return nil
}
