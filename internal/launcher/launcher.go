// Package launcher spawns an interactive agent session inside a worktree.
// It picks a strategy based on the environment: tmux when available and
// we're already inside tmux; an OS-native terminal otherwise; and finally
// falls back to printing copy-pasteable commands.
package launcher

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
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
	OpenAsTab     bool              // open as a new tab in the existing terminal instead of a new window (Ghostty only for now)
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

// hasITerm2 reports whether iTerm2 is installed on this macOS box. iTerm2
// has clean AppleScript tab support, so when it's present we prefer it
// over Terminal.app — Terminal.app's tab-in-current-window dance requires
// System Events keystrokes and is fragile in practice.
func hasITerm2() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	if _, err := os.Stat("/Applications/iTerm.app"); err == nil {
		return true
	}
	return false
}

// hasWindowsTerminal reports whether `wt.exe` (Microsoft's Windows Terminal)
// is on PATH. Default on Windows 11; opt-in on Windows 10. Supports a
// real "open as tab" command, unlike `cmd /c start`.
func hasWindowsTerminal() (string, bool) {
	if runtime.GOOS != "windows" {
		return "", false
	}
	if p, err := exec.LookPath("wt"); err == nil {
		return p, true
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
	return execRunFn(exec.Command("tmux", args...))
}

// execRunFn invokes cmd.Run() in production. Tests substitute a fake
// to assert on argv and simulate run failures without invoking the
// real tmux binary.
var execRunFn = func(cmd *exec.Cmd) error { return cmd.Run() }

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
	// Ghostty on macOS reaches into the running .app via IPC. Back-to-back
	// invocations from the same goroutine raced under trial #3c (only 1
	// of 3 windows materialized); waitForMacOSStagger spaces them out.
	if runtime.GOOS == "darwin" {
		waitForMacOSStagger()
	}
	return spawnFn(exec.Command(bin, ghosttyArgs(opts)...), opts.SessionName, opts.Out)
}

// ghosttyArgs builds the argv for the Ghostty CLI.
//
// Note on OpenAsTab: Ghostty's CLI does NOT currently support opening a
// new tab in an existing window — `+new-tab` exists as a keybinding
// action but isn't wired into the CLI. This is a long-requested upstream
// feature (ghostty-org/ghostty discussions #3445, #4579 among others).
// Until the upstream ships, OpenAsTab is silently ignored and every
// session opens in its own window. SPEC.md and docs/v0.2-roadmap.md flag
// this as deferred-on-Ghostty.
func ghosttyArgs(opts Options) []string {
	envPrefix := buildShellEnvPrefix(opts.Env)
	inner := fmt.Sprintf("cd %s && %s%s; exec bash",
		shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts))
	return []string{"-e", "bash", "-lc", inner}
}

// spawnFn is the production spawner used by the terminal-launch paths.
// Tests assign a fake to record argv and simulate post-fork failures
// without spawning real terminal windows. The (cmd, sessionName,
// outWriter) shape lets the production impl surface post-fork stderr
// to the operator without coupling spawnDetached to opts.Out directly.
var spawnFn = spawnDetached

// spawnDetached starts cmd without blocking on its exit. Used for terminal
// launches where the child window must outlive the bosun process. We reap
// the child in a goroutine so it doesn't sit as a zombie if bosun is still
// alive when it exits; once bosun exits, the OS reparents the child to
// init/launchd which handles reaping.
//
// Returns the error from Start() so genuinely-failed launches (binary not
// on PATH, permission denied, etc.) still surface to the caller. Post-fork
// errors (osascript reporting an AppleScript error, ghostty CLI failing
// its IPC handshake, an emulator exiting non-zero) used to be discarded
// in the Wait() goroutine — trial #3c (docs/v0.9-trial-3c-findings.md
// Bug D) found that pattern hid three vanished sub-agents. We now capture
// stderr and write a one-line diagnostic to outWriter so the operator
// sees what happened.
//
// outWriter is typically opts.Out (defaults to os.Stdout upstream); a
// nil outWriter drops the post-fork message — kept that way so tests
// can opt out of post-fork noise.
func spawnDetached(cmd *exec.Cmd, sessionName string, outWriter io.Writer) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		err := cmd.Wait()
		if err != nil && outWriter != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg != "" {
				fmt.Fprintf(outWriter, "bosun: launcher (%s) child exited %v: %s\n", sessionName, err, msg)
			} else {
				fmt.Fprintf(outWriter, "bosun: launcher (%s) child exited %v\n", sessionName, err)
			}
		}
	}()
	return nil
}

