package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jasondillingham/bosun/internal/brief"
	"github.com/jasondillingham/bosun/internal/config"
	"github.com/jasondillingham/bosun/internal/hooks"
	initstate "github.com/jasondillingham/bosun/internal/init"
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
		noLoadCheck   bool
		cleanPhantoms bool
		resume        bool
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
				noLoadCheck:   noLoadCheck,
				cleanPhantoms: cleanPhantoms,
				resume:        resume,
			})
		},
	}

	cmd.Flags().StringVar(&briefPath, "brief", "", "path to a plan markdown with '## <label>' sections (e.g. '## session-1' or '## auth')")
	cmd.Flags().BoolVar(&launch, "launch", false, "spawn an agent session in each worktree")
	cmd.Flags().BoolVar(&isolateCache, "isolate-cache", false, "set per-worktree build-cache env vars when launching")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing bosun worktrees")
	cmd.Flags().StringVar(&fromBranch, "from", "", "base branch (defaults to config.base_branch)")
	cmd.Flags().StringVar(&initialPrompt, "initial-prompt", "", "first message passed to each launched session (paired with --launch; default: 'Read BOSUN_BRIEF.md...' when --brief is also set)")
	cmd.Flags().BoolVar(&noLoadCheck, "no-load-check", false, "skip the pre-flight 1-minute load average check")
	cmd.Flags().BoolVar(&cleanPhantoms, "clean-phantoms", false, "auto-remove Finder/Spotlight phantom branch refs (off by default)")
	cmd.Flags().BoolVar(&resume, "resume", false, "continue a previously-interrupted bosun init using .bosun/init.state")

	return cmd
}

