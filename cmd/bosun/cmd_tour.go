package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// tourOpts collects the flag set surface area. Kept tiny on purpose — the
// tour is a guided walkthrough, not a configurable workflow tool.
type tourOpts struct {
	keepSandbox bool
}

func newTourCmd() *cobra.Command {
	var opts tourOpts
	cmd := &cobra.Command{
		Use:   "tour",
		Short: "Interactive 5-minute walkthrough on a throwaway sandbox repo",
		Long: `An interactive 5-minute walkthrough of a complete bosun round
(init → edit → status → predict → merge → cleanup) on a sandbox repo
bosun creates under /tmp. Each step pauses for Enter so you can read
the output; Ctrl-C aborts and cleans up. The sandbox is removed on
exit unless --keep-sandbox is passed.

Tests and recordings can set BOSUN_TOUR_AUTO=1 to skip the keypress
waits and run the whole flow non-interactively.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTour(cmd.OutOrStdout(), cmd.InOrStdin(), opts)
		},
	}
	cmd.Flags().BoolVar(&opts.keepSandbox, "keep-sandbox", false, "preserve the sandbox dir on exit (default: delete)")
	cmd.GroupID = "setup"
	return cmd
}

// runTour is the testable command body. It builds a throwaway repo under
// /tmp, drives the five tour steps by shelling out to this binary's own
// bosun sub-commands, and cleans the sandbox up on both normal exit and
// Ctrl-C. Returns userErr/internalErr — main maps those to exit codes.
func runTour(w io.Writer, r io.Reader, opts tourOpts) error {
	auto := os.Getenv("BOSUN_TOUR_AUTO") == "1"

	// os.Executable() is how the tour finds the binary the operator
	// invoked, so the recursive sub-command calls run the same code.
	bin, err := os.Executable()
	if err != nil {
		return internalErr("locate bosun binary", err)
	}

	// Sandbox lives under /tmp per issue #15 — iCloud-managed paths
	// silently strip worktree admin metadata under load and that's the
	// last shape we want a new user to see during their first round.
	rawSandbox, err := os.MkdirTemp("/tmp", "bosun-tour-")
	if err != nil {
		return internalErr("create sandbox", err)
	}
	// Resolve symlinks (/tmp -> /private/tmp on macOS) so the sibling
	// worktree paths bosun init computes match the paths the tour
	// hand-writes to in later steps. Without this, the basename-derived
	// worktree dirs would point at the unresolved /tmp/... shape and
	// confuse macOS-side path comparisons.
	sandbox, err := filepath.EvalSymlinks(rawSandbox)
	if err != nil {
		sandbox = rawSandbox
	}

	cleanedUp := false
	doCleanup := func() {
		if cleanedUp {
			return
		}
		cleanedUp = true
		if opts.keepSandbox {
			fmt.Fprintf(w, "\nSandbox preserved at %s (--keep-sandbox).\n", sandbox)
			return
		}
		// Sibling worktree dirs are created by `bosun init` under
		// <parent>/<basename>-bosun-N. They live outside the sandbox
		// dir itself, so we sweep them explicitly — `bosun cleanup`
		// at the end of the tour usually handles this, but an early
		// abort (Ctrl-C, error mid-step) needs the belt-and-braces.
		parent := filepath.Dir(sandbox)
		basename := filepath.Base(sandbox)
		if entries, err := os.ReadDir(parent); err == nil {
			prefix := basename + "-bosun-"
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), prefix) {
					_ = os.RemoveAll(filepath.Join(parent, e.Name()))
				}
			}
		}
		_ = os.RemoveAll(sandbox)
		fmt.Fprintf(w, "\nSandbox at %s removed.\n", sandbox)
	}
	defer doCleanup()

	// Trap Ctrl-C so the cleanup deferral above still runs before the
	// process exits. SIGTERM too, in case the tour is invoked under
	// `timeout` or supervised by a test harness.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		fmt.Fprintln(w, "\nbosun: tour aborted")
		doCleanup()
		os.Exit(exitUserErr)
	}()

	scan := bufio.NewScanner(r)
	// autoStepPause controls how long auto-mode lingers between steps. Zero
	// (the default for tests) blasts through with no delay; recordings can
	// set BOSUN_TOUR_AUTO_PAUSE to e.g. "2s" so an asciinema viewer has time
	// to read each step before the next one paints.
	autoStepPause := time.Duration(0)
	if auto {
		if raw := os.Getenv("BOSUN_TOUR_AUTO_PAUSE"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				autoStepPause = d
			}
		}
	}
	waitForKey := func() error {
		if auto {
			fmt.Fprintln(w, "[BOSUN_TOUR_AUTO=1: continuing]")
			if autoStepPause > 0 {
				time.Sleep(autoStepPause)
			}
			return nil
		}
		fmt.Fprint(w, "Press Enter to continue (Ctrl-C aborts)... ")
		if !scan.Scan() {
			// EOF on stdin (piped run that closed) — treat as continue
			// rather than fail; matches the BOSUN_TOUR_AUTO contract.
			if err := scan.Err(); err != nil {
				return userErr("read stdin: %v", err)
			}
		}
		return nil
	}

	fmt.Fprintln(w, "bosun tour — a 5-minute guided round on a throwaway repo")
	fmt.Fprintf(w, "Sandbox: %s\n\n", sandbox)

	// ---- Step 1 ----
	fmt.Fprintln(w, "── Step 1/5 ──────────────────────────────────────────────")
	fmt.Fprintln(w, "Setting up a sandbox repo with three placeholder Go files...")
	if err := setupTourSandboxRepo(sandbox); err != nil {
		return internalErr("set up sandbox repo", err)
	}
	fmt.Fprintf(w, "Set up a sandbox repo at %s. Continue?\n", sandbox)
	if err := waitForKey(); err != nil {
		return err
	}

	// ---- Step 2 ----
	fmt.Fprintln(w, "\n── Step 2/5 ──────────────────────────────────────────────")
	fmt.Fprintln(w, "Running `bosun init 2`:")
	if err := runTourBosun(w, sandbox, bin, "init", "2"); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nTwo worktrees created. Each has its own branch off main.")
	fmt.Fprintln(w, "The original repo (the one your editor is open on) is untouched.")
	if err := waitForKey(); err != nil {
		return err
	}

	// ---- Step 3 ----
	fmt.Fprintln(w, "\n── Step 3/5 ──────────────────────────────────────────────")
	fmt.Fprintln(w, "Simulating each session's work + committing on its branch...")
	if err := simulateTourEdits(sandbox); err != nil {
		return internalErr("simulate session edits", err)
	}
	if err := commitTourEdits(sandbox); err != nil {
		return internalErr("commit session work", err)
	}
	fmt.Fprintln(w, "Running `bosun status`:")
	if err := runTourBosun(w, sandbox, bin, "status"); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nEach session committed its change to its own branch.")
	fmt.Fprintln(w, "Main is untouched. In a real run, agents would be doing")
	fmt.Fprintln(w, "this work in parallel; bosun would track per-session state")
	fmt.Fprintln(w, "(WORKING / DIRTY / DONE) live as agents wrote files.")
	if err := waitForKey(); err != nil {
		return err
	}

	// ---- Step 4 ----
	fmt.Fprintln(w, "\n── Step 4/5 ──────────────────────────────────────────────")
	fmt.Fprintln(w, "Running `bosun predict` against a non-overlapping plan:")
	planPath := filepath.Join(sandbox, "tour-plan.md")
	if err := os.WriteFile(planPath, []byte(tourPlanMarkdown), 0o644); err != nil {
		return internalErr("write tour plan", err)
	}
	if err := runTourBosun(w, sandbox, bin, "predict", planPath); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nNo overlap. Safe to proceed.")
	if err := waitForKey(); err != nil {
		return err
	}

	// ---- Step 5 ----
	fmt.Fprintln(w, "\n── Step 5/5 ──────────────────────────────────────────────")
	fmt.Fprintln(w, "Marking each session done, merging, cleaning up...")
	if err := runTourBosun(w, sandbox, bin, "done", "session-1", "-m", "tour session-1 ready"); err != nil {
		return err
	}
	if err := runTourBosun(w, sandbox, bin, "done", "session-2", "-m", "tour session-2 ready"); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nRunning `bosun merge session-1`:")
	if err := runTourBosun(w, sandbox, bin, "merge", "session-1"); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nRunning `bosun merge session-2`:")
	if err := runTourBosun(w, sandbox, bin, "merge", "session-2"); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nMain branch's recent commits:")
	if err := runTourGit(w, sandbox, "log", "--oneline", "-n", "5"); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nRunning `bosun cleanup`:")
	if err := runTourBosun(w, sandbox, bin, "cleanup"); err != nil {
		return err
	}
	fmt.Fprintln(w, "\nBoth lanes landed on main. Worktrees torn down. You're back")
	fmt.Fprintln(w, "where you started — except main now has the work both lanes")
	fmt.Fprintln(w, "produced. That's bosun.")

	// ---- Next steps ----
	fmt.Fprintln(w, "\nNext steps:")
	fmt.Fprintln(w, "  • README quickstart: README.md#quick-start")
	fmt.Fprintln(w, "  • Brief recipe template: docs/brief-recipe-template.md")
	fmt.Fprintln(w, "  • Auto-suggest a plan from a goal: bosun suggest \"<your goal>\"")

	return nil
}

// tourPlanMarkdown is the brief.Parse-compatible plan used in step 4.
// File mentions are kept disjoint per session so `bosun predict` reports
// "no overlap" — the tour's promise to the operator.
const tourPlanMarkdown = `# Tour plan

## session-1

**Goal.** Tweak feature1.go in the first worktree. Scope is limited to
feature1.go and nothing else.

Files touched: feature1.go.

## session-2

**Goal.** Tweak feature2.go in the second worktree. Scope is limited to
feature2.go and nothing else.

Files touched: feature2.go.
`

// setupTourSandboxRepo turns root into a fresh git repo on main with
// three placeholder Go files committed. The configured user identity is
// scoped to this repo (--local equivalent via `git config` in the dir)
// so the tour doesn't inherit whatever global identity the operator has
// configured for their own work.
func setupTourSandboxRepo(root string) error {
	steps := [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "tour@bosun.local"},
		{"config", "user.name", "Bosun Tour"},
		{"config", "commit.gpgsign", "false"},
	}
	for _, args := range steps {
		if err := runGitSilent(root, args...); err != nil {
			return err
		}
	}
	for _, name := range []string{"feature1.go", "feature2.go", "shared.go"} {
		body := fmt.Sprintf("package main\n\n// %s placeholder for the bosun tour.\n", name)
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := runGitSilent(root, "add", "."); err != nil {
		return err
	}
	if err := runGitSilent(root, "commit", "-q", "-m", "initial commit"); err != nil {
		return err
	}
	return nil
}

// simulateTourEdits writes a small change into each session's worktree
// so step 3's `bosun status` shows non-zero dirty counts and step 5's
// commits land non-empty squash-merges on main.
func simulateTourEdits(sandboxRoot string) error {
	for i, file := range []string{"feature1.go", "feature2.go"} {
		wt := tourWorktreePath(sandboxRoot, i+1)
		path := filepath.Join(wt, file)
		body := fmt.Sprintf("package main\n\n// %s — edited by session-%d during the tour.\nfunc tweak%d() {}\n", file, i+1, i+1)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

// commitTourEdits stages and commits the simulated edits in each
// session worktree. Done as a single git call per worktree to keep the
// scrollback tight — the tour is busy enough already.
func commitTourEdits(sandboxRoot string) error {
	for i := 1; i <= 2; i++ {
		wt := tourWorktreePath(sandboxRoot, i)
		if err := runGitSilent(wt, "add", "."); err != nil {
			return err
		}
		msg := fmt.Sprintf("session-%d: tour edits", i)
		if err := runGitSilent(wt, "commit", "-q", "-m", msg); err != nil {
			return err
		}
	}
	return nil
}

// tourWorktreePath finds session N's worktree dir alongside the sandbox.
// Since v0.10's UID-per-worktree change (docs/uid-worktree-design.md),
// `bosun init` stamps each round's dirs with a UTC timestamp, so the
// on-disk name is `<basename>-bosun-<YYYYMMDD-HHMMSS>-N`. We scan the
// parent for that shape and fall back to the legacy `<basename>-bosun-N`
// form when nothing matches — keeps the tour working for both the new
// naming and any operator who ran a pre-v0.10 bosun against the sandbox.
func tourWorktreePath(sandboxRoot string, n int) string {
	parent := filepath.Dir(sandboxRoot)
	name := filepath.Base(sandboxRoot)
	prefix := name + "-bosun-"
	sub := strconv.Itoa(n)
	legacy := filepath.Join(parent, prefix+sub)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return legacy
	}
	wantSuffix := "-" + sub
	for _, e := range entries {
		base := e.Name()
		if !strings.HasPrefix(base, prefix) {
			continue
		}
		tail := strings.TrimPrefix(base, prefix)
		if tail == sub {
			return filepath.Join(parent, base)
		}
		if strings.HasSuffix(tail, wantSuffix) {
			head := strings.TrimSuffix(tail, wantSuffix)
			if looksLikeTourRoundTimestamp(head) {
				return filepath.Join(parent, base)
			}
		}
	}
	return legacy
}

// looksLikeTourRoundTimestamp matches the UTC `YYYYMMDD-HHMMSS` token
// cmd_init bakes into worktree dir names. Kept colocated with
// tourWorktreePath so the tour's path lookup doesn't reach into another
// package's internals.
func looksLikeTourRoundTimestamp(s string) bool {
	if len(s) != 15 || s[8] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 {
			continue
		}
		ch := s[i]
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// runTourBosun shells out to the bosun binary `bin` inside `dir` and
// streams its combined output to `w` so the operator sees the same
// scrollback they would running the command directly.
func runTourBosun(w io.Writer, dir, bin string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return userErr("`bosun %s` failed: %v", strings.Join(args, " "), err)
	}
	return nil
}

// runTourGit is the equivalent of runTourBosun for the git binary.
func runTourGit(w io.Writer, dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return userErr("`git %s` failed: %v", strings.Join(args, " "), err)
	}
	return nil
}

// runGitSilent runs git in dir and suppresses output unless the command
// fails — the sandbox setup chatter ("Initialized empty Git repository in
// /tmp/...") would clutter the tour's opening step.
func runGitSilent(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return nil
}
