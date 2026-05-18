// Package spawn implements the per-sub-session create pipeline used
// by the v0.9 `bosun_spawn` MCP tool. Given a parent session, an
// inline brief, and the relevant stores, it creates each sub-session
// the brief describes — branch, worktree, brief file, optional
// launcher — and records the parent-child relationship in the spawn
// tree.
//
// The full bosun init pipeline (cmd/bosun/cmd_init.go) does
// substantially more than this: pre-flight checks, --force cleanup,
// --resume reconciliation, init.state breadcrumbs, archive-plan,
// pre-init/post-init hooks. Spawn is the trimmed agent-driven
// version: the parent agent already proved the environment is
// healthy by being there, so the pre-flight stuff isn't load-bearing
// the way it is for the operator-keyboard entry path.
package spawn

import (
	"context"
	"fmt"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/git"
	"github.com/jasondillingham/bosun/internal/launcher"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/jasondillingham/bosun/internal/spawntree"
)

// Request is the input to Run.
type Request struct {
	// RepoRoot is the main worktree path (where .bosun/ lives).
	RepoRoot string

	// ParentLabel is the calling session's own label. The new
	// sub-sessions are created as ParentLabel.<suffix> for each
	// heading suffix in BriefMarkdown.
	ParentLabel string

	// BriefMarkdown is the brief content (same shape as a brief
	// file). Headings `## suffix` become sub-session suffixes.
	BriefMarkdown string

	// Launch, when true, spawns an agent in each new sub-session's
	// worktree via the launcher package. False creates the worktrees
	// + briefs without launching.
	Launch bool

	// Cfg supplies bosun's resolved config (VerifyCmd, SessionPrefix,
	// WorktreeSuffixPattern, etc.). The caller — typically the MCP
	// server — already has this loaded.
	Cfg config.Config
}

// Result reports per-sub outcomes from Run.
type Result struct {
	// Created lists the full sub-session labels (parent.suffix) that
	// were successfully created end-to-end.
	Created []string
	// Failed lists per-sub failures with the label that didn't make it
	// and the reason. A non-empty Failed slice + non-empty Created
	// slice is a partial success — some subs landed, others didn't.
	Failed []Failure
}

// Failure pairs a label with the human-readable reason it couldn't be
// created. Reasons cover brief-parse errors, label collisions, git
// failures, and (above the pipeline) auth/quota/depth refusals.
type Failure struct {
	Label  string
	Reason string
}

