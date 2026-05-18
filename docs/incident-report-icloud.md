# Bosun iCloud worktree-corruption — incident report

If you're reading this because `bosun doctor` flagged
`worktree-admin-integrity` as **FAIL**, or because `bosun status`
shows fewer sessions than you created — this is the recovery flow,
plus the report template to file your case back to the maintainer.

It happens when bosun runs against a repo under iCloud Drive
(typically `~/Documents/` or `~/Desktop/` on a default macOS
install). iCloud's File Provider quietly strips small files out of
`.git/worktrees/<name>/` and bosun loses its view of the sessions.

You haven't lost any work. Read the recovery flow below first.

## Recovery — two commands

From the repo root:

```sh
bosun doctor           # confirms worktree-admin-integrity FAILs
bosun doctor --fix     # reaps the corrupted admin dirs
```

**Your worktree content is preserved.** The fix only removes
`.git/worktrees/<name>/` (git's per-worktree metadata, ~6 small
files). Your actual worktree directories — `<repo>-bosun-1/`,
`<repo>-bosun-2/`, etc. — are not touched. Any uncommitted changes,
new files, or in-flight work in those directories is recoverable:

```sh
ls <repo>-bosun-1/     # your files are still here
```

If you want to rescue uncommitted work before deciding what to do
with it, copy the directory out to a non-iCloud location first
(`tar -C <parent> -cf - <repo>-bosun-1 | tar -C ~/code -xf -`) and
then re-run `bosun doctor --fix`.

After the fix, **relocate the repo out of iCloud** before running
`bosun init` again, or it will happen again. See
[macos-setup.md](./macos-setup.md#moving-an-existing-repo-out-of-icloud).

## Report template

Please file your incident as a comment on issue #15 so the field
data accumulates in one place:

**https://github.com/jasondillingham/bosun/issues/15**

Copy this template, fill in the blanks, and paste it as a new
comment. The commands are safe to run — they read state, they don't
change anything.

```markdown
### Environment

- macOS version: <e.g., 15.2 / Sequoia — `sw_vers -productVersion`>
- iCloud Drive enabled: <yes / no — System Settings > Apple ID > iCloud>
- Repo path: <e.g., ~/Documents/myproject>
- bosun version: <output of `bosun --version`>
- git version: <output of `git --version`>

### What I was doing

<one or two sentences: were you running `bosun init`, working in a
session, opening a fresh terminal, returning to the machine after
sleep, etc.>

### bosun doctor output

```
<paste the full output of `bosun doctor` here>
```

### .git/worktrees contents

```
<paste the output of `ls -la .git/worktrees/` here>
```

### git's view of worktrees

```
<paste the output of `git worktree list` here>
```

### Anything else

<phantom dirs you noticed, Finder behavior, whether iCloud was
actively syncing, prior bosun runs on the same repo, etc.>
```

That's it. The maintainer reads every comment on that issue.

## What we know so far

The corruption shape is consistent across reports:

- The files git writes to `.git/worktrees/<name>/` — `HEAD`,
  `commondir`, `gitdir` — get stripped, sometimes within minutes
  of `bosun init` completing successfully.
- Other files in the same dir (`index`, `logs/HEAD`, `refs/`)
  often survive. The asymmetry is part of why this is invisible to
  `git worktree list`, which silently skips broken entries.
- iCloud File Provider's reconciliation phantoms (dirs named like
  `<repo>-bosun-1 2/` or `<repo>-bosun-1 (1)/`) sometimes appear
  alongside the corruption. `bosun doctor --fix` reaps these too.
- Reproduces on iCloud-managed APFS volumes. **Does not reproduce
  on `/tmp/`, `~/code/`, or other non-iCloud paths.** That's the
  evidence iCloud's File Provider daemon is the trigger.

The full forensic trace from the round that first surfaced this
shape is in
[macos-worktree-corruption-forensics.md](./macos-worktree-corruption-forensics.md).
Background on which macOS paths are iCloud-managed by default and
how to relocate out of them is in
[macos-setup.md](./macos-setup.md).

Since v0.10, `bosun init` refuses to run from an iCloud-managed
path by default, so new installs are protected. This doc exists for
the operators who already hit the corruption shape before that
guard landed, or who overrode the guard with `--force-icloud` and
discovered why it's there.
