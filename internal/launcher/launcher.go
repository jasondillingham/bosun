// Package launcher spawns an interactive agent session inside a worktree.
// It picks a strategy based on the environment: tmux when available and
// we're already inside tmux; an OS-native terminal otherwise; and finally
// falls back to printing copy-pasteable commands.
package launcher

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Strategy is the resolved launch method.
type Strategy string

const (
	StrategyTmux     Strategy = "tmux"
	StrategyTerminal Strategy = "terminal"
	StrategyPrint    Strategy = "print"
	StrategyAuto     Strategy = "auto"
)

// Options controls a single launch.
type Options struct {
	Strategy      Strategy          // "auto" picks tmux → terminal → print
	WorktreePath  string            // absolute path to the worktree
	SessionName   string            // e.g. "session-1" (for window titles)
	Command       string            // command to run (default: "claude")
	InitialPrompt string            // optional initial message to pass to the launched agent
	Env           map[string]string // extra env vars to set (e.g. GOCACHE)
	Out           io.Writer         // where to print fallback commands; defaults to os.Stdout
}

// Launch runs Options against the chosen strategy. Returns the resolved
// strategy (auto resolves to one of the concrete strategies).
func Launch(opts Options) (Strategy, error) {
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Command == "" {
		opts.Command = "claude"
	}
	if opts.WorktreePath == "" {
		return "", fmt.Errorf("launcher: WorktreePath is required")
	}

	chosen := opts.Strategy
	if chosen == "" || chosen == StrategyAuto {
		chosen = pickAuto()
	}

	switch chosen {
	case StrategyTmux:
		if err := launchTmux(opts); err != nil {
			return chosen, err
		}
	case StrategyTerminal:
		if err := launchTerminal(opts); err != nil {
			// On failure, fall back to print rather than erroring out the whole init.
			fmt.Fprintf(opts.Out, "bosun: terminal launch failed for %s: %v — falling back to print\n", opts.SessionName, err)
			printFallback(opts)
			return StrategyPrint, nil
		}
	case StrategyPrint:
		printFallback(opts)
	default:
		return chosen, fmt.Errorf("unknown launcher strategy %q", chosen)
	}
	return chosen, nil
}

func pickAuto() Strategy {
	if hasTmux() {
		return StrategyTmux
	}
	if hasTerminal() {
		return StrategyTerminal
	}
	return StrategyPrint
}

func hasTmux() bool {
	if os.Getenv("TMUX") == "" {
		return false
	}
	_, err := exec.LookPath("tmux")
	return err == nil
}

func hasTerminal() bool {
	// Ghostty is cross-OS; if present anywhere, count this OS as having a terminal.
	if _, ok := hasGhostty(); ok {
		return true
	}
	switch runtime.GOOS {
	case "darwin":
		_, err := exec.LookPath("osascript")
		return err == nil
	case "linux":
		for _, c := range linuxTerminals {
			if _, err := exec.LookPath(c); err == nil {
				return true
			}
		}
		return false
	case "windows":
		return true // cmd /c start is always available
	}
	return false
}

var linuxTerminals = []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xterm"}

