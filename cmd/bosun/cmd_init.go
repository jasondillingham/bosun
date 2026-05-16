package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/hooks"
	"github.com/jasondillingham/bosun/internal/launcher"
	bosunmcp "github.com/jasondillingham/bosun/internal/mcp"
	"github.com/jasondillingham/bosun/internal/session"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var (
		briefPath     string
		launch        bool
		isolateCache  bool
		force         bool
		fromBranch    string
		initialPrompt string
	)

	cmd := &cobra.Command{
		Use:   "init [N | <label> ...]",
		Short: "Create N numbered (or one-per-label named) worktrees + branches",
		Long: `Without args, creates default_session_count numbered sessions.

A single integer N creates session-1..session-N.

Two or more non-numeric args create named sessions (e.g. ` + "`bosun init auth http storage`" + `
produces branches bosun/auth, bosun/http, bosun/storage with worktrees and briefs to match).

Mixing integers with names in the same invocation is a usage error.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd, args, initOpts{
				brief:         briefPath,
				launch:        launch,
				isolateCache:  isolateCache,
				force:         force,
				fromBranch:    fromBranch,
				initialPrompt: initialPrompt,
			})
		},
	}

	cmd.Flags().StringVar(&briefPath, "brief", "", "path to a plan markdown with '## <label>' sections (e.g. '## session-1' or '## auth')")
	cmd.Flags().BoolVar(&launch, "launch", false, "spawn an agent session in each worktree")
	cmd.Flags().BoolVar(&isolateCache, "isolate-cache", false, "set per-worktree build-cache env vars when launching")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing bosun worktrees")
	cmd.Flags().StringVar(&fromBranch, "from", "", "base branch (defaults to config.base_branch)")
	cmd.Flags().StringVar(&initialPrompt, "initial-prompt", "", "first message passed to each launched session (paired with --launch; default: 'Read BOSUN_BRIEF.md...' when --brief is also set)")

	return cmd
}

type initOpts struct {
	brief         string
	launch        bool
	isolateCache  bool
	force         bool
	fromBranch    string
	initialPrompt string
}

func runInit(cmd *cobra.Command, args []string, opts initOpts) error {
	rc, err := loadCtx()
	if err != nil {
		return err
	}

	labels, err := resolveInitLabels(args, rc.cfg)
	if err != nil {
		return err
	}

	base := opts.fromBranch
	if base == "" {
		base = rc.cfg.BaseBranch
	}

	// Verify we're on the base branch (unless --force).
	currentBranch, err := rc.git.CurrentBranch(rc.ctx, rc.repoRoot)
	if err != nil {
		return gitErr("read current branch", err)
	}
	if currentBranch != base && !opts.force {
		return userErr("HEAD is on %q, not base branch %q. Re-run with --force to proceed anyway.", currentBranch, base)
	}

	// Parse brief once, up front, so a bad plan fails fast.
	var briefs []brief.Brief
	if opts.brief != "" {
		briefs, err = brief.Parse(opts.brief)
		if err != nil {
			return userErr("parse brief: %v", err)
		}
		if len(briefs) == 0 {
			return userErr("brief %s contains no `## <label>` sections", opts.brief)
		}
	}

	// Fire pre-init before any filesystem mutation so the operator hook
	// can fail-closed to abort a bad run.
	preEnv := map[string]string{
		"BOSUN_REPO_ROOT":     rc.repoRoot,
		"BOSUN_SESSION_COUNT": strconv.Itoa(len(labels)),
		"BOSUN_BASE_BRANCH":   base,
	}
	if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "pre-init", preEnv); err != nil {
		return userErr("%v", err)
	}

	// Pre-flight: check for existing worktree paths.
	for _, label := range labels {
		path := session.WorktreePathForLabel(rc.repoRoot, rc.cfg, label)
		if _, err := os.Stat(path); err == nil {
			if !opts.force {
				return userErr("worktree path already exists: %s (use --force to overwrite)", path)
			}
		}
	}

	// If --force: remove existing worktrees first.
	if opts.force {
		existing, err := rc.git.ListWorktrees(rc.ctx, rc.repoRoot)
		if err != nil {
			return gitErr("list worktrees", err)
		}
		for _, label := range labels {
			branch := rc.cfg.BranchForLabel(label)
			for _, wt := range existing {
				if wt.Branch == "refs/heads/"+branch {
					if err := rc.git.RemoveWorktree(rc.ctx, rc.repoRoot, wt.Path, true); err != nil {
						return gitErr(fmt.Sprintf("remove existing worktree %s", wt.Path), err)
					}
				}
			}
			if exists, _ := rc.git.BranchExists(rc.ctx, rc.repoRoot, branch); exists {
				if err := rc.git.DeleteBranch(rc.ctx, rc.repoRoot, branch, true); err != nil {
					return gitErr(fmt.Sprintf("delete existing branch %s", branch), err)
				}
			}
		}
	}

	// Create branches + worktrees.
	type created struct {
		label  string
		branch string
		path   string
	}
	var made []created

	for _, label := range labels {
		branch := rc.cfg.BranchForLabel(label)
		path := session.WorktreePathForLabel(rc.repoRoot, rc.cfg, label)

		exists, err := rc.git.BranchExists(rc.ctx, rc.repoRoot, branch)
		if err != nil {
			return gitErr("check branch", err)
		}
		if !exists {
			if err := rc.git.CreateBranch(rc.ctx, rc.repoRoot, branch, base); err != nil {
				return gitErr("create branch "+branch, err)
			}
		}
		if err := rc.git.AddWorktree(rc.ctx, rc.repoRoot, path, branch); err != nil {
			return gitErr("add worktree "+path, err)
		}
		made = append(made, created{label: label, branch: branch, path: path})

		// Always exclude BOSUN_BRIEF.md and .claude/CLAUDE.md from the
		// worktree's index, even when no brief is written this run — that
		// way a brief authored later stays out of commits without bosun
		// having to remember.
		if err := rc.git.AppendWorktreeExclude(rc.ctx, path, "BOSUN_BRIEF.md"); err != nil {
			fmt.Fprintf(os.Stderr, "bosun: warning: update %s exclude: %v\n", label, err)
		}
		if err := rc.git.AppendWorktreeExclude(rc.ctx, path, ".claude/CLAUDE.md"); err != nil {
			fmt.Fprintf(os.Stderr, "bosun: warning: update %s exclude: %v\n", label, err)
		}

		if b := brief.LookupBriefByLabel(briefs, label); b != nil {
			if err := brief.WriteToWorktree(path, *b, rc.cfg.VerifyCmd); err != nil {
				return internalErr("write brief for "+label, err)
			}
			if err := brief.WriteSessionPointer(path, label); err != nil {
				return internalErr("write session pointer for "+label, err)
			}
		}
	}

	if opts.brief != "" {
		if err := brief.ArchivePlan(rc.repoRoot, opts.brief); err != nil {
			// Non-fatal: archiving is a nice-to-have.
			fmt.Fprintf(os.Stderr, "bosun: warning: archive plan: %v\n", err)
		}
		// If the plan file lives inside the main repo (the common case — operator
		// wrote `plan.md` at the root), add it to .gitignore so `git status`
		// doesn't surface it as untracked. v0.1 dogfood finding: dogfood-plan.md
		// sat at the root and felt "wrong" to leave there.
		if err := ensurePlanIgnored(rc.repoRoot, opts.brief); err != nil {
			fmt.Fprintf(os.Stderr, "bosun: warning: ignore plan file: %v\n", err)
		}
	}

	// Ensure .bosun/ is in .gitignore so we don't accidentally commit it.
	if err := ensureBosunIgnored(rc.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: update .gitignore: %v\n", err)
	}

	// Fire post-init after every worktree exists and every brief is on
	// disk, but before launching agents — operators wiring this hook
	// typically want to seed/inspect the worktrees and the launch step is
	// optional.
	postEnv := map[string]string{
		"BOSUN_REPO_ROOT":     rc.repoRoot,
		"BOSUN_SESSION_COUNT": strconv.Itoa(len(made)),
		"BOSUN_BASE_BRANCH":   base,
	}
	if err := hooks.Run(rc.ctx, rc.cfg.Hooks, "post-init", postEnv); err != nil {
		return userErr("%v", err)
	}

	// Print summary.
	fmt.Fprintf(os.Stdout, "Created %d session(s):\n", len(made))
	for _, c := range made {
		fmt.Fprintf(os.Stdout, "  %-10s → %s  (branch: %s)\n", c.label, c.path, c.branch)
	}

	// Optional launch.
	if opts.launch {
		// Resolve the initial prompt: explicit flag wins; otherwise default to
		// pointing the agent at its brief when --brief was supplied. With no
		// brief and no prompt, the launch is silent (bare `claude`).
		prompt := opts.initialPrompt
		if prompt == "" && opts.brief != "" {
			prompt = "Read BOSUN_BRIEF.md in this directory — it's your assignment. Read it in full, then follow the workflow it describes."
		}

		// Bring up (or attach to) the MCP daemon and capture the socket
		// path so each launched session gets BOSUN_MCP_SOCK injected
		// into its environment. A failure here is non-fatal: sessions
		// still launch, they just fall back to filesystem coordination.
		mcpSocket := ""
		if info, err := ensureMcp(rc.repoRoot); err != nil {
			fmt.Fprintf(os.Stderr, "bosun: warning: MCP autostart failed: %v\n", err)
		} else {
			mcpSocket = info.socketPath
			switch {
			case info.spawned:
				fmt.Fprintf(os.Stdout, "Started MCP server (pid %d) on %s\n", info.pid, info.socketPath)
			case info.pid != 0:
				fmt.Fprintf(os.Stdout, "Reusing MCP server (pid %d) on %s\n", info.pid, info.socketPath)
			default:
				fmt.Fprintf(os.Stdout, "Using MCP server from %s=%s\n", bosunmcp.SocketEnv, info.socketPath)
			}
		}

		fmt.Fprintln(os.Stdout, "\nLaunching sessions:")
		for i, c := range made {
			env := map[string]string{}
			if opts.isolateCache {
				env = launcher.IsolateCacheEnv(c.path)
			}
			if mcpSocket != "" {
				env[bosunmcp.SocketEnv] = mcpSocket
			}
			strategy, err := launcher.Launch(launcher.Options{
				Strategy:      launcher.Strategy(rc.cfg.Launcher),
				WorktreePath:  c.path,
				SessionName:   c.label,
				Command:       "claude",
				InitialPrompt: prompt,
				// First session creates a window; subsequent ones land as
				// tabs in the same window. Cleaner than 4 scattered windows.
				OpenAsTab: i > 0,
				Env:       env,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  %s: launch failed: %v\n", c.label, err)
				continue
			}
			fmt.Fprintf(os.Stdout, "  %-10s via %s\n", c.label, strategy)
		}
	}

	return nil
}

