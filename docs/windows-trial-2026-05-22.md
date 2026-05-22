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

## What we still haven't trialed

| Item | Why deferred | Likely outcome |
|---|---|---|
| `bosun init --launch` opening real `wt.exe` windows | SSH session has no GUI desktop attached, so spawned windows would have nowhere to render | Should work when run interactively; argv unit-tested |
| `cmd.exe` fallback path | Same — needs interactive console | Same |
| `bosun_attach` PID validation behavior | Touched on test-skip; the L2 cwd validation is documented no-op on Windows, but real-world flow needs verification | Should fall through cleanly; tests pass |
| `bosun tui` rendering | TTY-dependent | Bubbletea should render fine in Windows Terminal |
| `bosun serve` HTTP dashboard | Not blocked, just not exercised | Should work; net/http is cross-platform |
| `claude` MCP integration | Claude binary not installed; this trial was bosun-only | Anthropic's installer ships `claude.cmd`, exec.LookPath resolves; should work |

## Findings to fix

| # | Severity | Where | Fix |
|---|---|---|---|
| 1 | High | `internal/proc/terminate.go` | Short-circuit `Terminate` on `IsAlive(pid) == false` before FindProcess. Fixes a real production bug on Windows. |
| 2 | Medium (CI) | `.github/workflows/ci.yml` | Split `go test` step into POSIX (`-race`) vs Windows (plain) so the Windows runner can pass. |
| 3 | Low | `internal/hooks/hooks_test.go` | Add `runtime.GOOS == "windows"` skip guards to sh-dependent tests. |
| 4 | Low | `internal/remote/sshtunnel_test.go` | Same — skip sleep-dependent tests on Windows. |
| 5 | Low | `internal/mcp/server_test.go` `TestServer_Listen_SocketOwnerOnly` | Add the existing "Unix sockets aren't supported on Windows" skip pattern. |

Items 2-5 are bounded and can ship in one commit. Item 1 deserves its
own commit with proper test coverage of the fix (which the existing
`TestTerminate_AlreadyGone` already provides — it currently fails on
Windows, will pass after the fix).
