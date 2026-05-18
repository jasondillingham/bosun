# Bosun on macOS — setup notes

These are the things macOS does behind your back that can derail
bosun on a fresh setup. None of them are bosun bugs; all of them are
macOS-default behaviors that interact badly with how git stores its
object database.

If you only read one section, read [the headline](#the-headline).

---

## The headline

**Do not run bosun against a repository inside `~/Documents/`,
`~/Desktop/`, or any path under `~/Library/Mobile Documents/`.**

These are macOS's default iCloud-synced locations. The File Provider
daemon (`fileproviderd`) concurrently reads and writes files in those
trees as part of sync — exactly the wrong thing to do under a tool
that asks `git` to memory-map dozens of small object files
back-to-back. Symptoms range from random `signal: bus error` (SIGBUS)
during `git worktree add` to phantom `file 2.go` duplicates appearing
in your worktree, to (issue #15) iCloud silently stripping the
top-level `HEAD` / `commondir` / `gitdir` files from your worktrees'
git admin dirs — which makes the worktrees invisible to git and
breaks every multi-worktree bosun command.

Starting in v0.10, `bosun init` refuses to run under iCloud-managed
paths by default. The error message will point you here. If you've
already disabled iCloud sync for the dir (e.g. via "Optimize Mac
Storage" or the Documents-folder sync toggle) but bosun's heuristic
still trips, you can override with `bosun init --force-icloud`.

**Use one of these instead:**

- `~/code/` (or `~/projects/`, `~/work/` — any new top-level)
- `/Volumes/<external-drive>/<dir>/` — external SSD/HDD work fine
- `/tmp/` — ephemeral, fine for short experiments

Verify by running `bosun doctor` from your repo's root. The
`filesync-icloud` check will warn if the path is iCloud-managed.

---

## Why this matters

Bosun's safety contract requires `git` to behave deterministically.
`git worktree add` reads many small object files via `mmap`, and if
another process modifies those files between the `mmap` and the
read, the kernel raises SIGBUS and git crashes mid-operation.

In practice this manifests as:

```
$ bosun init 3
system load is 6.42; init may be slow (--no-load-check to skip)
Creating worktree session-1 (1/3)...
Error: bosun: add worktree ...: git worktree add ...: exit status 138:
       Preparing worktree (checking out 'bosun/session-1')
error: reset died of signal 10
```

`signal 10` is SIGBUS. **Bosun's safety contract holds under this
failure** — main isn't touched, no half-state worktree is created,
the breadcrumb `.bosun/init.state` lets you `bosun init --resume`
once the environment is healthy. But the cleanest fix is to remove
the cause: don't run from an iCloud-managed path.

---

## Moving an existing repo out of iCloud

If you already have a repo at `~/Documents/myproject/` and you want
to relocate it, **do not use `mv` or `cp -R`**:

- `mv` becomes a per-file copy when source and dest are on different
  volumes, and each file copy hits iCloud's lazy-hydration round-trip.
- `cp -R` and `ditto` produce phantom "No such file or directory"
  errors on files that demonstrably exist, because iCloud's File
  Provider virtualizes files between `readdir` and `read`.

**Use `tar` piping instead** — single I/O stream, robust against
iCloud's interleaving:

```sh
mkdir -p ~/code
tar -C ~/Documents -cf - myproject | tar -C ~/code -xf -
cd ~/code/myproject
git fsck --no-progress --no-dangling   # confirm integrity
```

`git fsck` exits 0 on a clean copy; any errors point at specific
objects you can debug.

Once the copy is verified, `rm -rf ~/Documents/myproject` (or leave
it as a backup; either way, run bosun against the new location).

---

## Optional belt-and-suspenders: stop Spotlight from indexing

The bigger problem is the File Provider, but Spotlight indexing of a
git repo's object database adds friction even outside iCloud paths.
To opt out:

```sh
touch /path/to/your/repo/.metadata_never_index
```

That single file tells Spotlight to skip the entire tree. Apple
documents this marker; macOS honors it without configuration. The
file is gitignore-safe (no content; just its presence matters), but
you may want to add `.metadata_never_index` to your project's
`.gitignore` so it stays per-developer-machine.

---

## External drives

External SSDs (USB-C, Thunderbolt) work fine for bosun if:

- The volume is **APFS** or **HFS+** (supports Unix sockets — the MCP
  daemon needs these to bind).
- The volume is **not** ExFAT/FAT32 (no Unix socket support; bosun's
  MCP server will fail to start).
- The dock or hub between your Mac and the drive is **stable under
  load** — flaky USB controllers can drop reads during a `bosun init`
  and produce the same SIGBUS-shaped errors as iCloud. If you see
  intermittent failures, try plugging the drive directly to the Mac.

`bosun doctor` includes an `mcp-socket` check that will fail-fast on
filesystems that refuse Unix sockets.

---

## What `bosun doctor` checks (and what `--fix` will auto-repair)

After cloning, `cd` into the repo and run:

```sh
bosun doctor
```

It checks: git version, git on PATH, repo + .bosun/ writability,
iCloud-managed path detection, orphan worktree dirs from prior
cleanups, **worktree admin metadata integrity** (issue #15), stale
`.bosun/init.lock`, phantom branch refs, Unix socket bind capability.

Exit codes: `0` clean, `1` warnings, `2` failures.

For the safe-to-touch issues, `bosun doctor --fix` will:

- Remove stale `.bosun/init.lock` files (>1h old).
- Remove phantom branch refs under `.git/refs/heads/bosun/`.
- Rename orphan `<repo>-bosun-*` directories to `_orphan-<name>` so
  they don't collide with future bosun init runs.
- **Reap phantom and broken admin dirs under `.git/worktrees/`** (the
  issue #15 corruption shape) and run `git worktree prune` afterward.

It will **not** auto-fix the iCloud-path warning — that needs a real
user decision (relocate the repo). Preview first with
`bosun doctor --fix --dry-run`.

---

## Recovery: my worktrees are broken and `bosun status` shows nothing

**If it already happened to you:** see
[`incident-report-icloud.md`](./incident-report-icloud.md) for the
operator-facing recovery flow and a report template to file your
case on [issue #15](https://github.com/jasondillingham/bosun/issues/15).
The short version is below.

This is the issue #15 corruption shape — iCloud File Provider stripped
your worktrees' git admin metadata. Symptoms:

- `bosun status` returns "no sessions" or shows fewer than you created.
- `git worktree list` only shows main (and maybe some of the sessions).
- `cd <repo>-bosun-N && git status` fails with `fatal: not a git
  repository: .../​.git/worktrees/<name>`.
- `.git/worktrees/` contains directories named like `<repo>-bosun-1 2/`
  or `<repo>-bosun-1 (1)/` — those are iCloud's reconciliation phantoms.

Recovery in two commands:

```sh
bosun doctor           # confirms the worktree-admin-integrity check fails
bosun doctor --fix     # reaps phantom + broken admin dirs, runs git worktree prune
```

The fix only touches `.git/worktrees/<name>/` admin dirs — your actual
worktree directories (`<repo>-bosun-N/`) are not removed. Any
uncommitted work in those directories survives. After the fix:

```sh
ls <repo>-bosun-1/     # work is still there
```

You can move the contents elsewhere if you want to keep them. Once
recovery is clean, relocate the repo out of iCloud (see "Moving an
existing repo out of iCloud" above) so it doesn't happen again.

---

## Rapid-fire launches (`bosun_spawn`, `bosun init --launch N`)

When bosun launches multiple agent windows in quick succession on
macOS — `bosun_spawn` from a parent agent that's creating several
sub-sessions, or `bosun init --launch 4` — the underlying
AppleScript / Apple Events plumbing (Terminal.app, iTerm2) and
Ghostty's CLI-to-app IPC handshake drop messages when invoked
back-to-back. Trial #3c
([`docs/v0.9-trial-3c-findings.md`](./v0.9-trial-3c-findings.md),
Bug D) saw three `bosun_spawn` calls in ~8 seconds yield zero
visible sub-agent windows — the parent's process was the only thing
left in the tree.

Bosun mitigates this in two ways, automatically:

1. **250 ms stagger** between successive macOS terminal launches.
   For 3-4 sub-sessions that adds ~750 ms-1 s total — invisible in
   practice.
2. **Post-fork stderr surfacing.** If `osascript` or Ghostty's CLI
   exits non-zero after bosun forked it (the actual failure mode in
   trial #3c), the captured stderr is written to bosun's output as
   `bosun: launcher (session-X) child exited ...`. No more silent
   vanishings.

You only need to know about this if you see the diagnostic line in
your launch output — it points at an AppleScript / Apple Events
failure on the macOS side. The usual fix is the same as for any
broken `--launch`: run `bosun launch <session-N>` manually, or
configure `bosun config launcher tmux` if you'd rather have new
sessions land as tmux windows.

---

## Reporting issues

If you hit a macOS-specific failure that isn't covered above, please
open an issue with:

- The output of `bosun doctor`.
- The output of `mount | grep $(pwd)` (filesystem type + mount opts).
- The git command + exit code that failed.
- Whether the failure repeated after relocating outside iCloud paths.

Environmental drama is real; we'd rather catch it in `bosun doctor`
than have it bite the next person.