// hasGhostty reports whether the Ghostty terminal can be invoked. Checks
// PATH first, then falls back to the standard macOS app-bundle location
// so users who installed via the .app download (and didn't symlink the
// CLI) still get a working --launch.
func hasGhostty() (string, bool) {
	if p, err := exec.LookPath("ghostty"); err == nil {
		return p, true
	}
	if runtime.GOOS == "darwin" {
		candidate := "/Applications/Ghostty.app/Contents/MacOS/ghostty"
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

func launchTmux(opts Options) error {
	envPairs := buildEnvPairs(opts.Env)
	args := []string{"new-window", "-c", opts.WorktreePath, "-n", opts.SessionName}
	for _, e := range envPairs {
		args = append(args, "-e", e)
	}
	// tmux runs the command directly (not through a shell), so pass command
	// + optional prompt as separate argv elements rather than as a shell-
	// quoted string.
	args = append(args, opts.Command)
	if opts.InitialPrompt != "" {
		args = append(args, opts.InitialPrompt)
	}
	cmd := exec.Command("tmux", args...)
	return cmd.Run()
}

// shellInvocation returns "<command>" or "<command> '<quoted-prompt>'" for
// use inside a POSIX shell pipeline. Shared by darwin/linux/ghostty paths.
func shellInvocation(opts Options) string {
	if opts.InitialPrompt == "" {
		return opts.Command
	}
	return opts.Command + " " + shellQuote(opts.InitialPrompt)
}

func launchTerminal(opts Options) error {
	// Prefer Ghostty when available. It's cross-OS, modern, and likely
	// what the operator is already using if it's on PATH — keeping the
	// new windows in the same terminal app is the consistent UX.
	if bin, ok := hasGhostty(); ok {
		return launchTerminalGhostty(opts, bin)
	}
	switch runtime.GOOS {
	case "darwin":
		return launchTerminalDarwin(opts)
	case "linux":
		return launchTerminalLinux(opts)
	case "windows":
		return launchTerminalWindows(opts)
	}
	return fmt.Errorf("no terminal strategy for %s", runtime.GOOS)
}

func launchTerminalGhostty(opts Options, bin string) error {
	envPrefix := buildShellEnvPrefix(opts.Env)
	inner := fmt.Sprintf("cd %s && %s%s; exec bash",
		shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts))
	cmd := exec.Command(bin, "-e", "bash", "-lc", inner)
	return cmd.Run()
}

func launchTerminalDarwin(opts Options) error {
	envPrefix := buildShellEnvPrefix(opts.Env)
	// osascript opens a new Terminal window cd'd to the worktree and runs the command.
	script := fmt.Sprintf(
		`tell application "Terminal" to do script "cd %s && %s%s"`,
		shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts),
	)
	cmd := exec.Command("osascript", "-e", script)
	return cmd.Run()
}

func launchTerminalLinux(opts Options) error {
	envPrefix := buildShellEnvPrefix(opts.Env)
	inner := fmt.Sprintf("cd %s && %s%s; exec bash",
		shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts))
	for _, term := range linuxTerminals {
		if _, err := exec.LookPath(term); err != nil {
			continue
		}
		var cmd *exec.Cmd
		switch term {
		case "gnome-terminal":
			cmd = exec.Command(term, "--", "bash", "-lc", inner)
		default:
			cmd = exec.Command(term, "-e", "bash", "-lc", inner)
		}
		return cmd.Run()
	}
	return fmt.Errorf("no terminal emulator found")
}

func launchTerminalWindows(opts Options) error {
	envPrefix := buildCmdEnvPrefix(opts.Env)
	cmdLine := opts.Command
	if opts.InitialPrompt != "" {
		// cmd.exe quoting: wrap in double-quotes, escape embedded `"` as `""`.
		quoted := strings.ReplaceAll(opts.InitialPrompt, `"`, `""`)
		cmdLine = fmt.Sprintf(`%s "%s"`, opts.Command, quoted)
	}
	// Use cmd /c start cmd /K to leave a window open after the command runs.
	args := []string{"/c", "start", "cmd", "/K",
		fmt.Sprintf("cd /D %s && %s%s", opts.WorktreePath, envPrefix, cmdLine)}
	cmd := exec.Command("cmd", args...)
	return cmd.Run()
}

func printFallback(opts Options) {
	envPrefix := buildShellEnvPrefix(opts.Env)
	fmt.Fprintf(opts.Out, "bosun: run %s manually:\n", opts.SessionName)
	fmt.Fprintf(opts.Out, "  cd %s && %s%s\n", shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts))
}

func buildEnvPairs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := sortedKeys(env)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return out
}

func buildShellEnvPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	parts := []string{}
	for _, k := range sortedKeys(env) {
		parts = append(parts, fmt.Sprintf("%s=%s", k, shellQuote(env[k])))
	}
	return strings.Join(parts, " ") + " "
}

func buildCmdEnvPrefix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	parts := []string{}
	for _, k := range sortedKeys(env) {
		parts = append(parts, fmt.Sprintf("set %s=%s &&", k, env[k]))
	}
	return strings.Join(parts, " ") + " "
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Inline sort to avoid pulling in another import.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
// Suitable for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// IsolateCacheEnv returns the env-var map for a per-worktree isolated build cache.
func IsolateCacheEnv(worktreePath string) map[string]string {
	cache := filepath.Join(worktreePath, ".cache")
	return map[string]string{
		"GOCACHE":             filepath.Join(cache, "go-build"),
		"GOMODCACHE":          filepath.Join(cache, "go-mod"),
		"npm_config_cache":    filepath.Join(cache, "npm"),
		"PYTHONPYCACHEPREFIX": filepath.Join(cache, "pycache"),
		"CARGO_TARGET_DIR":    filepath.Join(worktreePath, "target"),
	}
}