// macOSLaunchStagger is the minimum gap enforced between successive
// macOS terminal launches. AppleScript / Apple Events (Terminal.app,
// iTerm2) and Ghostty's IPC handshake all get unhappy under tight
// back-to-back invocations from the same caller: trial #3c
// (docs/v0.9-trial-3c-findings.md Bug D) saw three rapid bosun_spawn
// calls in ~8 seconds yield zero visible sub-agent windows. 250ms is
// empirically enough to clear the throttle without making the
// operator wait noticeably for typical session counts (3-4 subs ≈
// 750ms-1000ms total).
const macOSLaunchStagger = 250 * time.Millisecond

var (
	macOSStaggerMu sync.Mutex
	macOSLastAt    time.Time
)

// waitForMacOSStagger holds the package mutex while sleeping just
// long enough to guarantee macOSLaunchStagger has elapsed since the
// previous macOS launch. Two-purpose: serializes concurrent goroutine
// callers AND spaces out back-to-back calls from a single caller.
//
// Cheap on first call (lastAt is zero so no sleep), then bounded to
// macOSLaunchStagger thereafter. Called only from the macOS terminal
// paths — Linux and Windows are untouched per scope.
func waitForMacOSStagger() {
	macOSStaggerMu.Lock()
	defer macOSStaggerMu.Unlock()
	if !macOSLastAt.IsZero() {
		gap := time.Since(macOSLastAt)
		if gap < macOSLaunchStagger {
			time.Sleep(macOSLaunchStagger - gap)
		}
	}
	macOSLastAt = time.Now()
}

func launchTerminalDarwin(opts Options) error {
	// AppleScript-Bridge / Apple Events drop rapid back-to-back messages
	// (trial #3c Bug D). Stagger ensures osascript-2 doesn't race with
	// osascript-1 still wiring up its window.
	waitForMacOSStagger()
	if hasITerm2() {
		return spawnFn(exec.Command("osascript", "-e", iTerm2Script(opts)), opts.SessionName, opts.Out)
	}
	// Terminal.app: do script always opens a new window. There is no clean
	// "open new tab in current window" primitive — the workaround keystrokes
	// ⌘T via System Events, which races on focus. OpenAsTab is honored
	// best-effort by Terminal.app users: they see a new window. Install
	// iTerm2 (auto-detected above) for real tab support.
	envPrefix := buildShellEnvPrefix(opts.Env)
	script := fmt.Sprintf(
		`tell application "Terminal" to do script "cd %s && %s%s"`,
		shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts),
	)
	return spawnFn(exec.Command("osascript", "-e", script), opts.SessionName, opts.Out)
}

// iTerm2Script returns the AppleScript body that opens an iTerm2 window
// or tab and runs opts in the new session. Tab creation targets the front
// window (creating one first if none exists), so a `bosun init --launch`
// run plays out as: first session opens a window, subsequent sessions
// land as tabs in that window when OpenAsTab is set.
func iTerm2Script(opts Options) string {
	envPrefix := buildShellEnvPrefix(opts.Env)
	inner := fmt.Sprintf("cd %s && %s%s",
		shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts))
	// AppleScript string quoting: escape embedded backslashes and quotes
	// so the shell command survives the round trip.
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(inner)

	if opts.OpenAsTab {
		return fmt.Sprintf(`tell application "iTerm"
  activate
  if (count of windows) = 0 then
    create window with default profile
  end if
  tell current window
    set newTab to (create tab with default profile)
    tell current session of newTab to write text "%s"
  end tell
end tell`, quoted)
	}
	return fmt.Sprintf(`tell application "iTerm"
  activate
  set newWindow to (create window with default profile)
  tell current session of newWindow to write text "%s"
end tell`, quoted)
}

