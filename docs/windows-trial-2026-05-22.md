# Windows trial — 2026-05-22

First live trial of bosun against a real Windows host. Run via SSH from
the maintainer's macOS box into a dockur-managed Windows 11 25H2 VM
(`WIN-M8T236RVCKU`, build `10.0.26200.0`) hosted as a Kubernetes pod on
Thor.

This is the empirical backing for the README's "✓ supported" Windows
claim. Until this trial, that claim rested only on unit tests pinning
argv shape — never on a real binary running against a real Windows
filesystem.

## Environment

| Component | Version | Notes |
|---|---|---|
| OS | Windows 11 25H2 (NT 10.0.26200.0) | Default `Docker` user (admin) |
| Shell | PowerShell 5.1.26100.7920 | Built-in; PS7 not installed |
| sshd | running (OpenSSH 9.5p1) | Pre-installed in dockur image |
| Windows Terminal | `wt.exe` 1.x at `C:\Users\Docker\AppData\Local\Microsoft\WindowsApps\wt.exe` | Available pre-install |
| winget | present | Default on Win11 |
| Git | 2.54.0.windows.1 | Installed via `winget install Git.Git` |
| Go | 1.26.3 windows/amd64 | Installed via `winget install GoLang.Go` |
| Other tools | none | No `claude`, no `sh`, no `sleep` |

## What worked

End-to-end operator workflow at `C:\Users\Docker\trial\test-repo`:

| Step | Outcome |
|---|---|
| `git clone --depth 1 https://github.com/jasondillingham/bosun.git` | Clean |
| `go build -ldflags='-X main.version=trial-2026-05-22' -o bosun.exe ./cmd/bosun` | Clean compile, no Windows-specific build errors |
| `bosun --version` | Reports correctly |
| `bosun init 2 --brief plan.md` | Created 2 worktrees with paths like `test-repo-bosun-20260522-222836-3156-1` (timestamp + PID + ordinal scheme intact). `.bosun/` directory populated correctly — `briefs/`, `claims/`, `init.lock`, `spawn-tree.json`, `spawn-tree.lock`, `.metadata_never_index`, etc. |
| `bosun status` | Rendered the table correctly with backslash paths |
| `bosun claim session-1 lane-a.txt` | Registered path |
| `bosun done session-1 -m "..."` | Transitioned to DONE |
| `bosun merge --dry-run` | Previewed both squashes |
| `bosun merge` | Squash-merged both branches onto `main`; commits visible in `git log` |
| `bosun cleanup` | Reaped both worktrees and branches |
| `bosun status` (post-cleanup) | Clean "no sessions" |

**Path with spaces:** repeated the entire cycle at `C:\Users\Docker\my space trial\test-repo`. **Worked identically.** Worktree path was `C:\Users\Docker\my space trial\test-repo-bosun-20260522-222936-1228-1`, brief written correctly, git operations completed cleanly. The path-quoting helpers in the launcher and the platform-neutral `filepath.Join` usage hold up.

## Test suite results

`go test ./... -count=1 -timeout 900s` (without `-race` — see below).

**29 packages green:** brief, briefscaffold, claims, claudehook, config, debug, doctor, events, git, history, init, launcher, phantom, predict, preflight, session, spawntree, state, status, subtask, suggest, tui, tui/control, usage, web, webhooks.

**4 packages with failures:**

### `internal/hooks` — sh-dependent test fixtures

Tests construct hook commands like `sh -c 'echo X'` to exercise the
hook runner. Windows has no `sh` on default PATH. The production code
is fine — operators on Windows would set `hooks[*].command` to
`cmd /c …` or `powershell -c …`, which exec.LookPath resolves. **Fix:
skip the affected tests on Windows.**

### `internal/remote/sshtunnel_test.go` — sleep-dependent fixtures

Tests stub a long-running process with `sleep 30`. Same shape: no
`sleep` on Windows PATH. The remote-docker pipeline itself is Linux-
shaped (it SSHes from any host into a Linux docker host) so the tests
exercising the bridge wiring on Windows aren't load-bearing for
Windows operators. **Fix: skip on Windows.**

### `internal/mcp/server_test.go` — `TestServer_Listen_SocketOwnerOnly`

