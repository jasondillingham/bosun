package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jasondillingham/bosun/internal/suggest"
	"github.com/spf13/cobra"
)

// suggestOpts mirrors the command's flag set. Kept separate from
// suggestDeps so the runner is easy to drive from tests that supply
// their own stub Inspector/Proposer.
type suggestOpts struct {
	goal          string
	sessions      int
	out           string
	model         string
	maxTokens     int
	inspectOnly   bool
	allowOverlaps bool
}

// suggestDeps is the injection seam for tests. Production wiring fills
// these with internal/suggest's real Inspect + ClaudeProposer.
type suggestDeps struct {
	inspector suggest.Inspector
	// proposer may be nil when opts.inspectOnly is true — the runner
	// short-circuits before calling it.
	proposer suggest.Proposer
}

// inspectorFunc adapts suggest.Inspect (a plain function) into the
// suggest.Inspector interface the runner consumes.
type inspectorFunc func(string) (suggest.RepoIntel, error)

func (f inspectorFunc) Inspect(root string) (suggest.RepoIntel, error) { return f(root) }

func newSuggestCmd() *cobra.Command {
	var opts suggestOpts

	cmd := &cobra.Command{
		Use:   "suggest <goal>",
		Short: "Propose N parallel session lanes for a goal and write a plan markdown",
		Long: `Inspects the current repo, asks Claude to propose <sessions> disjoint
lanes that satisfy the goal, validates the proposal, and writes the
result as a plan markdown the operator can feed into ` + "`bosun init --brief`" + `.

The goal may span multiple positional args (joined with spaces) or be
passed as a single quoted string.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.goal = strings.TrimSpace(strings.Join(args, " "))
			if opts.goal == "" {
				return userErr("goal must not be empty")
			}

			rc, err := loadCtx()
			if err != nil {
				return err
			}
			if opts.sessions == 0 {
				opts.sessions = rc.cfg.DefaultSessionCount
			}
			if opts.sessions < 1 {
				return userErr("--sessions must be ≥ 1, got %d", opts.sessions)
			}

			deps := suggestDeps{
				inspector: inspectorFunc(suggest.Inspect),
			}
			if !opts.inspectOnly {
				sugCfg := rc.cfg.Suggest
				if opts.model != "" {
					sugCfg.Model = opts.model
				}
				if opts.maxTokens > 0 {
					sugCfg.MaxTokens = opts.maxTokens
				}
				proposer, err := suggest.NewClaudeProposer(suggest.ClaudeProposerOptions{
					Model:     sugCfg.Model,
					MaxTokens: sugCfg.MaxTokens,
					APIKeyEnv: sugCfg.APIKeyEnv,
				})
				if err != nil {
					return userErr("%v", err)
				}
				deps.proposer = proposer
			}

			return runSuggest(rc.ctx, cmd.OutOrStdout(), rc.repoRoot, opts, deps)
		},
	}

	cmd.Flags().IntVar(&opts.sessions, "sessions", 0, "number of session lanes to propose (default: config.default_session_count)")
	cmd.Flags().StringVar(&opts.out, "out", "suggested-plan.md", "path to write the plan markdown")
	cmd.Flags().StringVar(&opts.model, "model", "", "Claude model id (overrides config.suggest.model)")
	cmd.Flags().BoolVar(&opts.inspectOnly, "inspect-only", false, "print RepoIntel JSON to stdout and exit; do not call the API")
	cmd.Flags().IntVar(&opts.maxTokens, "max-tokens", 0, "max tokens in the Claude response (overrides config.suggest.max_tokens)")
	cmd.Flags().BoolVar(&opts.allowOverlaps, "allow-overlaps", false, "surface validator overlap/cycle errors as warnings and write the plan anyway")

	cmd.GroupID = "setup"
	return cmd
}

// runSuggest is the testable command body. It runs the inspector, then
// (unless --inspect-only) the proposer + validator + renderer, then
// writes the plan to opts.out and prints a one-liner success summary.
func runSuggest(ctx context.Context, w io.Writer, repoRoot string, opts suggestOpts, deps suggestDeps) error {
	if deps.inspector == nil {
		return internalErr("suggest", errors.New("inspector not configured"))
	}

	intel, err := deps.inspector.Inspect(repoRoot)
	if err != nil {
		return internalErr("inspect repo", err)
	}

	if opts.inspectOnly {
		data, err := json.MarshalIndent(intel, "", "  ")
		if err != nil {
			return internalErr("marshal repo intel", err)
		}
		_, _ = fmt.Fprintln(w, string(data))
		return nil
	}

	if deps.proposer == nil {
		return internalErr("suggest", errors.New("proposer not configured"))
	}

	proposal, err := deps.proposer.Propose(ctx, opts.goal, intel, opts.sessions)
	if err != nil {
		return userErr("propose lanes: %v", err)
	}

	warnings, vErr := suggest.Validate(proposal, opts.sessions)
	if vErr != nil {
		// Structural details on the two errors the brief calls out.
		// Other validation errors (schema, label charset, unknown deps)
		// are always fatal — --allow-overlaps is specifically about
		// overlap/cycle, not free-pass on every check.
		var overlapErr *suggest.OverlapError
		var cycleErr *suggest.CycleError
		switch {
		case errors.As(vErr, &overlapErr):
			_, _ = fmt.Fprintf(w, "bosun: validator overlap: lane %q pattern %q overlaps lane %q pattern %q\n",
				overlapErr.LaneA, overlapErr.PatternA, overlapErr.LaneB, overlapErr.PatternB)
		case errors.As(vErr, &cycleErr):
			_, _ = fmt.Fprintf(w, "bosun: validator cycle: %s\n", strings.Join(cycleErr.Cycle, " → "))
		default:
			return userErr("validate plan: %v", vErr)
		}
		if !opts.allowOverlaps {
			return userErr("plan failed validation; rerun with --allow-overlaps to write it anyway")
		}
		_, _ = fmt.Fprintln(w, "bosun: --allow-overlaps set, writing plan despite validation error")
	}
	for _, msg := range warnings {
		_, _ = fmt.Fprintf(w, "bosun: warning: %s\n", msg)
	}

	md := suggest.RenderPlanMarkdown(proposal)
	if err := os.WriteFile(opts.out, []byte(md), 0o644); err != nil {
		return internalErr("write plan "+opts.out, err)
	}

	_, _ = fmt.Fprintf(w, "Wrote plan to %s (%d sessions).\n", opts.out, len(proposal.Sessions))
	_, _ = fmt.Fprintf(w, "Next: review the plan, then run `bosun init --brief %s`.\n", opts.out)
	return nil
}
