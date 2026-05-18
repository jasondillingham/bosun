package main

import (
	"os"

	"github.com/jasondillingham/bosun/internal/claudehook"
	"github.com/spf13/cobra"
)

// newHookCmd wires the three v0.9.1 `bosun hook ...` subcommands.
// All three operate on the main worktree's `.claude/settings.json`
// — install/uninstall mutate it; claim reads PreToolUse JSON from
// stdin and records a claim. See docs/v0.9.1-hook-spec.md for the
// full design.
//
// `hook claim` is the only subcommand operators don't run by hand —
// Claude Code invokes it on every Edit/Write/MultiEdit/NotebookEdit
// once the install entry is in place. Its contract is "exit 0, log
// to stderr on errors" so a misconfigured bosun can't block a tool
// call that would otherwise succeed.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Manage the Claude Code PreToolUse hook that auto-records bosun_claim",
	}
	cmd.AddCommand(newHookInstallCmd(), newHookUninstallCmd(), newHookClaimCmd())
	cmd.GroupID = "wiring"
	return cmd
}

func newHookInstallCmd() *cobra.Command {
	// gitignore + noGitignore are a paired bool: --gitignore is the
	// (redundant) explicit opt-in; --no-gitignore is the opt-out.
	// Default behavior is to manage .gitignore so a fresh repo
	// doesn't accumulate per-developer Claude Code state in
	// untracked .claude/ paths that `git add .` would sweep in.
	gitignore := true
	var noGitignore bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the PreToolUse hook into <repo>/.claude/settings.json",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := loadCtx()
			if err != nil {
				return err
			}
			if noGitignore {
				gitignore = false
			}
			res, err := claudehook.Install(rc.repoRoot, claudehook.InstallOptions{
				ManageGitignore: gitignore,
			})
			if err != nil {
				return internalErr("install hook", err)
			}
			if res.Changed {
				printf("bosun: hook installed at %s\n", res.SettingsPath)
				printf("       Edit/Write/MultiEdit/NotebookEdit will now auto-claim paths via bosun_claim.\n")
				printf("       To uninstall: bosun hook uninstall\n")
			} else {
				printf("bosun: hook is already installed at %s (no changes).\n", res.SettingsPath)
			}
			if res.GitignoreChanged {
				printf("bosun: updated %s — .claude/settings.json stays tracked; everything else under .claude/ is ignored.\n", res.GitignorePath)
			}
			if res.Gitignored {
				// The hook config still applies locally — git just won't
				// carry it to anyone who clones the repo. Surfacing this
				// once at install is the only chance to flag it.
				printf("bosun: note — .claude/settings.json appears gitignored.\n")
				printf("       The hook applies on this machine but won't travel with the repo.\n")
				printf("       To make it portable, remove .claude/settings.json from .gitignore.\n")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&gitignore, "gitignore", true, "manage <repo>/.gitignore so .claude/settings.json stays tracked while other .claude/ state is ignored")
	cmd.Flags().BoolVar(&noGitignore, "no-gitignore", false, "leave <repo>/.gitignore untouched (opt out of the gitignore management step)")
	return cmd
}

func newHookUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the bosun PreToolUse hook from <repo>/.claude/settings.json",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := loadCtx()
			if err != nil {
				return err
			}
			res, err := claudehook.Uninstall(rc.repoRoot)
			if err != nil {
				return internalErr("uninstall hook", err)
			}
			switch {
			case res.Removed:
				printf("bosun: hook removed from %s\n", res.SettingsPath)
			case res.Existed:
				printf("bosun: no bosun hook entry in %s (nothing to do).\n", res.SettingsPath)
			default:
				printf("bosun: %s does not exist (nothing to do).\n", res.SettingsPath)
			}
			return nil
		},
	}
}

func newHookClaimCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "claim",
		Short:  "PreToolUse handler — reads JSON on stdin, records a claim. Invoked by Claude Code, not by hand.",
		Hidden: true,
		Args:   cobra.NoArgs,
		// SilenceErrors and SilenceUsage prevent Cobra from emitting
		// anything to stderr ahead of the hook's own diagnostics.
		// Claude Code surfaces hook stderr to the agent; a stray
		// usage banner there would just be noise.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// HandlePreToolUse always returns nil — every failure
			// mode logs to stderr and exits 0 so the agent's tool
			// call is never blocked by a bosun-side problem.
			return claudehook.HandlePreToolUse(os.Stdin, os.Stderr, claudehook.HandleOptions{})
		},
	}
}