Asserts the Unix socket lands at mode `0o600`. On Windows the test
reports `0o666` (NTFS doesn't honor POSIX mode bits via the same
syscall path). The MCP socket isn't actually used on Windows — bosun's
MCP transport over Unix sockets is Linux/macOS-shaped — but the test
currently runs unconditionally. **Fix: skip on Windows.** (Other tests
in `internal/mcp` already skip with the "Unix sockets aren't supported
on Windows runners" guard; this one was missed.)

### `internal/proc/terminate_test.go` — `TestTerminate_AlreadyGone` ⚠️ real bug

Test spawns `cmd /c exit /b 0`, captures the PID, then calls
`proc.Terminate(pid, 200ms)` expecting nil (already-gone is a no-op
success). On Windows this returns
`find process N: OpenProcess: The parameter is incorrect`.

Root cause: `os.FindProcess(pid)` always succeeds on POSIX (returns
an opaque handle), but on Windows it opens the process handle via
`OpenProcess`. When the PID is gone, OpenProcess fails with various
shapes including `ERROR_INVALID_PARAMETER` ("The parameter is
incorrect"). `proc.Terminate` wraps the error as
`"find process N: %w"` and returns it — defeating the documented
"already-gone is success" contract.

**This is a real production-affecting bug.** Bosun cleanup against a
session whose agent has already exited can return an error instead of
silently no-oping. **Fix: short-circuit on `IsAlive(pid)` before
touching FindProcess, on every platform** — already-gone PID returns
nil immediately. Bonus: faster (no syscall on the common case).

## `-race` requires cgo on Windows

```
go: -race requires cgo; enable cgo by setting CGO_ENABLED=1
```

Windows Go installs don't ship a C toolchain by default (the install
is the Go compiler only — no MinGW, no TDM-GCC). Enabling cgo would
require installing GCC separately. Two acceptable answers:

1. **Drop `-race` on Windows CI.** The race detector catches concurrent
   data races; the same packages run with `-race` under Linux CI, so
   races detected anywhere will surface there. Windows CI runs without
   `-race` confirms the *build* and *test correctness* on Windows
   without duplicating coverage.
2. **Install MinGW in CI.** Possible via `setup-mingw` action or a
   chocolatey install. Adds CI minutes for no clear marginal coverage.

Going with (1). The CI workflow needs to split `go test -race` into
two steps: `-race` on POSIX, plain `go test` on Windows.

## Bootstrap recipe (for future first-runs)

```pwsh
# As Docker (admin) over SSH
winget install -e --id Git.Git --scope user --accept-package-agreements --accept-source-agreements --silent
winget install -e --id GoLang.Go --accept-package-agreements --accept-source-agreements --silent
git config --global user.email '...' ; git config --global user.name '...' ; git config --global init.defaultBranch main

git clone --depth 1 https://github.com/jasondillingham/bosun.git C:\Users\Docker\src\bosun
cd C:\Users\Docker\src\bosun
go build -ldflags='-X main.version=trial' -o bosun.exe ./cmd/bosun
.\bosun.exe --version
```

Note: `Git.Git` accepts `--scope user`. `GoLang.Go` requires machine-
wide install (no user-scope option) — the dockur image's `Docker` user
is admin so this is silent; on a non-admin Windows user it would
prompt for elevation.

## Findings to fix (first-pass — SSH-only)

| # | Severity | Where | Fix |
|---|---|---|---|
| 1 | High | `internal/proc/terminate.go` | Short-circuit `Terminate` on `IsAlive(pid) == false` before FindProcess. Fixes a real production bug on Windows. |
| 2 | Medium (CI) | `.github/workflows/ci.yml` | Split `go test` step into POSIX (`-race`) vs Windows (plain) so the Windows runner can pass. |
| 3 | Low | `internal/hooks/hooks_test.go` | Add `runtime.GOOS == "windows"` skip guards to sh-dependent tests. |
| 4 | Low | `internal/remote/sshtunnel_test.go` | Same — skip sleep-dependent tests on Windows. |
| 5 | Low | `internal/mcp/server_test.go` `TestServer_Listen_SocketOwnerOnly` | Add the existing "Unix sockets aren't supported on Windows" skip pattern. |

Items 1-5 all shipped same day (commits `ce7dc8f` and `b33f037`).
Items 2-5 bundled; item 1 was its own commit. The existing
`TestTerminate_AlreadyGone` test now passes on Windows after the fix.

## Second-round validation — VNC console (2026-05-22 evening)

The first-pass run was over SSH, which has no interactive Windows
desktop session. AppX-aliased binaries (`wt.exe`) silently refuse to
surface a window without one — so the SSH run produced inconclusive
launcher results (TEST 1 reported success but no terminal processes
spawned; TEST 2 surfaced cmd windows but markers couldn't validate
end-to-end).

Repeated the trial from the **VNC console** with a richer instrumented
PowerShell script (`bosun-trial.ps1` left on the Desktop):

- fresh `git init`, plan with 2 sub-sessions
- `marker.cmd` committed to the repo so every worktree inherits it
- `.bosun/config.json` sets `agent_command` to `marker.cmd` — pure
  batch, no nested quoting, so it survives the launcher's `cmd /K …`
  invocation chain
- each launched window's marker.cmd writes `launched-marker.txt` with
  its `%CD%` and timestamp, giving us machine-verifiable proof that
  the launcher cd'd into the right worktree before invoking the agent

### Result matrix

| Test | Exit | New procs | Markers | Verdict |
|---|---|---|---|---|
| 1 — `wt.exe` launcher | 0 | 2 (`cmd`) | **2/2** | **PASS** |
| 2 — `cmd.exe` fallback (wt.exe renamed away) | 0 | 4 (2× `cmd`, 2× `conhost`) | **0/2** | **PARTIAL — windows opened but agent_command didn't run** |
| 3 — `bosun tui --help` | 0 | n/a | n/a | PASS |

The `wt.exe` path is end-to-end verified. The fallback path opens
windows but the inner `cd /D … && marker.cmd` doesn't actually run
the agent. See finding C below.

## Findings — second round

### A. `wt.exe` launcher verified end-to-end ✓

Headline: the path documented in `README.md` ("Terminal launcher
prefers `wt.exe`…") now has empirical backing on a real Win11 25H2
host. windows opened, `cd` landed in the correct worktree, agent
command executed in that worktree, marker file landed where expected.

### B. `bosun cleanup --force` fails on open launched terminals — Low/Med

When a `bosun cleanup --force` runs while the launched terminal
windows are still open, git's worktree removal fails with the bare
operator-facing error:

```
bosun: remove worktree C:/…: git worktree remove …: exit status 255:
error: failed to delete 'C:/…': Permission denied
```

Windows refuses to delete a directory any process has open as its
cwd. Each launched terminal window holds its worktree dir open via
its `cmd` cwd, so cleanup pinions on them.

**Fix path (not done):** when `bosun cleanup` hits this Windows-shape
error, walk `Win32_Process` for cmd/conhost processes whose
command-line mentions the worktree path, surface them by PID and
name in the error ("worktree pinned by PID 1234 (cmd.exe) — close
the launched terminal and retry"). Aggressive alternative: with a
`--terminate-blockers` flag, kill them automatically before retrying.

### C. `cmd.exe` fallback launcher: `cd /D … && agent_command` quoting collapses — Med

`internal/launcher/launcher.go`'s `cmdExeArgs` constructs:

```
cmd /c start "" cmd /K cd /D "<worktree>" && <agent_command>
```

Go's `exec.Command` then wraps the whole `cd /D "<worktree>" &&
<agent_command>` block as a single argv element. When cmd.exe parses
that on the way into the inner `/K`, its complex
"more-than-two-quotes-with-special-chars" rule (`cmd /?` describes
it) strips the outer quotes and leaves the embedded `\"` as literal
chars. The launched window's cmd then sees:

```
cd /D \"C:\path\to\worktree\" && marker.cmd
```

It interprets `cd /D \` as cd to drive root (taking the
backslash-quote as garbage), the `&& marker.cmd` then runs from
drive root where `marker.cmd` isn't on PATH, and the window sits
at a useless prompt at `C:\`.

Effect:
- A window DOES open (4 new processes in the diff: 2 cmd + 2
  conhost) so naive testing says "launcher worked"
- But the cd didn't land in the worktree, AND the agent_command
  never ran
- A grep across the entire filesystem for `launched-marker.txt`
  after the run found zero files — concrete proof the agent never
  executed

Practical impact:
- On Win11 25H2 wt.exe is built-in, so virtually every operator
  hits the wt.exe path (which works). The fallback is invoked only
  when an operator explicitly removes wt.exe, OR on Win10 without
  the Windows Terminal opt-in install.
- For operators who DO hit the fallback, `bosun init --launch`
  appears to work (windows pop up) but agent sessions never start
  and the operator wonders why their agents aren't showing up in
  `bosun status` as RUNNING.

**Fix path (not done):** stop chaining `cd /D && agent_command` as
a single argv element through cmd.exe's parser. Options:
1. Write a per-launch `.bosun/launch-<session>.cmd` helper inside
   the worktree that does the cd-and-run, and invoke it via
   `cmd /K <path-to-helper>`. The helper file's contents stay
   inside the file system, no shell-argv quoting in the picture.
   This is what `marker.cmd` already proved works.
2. Use `wt.exe` (which works) when available; refuse to fall back
   to cmd.exe with a clear "Windows Terminal required" error.
3. Investigate the documented "use backslashes to escape inside
   /K" pattern; brittle and version-dependent.

Option 1 is most robust and matches the marker.cmd pattern.

### D. PowerShell 5.1 console mangles `·` (cosmetic) — Low

`bosun tui --help` prints key bindings separated by `·` (Unicode
U+00B7 middle-dot). PowerShell 5.1's console renders this as `-+`
because its default code page (cp437/cp1252) doesn't have a
single-glyph mapping for it. The same character renders correctly
in Windows Terminal (UTF-8 aware) and in PowerShell 7. Likely also
visible in the actual Bubbletea TUI's status line. README's Windows
notes section should mention that PS 5.1 console rendering is
suboptimal and recommend Windows Terminal / PS7.

## What we still haven't trialed

| Item | Why deferred | Likely outcome |
|---|---|---|
| `bosun tui` rendering (actual full-screen Bubbletea) | Needs human eyeballs in VNC; the script can only `--help` smoke-test | Bubbletea should render correctly in Windows Terminal; PS 5.1 console will mangle Unicode glyphs (finding D) |
| `bosun serve` HTTP dashboard | Not blocked, just not exercised | Should work; `net/http` and the embedded static asset path are cross-platform |
| `claude` MCP integration | Claude binary not installed; this trial was bosun-only | Anthropic's installer ships `claude.cmd`, exec.LookPath resolves; should work |
| `bosun_attach` cwd validation behavior | The L2 cwd validation is documented no-op on Windows; real-world flow not exercised but tests pass | Should fall through cleanly |