func launchTerminalLinux(opts Options) error {
	envPrefix := buildShellEnvPrefix(opts.Env)
	inner := fmt.Sprintf("cd %s && %s%s; exec bash",
		shellQuote(opts.WorktreePath), envPrefix, shellInvocation(opts))
	for _, term := range linuxTerminals {
		if _, err := exec.LookPath(term); err != nil {
			continue
		}
		// gnome-terminal --tab opens in the most-recently-used gnome-terminal
		// window when one exists; falls back to a new window otherwise. Other
		// emulators in linuxTerminals (xterm, konsole, x-terminal-emulator)
		// don't have a comparable cross-version flag, so OpenAsTab is a no-op
		// there — they open a new window like before.
		var cmd *exec.Cmd
		switch term {
		case "gnome-terminal":
			args := []string{"--working-directory", opts.WorktreePath}
			if opts.OpenAsTab {
				args = append(args, "--tab")
			}
			args = append(args, "--", "bash", "-lc", inner)
			cmd = exec.Command(term, args...)
		default:
			cmd = exec.Command(term, "-e", "bash", "-lc", inner)
		}
		return spawnFn(cmd, opts.SessionName, opts.Out)
	}
	return fmt.Errorf("no terminal emulator found")
}

func launchTerminalWindows(opts Options) error {
	// Prefer Windows Terminal when present — it has real tab support via
	// `wt -w 0 new-tab`, which targets the most-recently-used window.
	if wt, ok := hasWindowsTerminal(); ok {
		return spawnFn(exec.Command(wt, windowsTerminalArgs(opts)...), opts.SessionName, opts.Out)
	}
	return spawnFn(exec.Command("cmd", cmdExeArgs(opts)...), opts.SessionName, opts.Out)
}

// cmdExeArgs builds the argv for the cmd.exe fallback path on Windows. Split
// out from launchTerminalWindows so cross-platform unit tests can assert on
// the quoting without actually invoking cmd.exe.
func cmdExeArgs(opts Options) []string {
	envPrefix := buildCmdEnvPrefix(opts.Env)
	cmdLine := opts.Command
	if opts.InitialPrompt != "" {
		// cmd.exe quoting: wrap in double-quotes, escape embedded `"` as `""`.
		quoted := strings.ReplaceAll(opts.InitialPrompt, `"`, `""`)
		cmdLine = fmt.Sprintf(`%s "%s"`, opts.Command, quoted)
	}
	// cmd.exe fallback: no tab support. Use `cmd /c start "" cmd /K` to leave
	// the window open after the command runs. The empty "" after `start` is
	// the window title — required so `start` doesn't misinterpret a quoted
	// path-with-spaces (e.g. "C:\Users\Joe Smith\repo") as the title. The
	// worktree path itself is wrapped in double-quotes for `cd /D`; Windows
	// paths can't legally contain `"`, so no further escaping is needed.
	inner := fmt.Sprintf(`cd /D %s && %s%s`, cmdQuotePath(opts.WorktreePath), envPrefix, cmdLine)
	return []string{"/c", "start", "", "cmd", "/K", inner}
}

// cmdQuotePath wraps a Windows filesystem path in double quotes so cmd.exe
// treats it as a single token even when it contains spaces. Windows paths
// can't legally contain `"` (the character is illegal in NTFS filenames),
// so a naive wrap is safe — no escape pass is needed.
func cmdQuotePath(p string) string {
	return `"` + p + `"`
}

// windowsTerminalArgs builds the `wt.exe` argv. Tab mode targets
// window 0 (the most recently used Windows Terminal window) so a session
// joins the operator's existing window when one's open.
func windowsTerminalArgs(opts Options) []string {
	envPrefix := buildCmdEnvPrefix(opts.Env)
	cmdLine := opts.Command
	if opts.InitialPrompt != "" {
		quoted := strings.ReplaceAll(opts.InitialPrompt, `"`, `""`)
		cmdLine = fmt.Sprintf(`%s "%s"`, opts.Command, quoted)
	}
	inner := fmt.Sprintf(`%s%s`, envPrefix, cmdLine)
	if opts.OpenAsTab {
		return []string{"-w", "0", "new-tab", "-d", opts.WorktreePath, "cmd", "/K", inner}
	}
	return []string{"-d", opts.WorktreePath, "cmd", "/K", inner}
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