// resolveInitLabels classifies the positional args into either numbered or
// named mode and returns the canonical label list bosun init should create.
//
// Rules:
//   - No args                 → numbered, count = cfg.DefaultSessionCount
//   - One positive integer    → numbered, count = arg
//   - 1+ non-numeric labels   → named, validated against the label charset
//   - Anything mixed          → usage error
func resolveInitLabels(args []string, cfg config.Config) ([]string, error) {
	if len(args) == 0 {
		return numberedLabels(cfg.DefaultSessionCount, cfg), nil
	}

	// Single integer arg → count form.
	if len(args) == 1 {
		if n, err := strconv.Atoi(args[0]); err == nil {
			if n < 1 {
				return nil, userErr("N must be a positive integer, got %q", args[0])
			}
			return numberedLabels(n, cfg), nil
		}
	}

	// At this point every arg must be a non-numeric label. Mixed is rejected.
	var labels []string
	seen := map[string]bool{}
	for _, a := range args {
		if _, err := strconv.Atoi(a); err == nil {
			return nil, userErr("init: cannot mix integer counts with named labels (got %q alongside named args)", a)
		}
		if err := session.ValidateLabel(a); err != nil {
			return nil, userErr("%v", err)
		}
		if seen[a] {
			return nil, userErr("duplicate session label %q", a)
		}
		seen[a] = true
		labels = append(labels, a)
	}
	return labels, nil
}