type initOpts struct {
	brief         string
	launch        bool
	isolateCache  bool
	force         bool
	fromBranch    string
	initialPrompt string
	noLoadCheck   bool
	cleanPhantoms bool
	resume        bool
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

	// Resume / refuse-on-stale gate. We do this before any pre-flight
	// (phantom scan, load average, hooks) so an operator who Ctrl-C'd a
	// prior init isn't surprised by the same flaky pre-flight running
	// twice. The two states are mutually exclusive: plain `init` with a
	// state file = refuse; `init --resume` without a state file = refuse.
	istate, err := initstate.Load(rc.repoRoot)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No prior state. --resume without a state file is a clear operator
		// mistake — surface it rather than falling through to a fresh init.
		if opts.resume {
			return userErr("--resume requested but no %s found; remove the flag for a fresh init", initstate.Path(rc.repoRoot))
		}
		istate = initstate.New(len(labels), opts.brief)
	case err != nil:
		return userErr("read %s: %v", initstate.Path(rc.repoRoot), err)
	case opts.force && !opts.resume:
		// --force is bosun's "I know, blow it away" escape hatch. Clear
		// the stale state breadcrumb and start a fresh run.
		fmt.Fprintf(os.Stdout, "bosun: --force: discarding stale %s.\n", initstate.Path(rc.repoRoot))
		if err := initstate.ClearFile(rc.repoRoot); err != nil {
			return internalErr("clear stale init state", err)
		}
		istate = initstate.New(len(labels), opts.brief)
	case !opts.resume:
		return userErr(
			"previous bosun init didn't finish (see %s).\n"+
				"  run `bosun init --resume` to continue, or `rm %s` to start fresh",
			initstate.Path(rc.repoRoot),
			initstate.Path(rc.repoRoot),
		)
	default:
		// --resume with valid state.
		fmt.Fprintf(os.Stdout, "Resuming previous init (started %s).\n", istate.StartedAt.Format(time.RFC3339))
		if istate.TotalSessions != len(labels) {
			return userErr(
				"--resume label count (%d) doesn't match prior init (%d). Re-run with the same args, or remove %s to start fresh.",
				len(labels), istate.TotalSessions, initstate.Path(rc.repoRoot),
			)
		}
	}

	// Pre-flight #1: phantom-branch detection. Cheap directory scan that
	// catches Finder / Time Machine / Spotlight artifacts (literal "<name>
	// <digit>" duplicates) before they confuse later git operations.
	if phantoms, err := findPhantomBranchRefs(rc.repoRoot, rc.cfg.SessionPrefix); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: phantom-ref scan failed: %v\n", err)
	} else if len(phantoms) > 0 {
		if opts.cleanPhantoms {
			for _, p := range phantoms {
				if err := os.Remove(p); err != nil {
					fmt.Fprintf(os.Stderr, "bosun: warning: remove phantom ref %s: %v\n", p, err)
				}
			}
			fmt.Fprintf(os.Stdout, "Removed %d phantom branch ref(s) under .git/refs/heads/%s/.\n", len(phantoms), rc.cfg.SessionPrefix)
		} else {
			example := filepath.Join(".git", "refs", "heads", rc.cfg.SessionPrefix, filepath.Base(phantoms[0]))
			fmt.Fprintf(os.Stdout, "found %d phantom branch ref(s) under .git/refs/heads/%s/; remove with `rm '%s'` (or re-run with --clean-phantoms)\n", len(phantoms), rc.cfg.SessionPrefix, example)
		}
	}

	// Pre-flight #2: 1-minute load average advisory. A high load right
	// before spinning up N worktrees + N agents tends to turn a slow init
	// into a silent-looking hang.
	if !opts.noLoadCheck {
		if load, err := getLoadAverage(); err != nil {
			fmt.Fprintf(os.Stderr, "bosun: warning: load-average check failed: %v\n", err)
		} else if load > loadAverageWarnThreshold {
			fmt.Fprintf(os.Stdout, "system load is %.2f; init may be slow (--no-load-check to skip)\n", load)
			time.Sleep(loadAveragePauseDuration)
		}
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

	// Pre-flight: check for existing worktree paths. Completed-in-prior-run
	// labels are exempt under --resume — we explicitly want to reuse those
	// worktrees, not refuse on them.
	for _, label := range labels {
		path := session.WorktreePathForLabel(rc.repoRoot, rc.cfg, label)
		if _, err := os.Stat(path); err == nil {
			if opts.resume && istate.IsCompleted(label) {
				continue
			}
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

	// On --resume, every label already in completed_sessions must still
	// have its worktree on disk. If somebody manually `rm -rf`'d a sibling
	// worktree directory, the resume can't safely "skip" — we'd leave the
	// final state inconsistent. Refuse with a clear pointer rather than
	// silently re-creating, which could clobber operator hand-fixes.
	if opts.resume && len(istate.CompletedSessions) > 0 {
		worktrees, err := rc.git.ListWorktrees(rc.ctx, rc.repoRoot)
		if err != nil {
			return gitErr("list worktrees", err)
		}
		known := map[string]bool{}
		for _, wt := range worktrees {
			known[wt.Path] = true
		}
		for _, label := range istate.CompletedSessions {
			path := session.WorktreePathForLabel(rc.repoRoot, rc.cfg, label)
			if _, statErr := os.Stat(path); statErr != nil {
				return userErr(
					"resume: completed session %s is missing its worktree at %s.\n"+
						"  remove %s and re-run `bosun init %d` for a clean start.",
					label, path, initstate.Path(rc.repoRoot), len(labels),
				)
			}
			if !known[path] {
				fmt.Fprintf(os.Stderr, "bosun: warning: %s exists on disk but is not registered as a worktree; resume continuing\n", path)
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

	for i, label := range labels {
		branch := rc.cfg.BranchForLabel(label)
		path := session.WorktreePathForLabel(rc.repoRoot, rc.cfg, label)

		// Resume short-circuit: already-completed sessions are skipped wholesale.
		if opts.resume && istate.IsCompleted(label) {
			fmt.Fprintf(os.Stdout, "Skipping %s (already completed in prior run).\n", label)
			made = append(made, created{label: label, branch: branch, path: path})
			continue
		}

		if err := istate.SetCurrent(rc.repoRoot, label, initstate.StepBranchCreate); err != nil {
			return internalErr("persist init state", err)
		}

		exists, err := rc.git.BranchExists(rc.ctx, rc.repoRoot, branch)
		if err != nil {
			return gitErr("check branch", err)
		}
		if !exists {
			if err := rc.git.CreateBranch(rc.ctx, rc.repoRoot, branch, base); err != nil {
				return gitErr("create branch "+branch, err)
			}
		}

		if err := istate.SetCurrent(rc.repoRoot, label, initstate.StepGitWorktreeAdd); err != nil {
			return internalErr("persist init state", err)
		}

		// On --resume, the worktree path may already exist if the prior
		// run completed AddWorktree but failed afterwards. Detect that
		// case via `git worktree list` and skip the re-add so we don't
		// hit "already exists" errors. A bare directory at the path that
		// git doesn't know about is still a hard error — that's the
		// operator-fingerprint case the safety contract wants to surface.
		if opts.resume {
			worktrees, listErr := rc.git.ListWorktrees(rc.ctx, rc.repoRoot)
			if listErr != nil {
				return gitErr("list worktrees", listErr)
			}
			registered := false
			for _, wt := range worktrees {
				if wt.Path == path || wt.Branch == "refs/heads/"+branch {
					registered = true
					break
				}
			}
			if registered {
				fmt.Fprintf(os.Stdout, "Reusing existing worktree for %s (%d/%d).\n", label, i+1, len(labels))
			} else {
				fmt.Fprintf(os.Stdout, "Creating worktree %s (%d/%d)...\n", label, i+1, len(labels))
				if err := rc.git.AddWorktree(rc.ctx, rc.repoRoot, path, branch); err != nil {
					return gitErr("add worktree "+path, err)
				}
			}
		} else {
			fmt.Fprintf(os.Stdout, "Creating worktree %s (%d/%d)...\n", label, i+1, len(labels))
			if err := rc.git.AddWorktree(rc.ctx, rc.repoRoot, path, branch); err != nil {
				return gitErr("add worktree "+path, err)
			}
		}

		made = append(made, created{label: label, branch: branch, path: path})

		if err := istate.SetCurrent(rc.repoRoot, label, initstate.StepStateFileWrite); err != nil {
			return internalErr("persist init state", err)
		}

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

		if err := istate.MarkComplete(rc.repoRoot, label); err != nil {
			return internalErr("persist init state", err)
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

	if err := istate.SetCurrent(rc.repoRoot, "", initstate.StepHookPostInit); err != nil {
		return internalErr("persist init state", err)
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

	// Everything succeeded — discard the resume breadcrumb so the next
	// plain `bosun init` isn't refused on stale state.
	if err := istate.Clear(rc.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "bosun: warning: clear init state: %v\n", err)
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

// loadAverageWarnThreshold is the 1-minute load average above which init
// prints a slow-warning. loadAveragePauseDuration is how long it pauses
// to give the operator a chance to abort. Both are var-scoped (not const)
// so tests can shorten the pause without sleeping for real seconds.
var (
	loadAverageWarnThreshold = 5.0
	loadAveragePauseDuration = 2 * time.Second
)

// getLoadAverage is the indirection point tests override to inject a
// synthetic 1-minute load average. The default reads the host's actual
// load (or honors BOSUN_TEST_LOAD_AVERAGE when set, which lets
// subprocess-style scenario tests exercise the warning path).
var getLoadAverage = readSystemLoadAverage

func readSystemLoadAverage() (float64, error) {
	if v := os.Getenv("BOSUN_TEST_LOAD_AVERAGE"); v != "" {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, fmt.Errorf("BOSUN_TEST_LOAD_AVERAGE=%q: %w", v, err)
		}
		return f, nil
	}
	switch runtime.GOOS {
	case "linux":
		return readLoadFromProcLoadavg()
	case "darwin":
		return readLoadFromUptime()
	default:
		// Windows / other: no reliable cross-vendor 1-min load average.
		return 0, nil
	}
}

func readLoadFromProcLoadavg() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected /proc/loadavg format: %q", data)
	}
	return strconv.ParseFloat(fields[0], 64)
}

func readLoadFromUptime() (float64, error) {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		return 0, err
	}
	return parseUptimeLoad(string(out))
}

// parseUptimeLoad pulls the 1-minute load average out of `uptime` output.
// Example input: "14:32  up 1:23, 2 users, load averages: 1.23 1.45 1.67".
// macOS uses "load averages:" (plural) and Linux uses "load average:";
// match either by anchoring on "load average".
func parseUptimeLoad(text string) (float64, error) {
	idx := strings.Index(text, "load average")
	if idx < 0 {
		return 0, fmt.Errorf("unexpected uptime output: %q", text)
	}
	colon := strings.Index(text[idx:], ":")
	if colon < 0 {
		return 0, fmt.Errorf("unexpected uptime output: %q", text)
	}
	rest := text[idx+colon+1:]
	// Linux uptime separates with commas; macOS with spaces. Normalize.
	rest = strings.ReplaceAll(rest, ",", " ")
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, fmt.Errorf("unexpected uptime output: %q", text)
	}
	return strconv.ParseFloat(fields[0], 64)
}

// phantomBranchRefPattern matches macOS Finder / Time Machine / Spotlight
// ref duplicates: a literal space followed by a single digit (1-9) at end
// of the filename. Pattern documented at the call site in runInit.
var phantomBranchRefPattern = regexp.MustCompile(` [0-9]$`)

// findPhantomBranchRefs scans .git/refs/heads/<sessionPrefix>/ for ref
// files whose basename matches the phantom pattern. Returns absolute
// paths. A missing directory (no bosun branches ever created here) is
// not an error.
func findPhantomBranchRefs(repoRoot, sessionPrefix string) ([]string, error) {
	dir := filepath.Join(repoRoot, ".git", "refs", "heads", sessionPrefix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var phantoms []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if phantomBranchRefPattern.MatchString(e.Name()) {
			phantoms = append(phantoms, filepath.Join(dir, e.Name()))
		}
	}
	return phantoms, nil
}
