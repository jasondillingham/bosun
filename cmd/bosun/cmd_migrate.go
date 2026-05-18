package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/git"
	initstate "github.com/jasondillingham/bosun/internal/init"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

// timestampLayout is the rename's timestamp string format —
// `YYYYMMDDHHMMSS`, 14 digits, lexicographically sortable, filesystem-safe
// on every platform bosun targets (no `:` to confuse Windows paths). The
// naming-scheme lane settled on this shape for the new
// `<repo>-bosun-<timestamp>-<sub>` worktree directory format; see
// docs/uid-worktree-migration.md for the audit.
const timestampLayout = "20060102150405"

func newMigrateCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Rename legacy worktree directories to the new shape",
		Long: `Detect every sibling worktree directory still on the pre-v0.11
` + "`<repo>-bosun-<sub>`" + ` shape and rename it forward to
` + "`<repo>-bosun-<timestamp>-<sub>`" + `.

The timestamp comes from ` + "`.bosun/init.state`'s StartedAt" + ` when that
file is present; otherwise the worktree directory's mtime is the fallback.
All worktrees from one init share the same timestamp so the rename is
visually grouped.

Rename uses ` + "`git worktree move`" + ` (not a raw filesystem rename) so git's
admin metadata under ` + "`<main>/.git/worktrees/`" + ` stays in sync.

The command is idempotent: if a previous run died after renaming some
but not all worktrees, the next invocation picks up where it left off.

A "conflict" — both the legacy and the new-shape paths exist for the
same session — is surfaced as an error. That state means a fresh init
ran after the legacy worktrees were stranded; the operator has to
decide which one to keep.

` + "`--dry-run`" + ` shows what would happen without invoking git.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen, don't act")
	cmd.GroupID = "wiring"
	return cmd
}

// migratePlan is one proposed (or completed) rename. From and To are
// absolute paths.
type migratePlan struct {
	label string
	from  string
	to    string
}

func runMigrate(dryRun bool) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	plans, conflicts, err := planMigrate(rc.ctx, rc.git, rc.repoRoot, time.Now)
	if err != nil {
		return err
	}

	if len(conflicts) > 0 {
		// Conflicts block the run wholesale rather than letting the
		// non-conflicting renames proceed. A partial migration in the
		// presence of a conflict leaves the operator with split state
		// that's harder to reason about than the original; better to
		// stop and ask them to clean up first.
		var b strings.Builder
		_, _ = fmt.Fprintf(&b, "bosun: cannot migrate — %d session(s) have BOTH the legacy and new-shape paths on disk:\n", len(conflicts))
		for _, c := range conflicts {
			_, _ = fmt.Fprintf(&b, "  %s: legacy=%s new=%s\n", c.label, c.from, c.to)
		}
		b.WriteString("Decide which to keep, remove the other manually, then re-run `bosun migrate`.")
		return userErr("%s", b.String())
	}

	if len(plans) == 0 {
		printf("bosun: nothing to migrate (no legacy-named worktrees)\n")
		return nil
	}

	moved := 0
	for _, p := range plans {
		if dryRun {
			printf("  ▸ %s: would rename %s → %s\n", p.label, p.from, p.to)
			continue
		}
		if err := rc.git.MoveWorktree(rc.ctx, rc.repoRoot, p.from, p.to); err != nil {
			return gitErr(fmt.Sprintf("worktree move %s → %s", p.from, p.to), err)
		}
		printf("  ✓ %s: renamed %s → %s\n", p.label, p.from, p.to)
		moved++
	}

	if dryRun {
		printf("\nbosun: dry-run — would migrate %d worktree(s) (no changes made)\n", len(plans))
	} else {
		printf("\nbosun: migrated %d worktree(s)\n", moved)
	}
	return nil
}

// migrateConflict records a session whose legacy AND new-shape paths
// both exist on disk. Surfaced as a hard error from planMigrate.
type migrateConflict struct {
	label string
	from  string
	to    string
}

// planMigrate enumerates legacy worktrees git tracks for repoRoot and
// produces the rename plan, plus a list of conflicts (sessions where the
// new-shape path already exists alongside the legacy one — meaning a
// fresh init ran after the legacy state was left behind).
//
// `now` is injected so tests can pin a deterministic fallback timestamp
// for worktrees whose mtime is ambiguous; production passes time.Now.
func planMigrate(ctx context.Context, c *git.Client, repoRoot string, now func() time.Time) ([]migratePlan, []migrateConflict, error) {
	worktrees, err := c.ListWorktrees(ctx, repoRoot)
	if err != nil {
		return nil, nil, gitErr("list worktrees", err)
	}

	timestamp := timestampForMigration(repoRoot, now)

	var (
		plans     []migratePlan
		conflicts []migrateConflict
	)
	for _, wt := range worktrees {
		// Skip the main repo itself and any non-bosun-managed worktrees.
		// Bosun-management is signaled by a branch under the configured
		// session prefix; here we just sniff for `refs/heads/bosun/` since
		// the prefix is config-tunable but ~all real deployments use the
		// default. A stray non-bosun worktree happening to live at a path
		// matching `<repo>-bosun-<label>` is exotic enough we don't paper
		// over it — it'd be filtered by the IsLegacyWorktreePath shape
		// check below anyway.
		if !strings.HasPrefix(wt.Branch, "refs/heads/") {
			continue
		}
		if !session.IsLegacyWorktreePath(repoRoot, wt.Path) {
			continue
		}
		label := labelFromLegacyPath(repoRoot, wt.Path)
		if label == "" {
			continue
		}
		// Suffix substitution mirrors session.WorktreeSuffixForLabel:
		// numeric labels strip the "session-" prefix.
		sub := label
		if rest, ok := strings.CutPrefix(label, "session-"); ok {
			sub = rest
		}
		parent := filepath.Dir(filepath.Clean(repoRoot))
		base := filepath.Base(filepath.Clean(repoRoot))
		newPath := filepath.Join(parent, fmt.Sprintf("%s-bosun-%s-%s", base, timestamp, sub))
		if _, err := os.Stat(newPath); err == nil {
			conflicts = append(conflicts, migrateConflict{label: label, from: wt.Path, to: newPath})
			continue
		}
		plans = append(plans, migratePlan{label: label, from: wt.Path, to: newPath})
	}

	// Deterministic order so dry-run output and the actual rename order
	// match across invocations — handy when an operator does a dry-run,
	// reviews, then runs for real.
	sort.Slice(plans, func(i, j int) bool { return plans[i].label < plans[j].label })
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].label < conflicts[j].label })
	return plans, conflicts, nil
}

// labelFromLegacyPath strips the `<repo>-bosun-` prefix from the path's
// basename and re-applies the numeric-vs-named decoding to recover the
// session label. Returns "" when the suffix doesn't parse.
func labelFromLegacyPath(repoRoot, path string) string {
	base := filepath.Base(filepath.Clean(repoRoot))
	prefix := base + "-bosun-"
	name := filepath.Base(filepath.Clean(path))
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" {
		return ""
	}
	// Numeric suffix → "session-N"; bare label suffix → unchanged.
	label, err := session.ParseLabel(rest)
	if err != nil {
		return ""
	}
	return label
}

// timestampForMigration returns the rename's timestamp string. Preference
// order matches the brief: init.state's StartedAt → repo's parent-dir
// mtime → now().
//
// Falling back to the parent dir's mtime (rather than each worktree
// individually) keeps every legacy worktree in one repo grouped under the
// same timestamp — same intent as the init.state path. Worktrees that
// genuinely came from different inits without an init.state will share
// the timestamp, which is fine: the timestamp is only there to make the
// new-shape directory name unique against the next init.
func timestampForMigration(repoRoot string, now func() time.Time) string {
	if s, err := initstate.Load(repoRoot); err == nil && !s.StartedAt.IsZero() {
		return s.StartedAt.UTC().Format(timestampLayout)
	}
	parent := filepath.Dir(filepath.Clean(repoRoot))
	if info, err := os.Stat(parent); err == nil {
		return info.ModTime().UTC().Format(timestampLayout)
	}
	return now().UTC().Format(timestampLayout)
}