// numberedLabels returns ["session-1", ..., "session-N"].
func numberedLabels(n int, cfg config.Config) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, cfg.SessionName(i))
	}
	return out
}

// canonicalAbs returns an absolute path with symlinks resolved. On macOS,
// /var is a symlink to /private/var, so `t.TempDir()` paths and paths
// reported by git can have different prefixes for the same directory.
// Without resolving, filepath.Rel computes a path full of `..` traversals
// and downstream checks misclassify the file as "outside the repo." If
// the path doesn't exist yet, fall back to the absolute (un-resolved)
// form rather than failing.
func canonicalAbs(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// ensureBosunIgnored appends `.bosun/` to .gitignore if it's not already there.
func ensureBosunIgnored(repoRoot string) error {
	gi := filepath.Join(repoRoot, ".gitignore")
	data, err := os.ReadFile(gi)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(data)
	if containsLine(content, ".bosun/") || containsLine(content, "/.bosun/") {
		return nil
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content += "\n"
	}
	content += ".bosun/\n"
	return os.WriteFile(gi, []byte(content), 0o644)
}

// ensurePlanIgnored appends planPath (repo-relative) to .gitignore if the plan
// file lives inside repoRoot. Plans outside the repo (absolute or symlinked
// elsewhere) and plans already under .bosun/ (covered by ensureBosunIgnored)
// are no-ops.
func ensurePlanIgnored(repoRoot, planPath string) error {
	absPlan, err := canonicalAbs(planPath)
	if err != nil {
		return err
	}
	absRoot, err := canonicalAbs(repoRoot)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPlan)
	if err != nil {
		return err
	}
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		// Plan lives outside the repo; nothing to ignore here.
		return nil
	}
	relSlash := filepath.ToSlash(rel)
	if strings.HasPrefix(relSlash, ".bosun/") {
		// Already covered by the .bosun/ ignore.
		return nil
	}
	gi := filepath.Join(repoRoot, ".gitignore")
	data, err := os.ReadFile(gi)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(data)
	if containsLine(content, relSlash) || containsLine(content, "/"+relSlash) {
		return nil
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content += "\n"
	}
	content += relSlash + "\n"
	return os.WriteFile(gi, []byte(content), 0o644)
}

func containsLine(content, want string) bool {
	for _, line := range splitLines(content) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, trimCR(s[start:i]))
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, trimCR(s[start:]))
	}
	return out
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}