// Run performs the per-sub-session create pipeline. The caller is
// responsible for the auth + quota + depth gates that govern WHETHER
// to spawn at all — Run trusts that the request is allowed. It
// records each successful sub-session in the spawn tree before
// returning.
//
// On launcher failure the worktree is left in place — the operator
// can `bosun launch <sub-label>` later. On git failure the worktree
// is rolled back (RemoveWorktree + DeleteBranch best-effort) and the
// label lands in Failed.
func Run(ctx context.Context, client *git.Client, tree *spawntree.Store, req Request) (Result, error) {
	var res Result

	briefs, err := brief.ParseString(req.BriefMarkdown)
	if err != nil {
		return res, fmt.Errorf("parse brief: %w", err)
	}
	if len(briefs) == 0 {
		return res, fmt.Errorf("brief has no `## <suffix>` headings")
	}

	for _, b := range briefs {
		// b.Label is the suffix the agent wrote (e.g. "auth"). The
		// full sub-session label is parent.suffix.
		full := req.ParentLabel + "." + b.Label
		if err := session.ValidateLabel(full); err != nil {
			res.Failed = append(res.Failed, Failure{Label: full, Reason: fmt.Sprintf("invalid label: %v", err)})
			continue
		}

		branch := req.Cfg.SessionPrefix + "/" + full
		path := session.WorktreePathForLabel(req.RepoRoot, req.Cfg, full, "")

		// Branch from parent's branch — hierarchical model per the
		// v0.9 spec. The parent's HEAD is the base for the sub so the
		// agent's already-committed work shows up in the sub's tree.
		parentBranch := req.Cfg.SessionPrefix + "/" + req.ParentLabel
		if err := client.CreateBranch(ctx, req.RepoRoot, branch, parentBranch); err != nil {
			res.Failed = append(res.Failed, Failure{Label: full, Reason: fmt.Sprintf("create branch: %v", err)})
			continue
		}
		if err := client.AddWorktree(ctx, req.RepoRoot, path, branch); err != nil {
			res.Failed = append(res.Failed, Failure{Label: full, Reason: fmt.Sprintf("add worktree: %v", err)})
			// Roll back the branch we just created so a retry isn't
			// stuck on a stale-branch refusal.
			_ = client.DeleteBranch(ctx, req.RepoRoot, branch, true)
			continue
		}

		// Mirror cmd_init.go's exclude additions so BOSUN_BRIEF.md and
		// the .claude/CLAUDE.md pointer don't end up in commits.
		_ = client.AppendWorktreeExclude(ctx, path, "BOSUN_BRIEF.md")
		_ = client.AppendWorktreeExclude(ctx, path, ".claude/CLAUDE.md")

		// Re-label the brief to its full sub-session form before
		// writing so the brief's "session-1.auth" heading matches the
		// branch + worktree name.
		bWithFull := b
		bWithFull.Label = full
		if err := brief.WriteToWorktree(path, bWithFull, req.Cfg.VerifyCmd); err != nil {
			res.Failed = append(res.Failed, Failure{Label: full, Reason: fmt.Sprintf("write brief: %v", err)})
			_ = client.RemoveWorktree(ctx, req.RepoRoot, path, true)
			_ = client.DeleteBranch(ctx, req.RepoRoot, branch, true)
			continue
		}
		if err := brief.WriteSessionPointer(path, full); err != nil {
			res.Failed = append(res.Failed, Failure{Label: full, Reason: fmt.Sprintf("write session pointer: %v", err)})
			_ = client.RemoveWorktree(ctx, req.RepoRoot, path, true)
			_ = client.DeleteBranch(ctx, req.RepoRoot, branch, true)
			continue
		}

		// Record in the spawn tree BEFORE launching so a launcher
		// failure doesn't leave a child untracked. AddChild's
		// idempotency check also serves as a collision detector — if
		// the label is already tracked, the spawn aborts cleanly.
		if err := tree.AddChild(req.ParentLabel, full); err != nil {
			res.Failed = append(res.Failed, Failure{Label: full, Reason: fmt.Sprintf("record spawn tree: %v", err)})
			_ = client.RemoveWorktree(ctx, req.RepoRoot, path, true)
			_ = client.DeleteBranch(ctx, req.RepoRoot, branch, true)
			continue
		}

		if req.Launch {
			if _, err := launcher.Launch(launcher.Options{
				WorktreePath: path,
				SessionName:  full,
				Strategy:     launcher.Strategy(req.Cfg.Launcher),
				// Match cmd_init.go's --launch behavior: no initial
				// prompt by default; the v0.7 launch UX default fills
				// in "Read BOSUN_BRIEF.md..." when the worktree has
				// one (which we just wrote). Operators wanting custom
				// prompts can launch sub-sessions manually after.
			}); err != nil {
				// Launcher failure is non-fatal — the worktree is
				// usable; operator can `bosun launch <full>` to
				// retry. Record as a soft failure but keep the
				// sub-session in Created (it exists; an agent just
				// didn't start).
				res.Failed = append(res.Failed, Failure{Label: full, Reason: fmt.Sprintf("launch (worktree created; sub-session usable): %v", err)})
			}
		}

		res.Created = append(res.Created, full)
	}

	if len(res.Created) == 0 && len(res.Failed) > 0 {
		return res, fmt.Errorf("no sub-sessions created; %d failure(s)", len(res.Failed))
	}
	return res, nil
}
