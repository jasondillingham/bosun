# Bosun project state — 2026-05-17 (pre-reboot snapshot)

This is a self-contained handoff so the next session (after the
operator reboots the machine) can pick up cleanly.

---

## Where we are in one sentence

**v0.6.2 just landed**, the second external-repo trial has been
attempted once (got a real validation signal that the v0.6.2
timeout fix works, but the trial itself needs to be re-run on a
healthy machine to actually exercise Phase 2).

---

## Project trajectory (what's shipped)

All squash-merged to `main` in `~/Documents/Homelab/bosun`:

- **v0.2.0** — tagged. Foundation: CLI, status, claims, init/launch,
  cleanup/remove, manual merge.
- **v0.3** — TUI, web UI, named sessions, agent detection
  (`proc.Running`).
- **v0.3.1** — concurrent-claim fix, MCP daemon hardening, tree-
  equivalence merge detection, GOMODCACHE-tree cleanup helpers.
- **v0.4** — hooks scaffolding (`pre-init`, `post-init`,
  `post-done`), `merge --dry-run`, `list`/`show --json`, orphan-dir
  recovery, config CLI (`get` / `set` / `list`).
- **v0.5** — `bosun suggest` (Claude-API-backed brief authoring),
  hooks call-sites (`pre-merge`, `post-merge`, `pre-cleanup`,
  `post-cleanup`, `pre-remove`), per-op git timeout (initial — see
  v0.6.2 below), init progress reporting, pre-flight load check,
  predictive conflict (`bosun predict`), config round-out (`unset`
  / `validate` / `init`).
- **v0.6** — resilience anchor: agent-liveness gate on destructive
  ops (merge, remove, cleanup), pre-merge `git fsck`, reflog merge
  undo (`bosun merge --undo`), CRASHED state + `bosun rescue`,
  heartbeat MCP tool, hook timeout enforcement, init resumability
  (`bosun init --resume`), README "Safety contract" section.
- **v0.6.1** — trial blockers from trial #1: `git worktree add`
  timeout coverage (session-1), `bosun init --resume` actually
  resuming (session-2).
- **v0.6.2** — timeout coverage gap: migrate FsckWorktree,
  rev-parse HEAD, reset --hard from `exec.CommandContext` onto
  `Client.run` so they all inherit `git_op_timeout_seconds`. Adds
  Client-level table-driven test that exercises every public method
  against a fake-hung-git shim — regression-proof against future
  backsliding.

`main` HEAD at snapshot time: `c6bd72a` (v0.6.2 commit).

---

## Public-launch readiness

**Bosun is NOT public yet.** Per `CLAUDE.md`'s "Release stance"
section, the repo doesn't flip to public until the external-repo
trial validates the safety contract in a codebase bosun has never
seen. That gate has NOT been cleared yet.

Trial #1 (2026-05-16) on `homelab-status-mcp` produced two
blockers (silent init hang, broken `--resume`) → fixed in v0.6.1.

Trial #2 attempt (2026-05-17, this session, mid-trial) produced
one validation signal: v0.6.2's timeout fix works — the git
subprocess died from system SIGBUS under crushing load (13.83 avg)
and bosun surfaced the error cleanly instead of hanging silently.
**But the trial itself didn't reach Phase 2** because the system
was too overloaded to give a clean signal. Needs re-run on a
healthy machine.

---

## Trial #2 in-progress state (resume target)

**Trial repo:** `~/Documents/Homelab/homelab-status-mcp`
- HEAD: `667b7f3de7618d850381af8d46440a46f2c97a76` (unchanged
  through both trial attempts — safety contract holding)
- Trial plan: `v0.6-trial-plan.md` in repo root (3 disjoint lanes:
  cloudflare, tailscale, synology test coverage)
- `.bosun/init.state` is **preserved intentionally** as a `--resume`
  test target post-reboot. Contents:
  ```json
  {
    "version": "v0.6",
    "started_at": "2026-05-17T11:47:48Z",
    "plan_path": "v0.6-trial-plan.md",
    "total_sessions": 3,
    "labels": ["session-1", "session-2", "session-3"],
    "completed_sessions": [],
    "current_session": "session-1",
    "current_step": "git_worktree_add"
  }
  ```
- No worktrees on disk (init failed at the first
  `git worktree add` so nothing got partially created)

**After reboot, the resume test:**
```sh
cd ~/Documents/Homelab/homelab-status-mcp
~/go/bin/bosun init --resume   # should detect init.state, pick up
```

If `--resume` works correctly, it should create all 3 worktrees
fresh (since current_step was on the FIRST step that failed
entirely, no actual state to recover). If it doesn't, that's a
v0.7 finding.

Alternative: delete `.bosun/init.state` and start fresh with
`bosun init 3 --brief v0.6-trial-plan.md`. The plan file is still
there.

---

## Bosun binary state

- **Source built at:** `c6bd72a` (v0.6.2)
- **Installed location:** `~/go/bin/bosun`
- **PATH note:** `~/go/bin` is not on the interactive PATH;
  invocations in this session used the absolute path. To make
  `bosun` callable as a bare command, add to `.zshrc`:
  ```sh
  export PATH="$HOME/go/bin:$PATH"
  ```
  Or symlink:
  ```sh
  sudo ln -s ~/go/bin/bosun /usr/local/bin/bosun
  ```

- **MCP daemon:** was running as PID 82378 on
  `~/Documents/Homelab/bosun/.bosun/mcp.sock`. Will die at reboot;
  any `bosun launch` after reboot will respawn it.

---

## Pre-reboot system state (for context)

- Load: 13.83 / 11.02 / 8.30 (1/5/15 min)
- Free disk: ~95 GB on `~`
- corespotlightd at 57% CPU (Spotlight reindex, lingering from
  trial-prep cache cleanup days ago)
- WindowServer 46% (open Ghostty windows accumulating)
- Existing claude conversation (this one) on PID 50871 running
  since 2026-05-15 — will die with reboot

---

## Findings docs (read these for full context)

- `docs/v0.4-findings.md` — v0.4 round bugs + the "agent-crash /
  API-error / hung-session resilience" TODO that became v0.6's
  anchor.
- `docs/v0.5-roadmap.md` — v0.5 scope.
- `docs/v0.5-suggest-spec.md` — `bosun suggest` design.
- `docs/v0.6-roadmap.md` — v0.6 resilience anchor (eight asks).
- `docs/v0.6-trial-protocol.md` — operator playbook for the
  external-repo trial.
- `docs/v0.6-trial-findings.md` — trial #1 results + addendum
  for the v0.6.1 round's fsck finding.

Plan markdowns (gitignored, in repo root):
- `v0.5-round{1,2,3}-plan.md`, `v0.6-round1-plan.md`,
  `v0.6.1-plan.md`, `v0.6.2-plan.md`

---

## v0.7 candidates surfaced during this session

Not blockers for public launch, but capture before forgetting:

1. **`bosun launch <session>` default prompt.** Today the standalone
   `bosun launch session-N` opens claude with no prompt — agent
   sits idle. `bosun init --launch` injects "Read BOSUN_BRIEF.md".
   Standalone launch should match.
2. **Predictor false-positives on "Files (avoid)" sections.**
   `bosun predict` treats avoid-list paths as predicted edits.
   v0.6 round 1 plan triggered 79 false-positive overlaps. Fix:
   ignore lines under "Files (avoid)" / "Do not touch" headings.
3. **`bosun init` should reset stale session branches.** If a
   session branch already exists (from a prior round), `bosun init`
   silently creates a worktree pointing at the stale commit. Either
   reset or refuse with a clearer error than "worktree path already
   exists."
4. **macOS Finder phantom `.done` files.** `.bosun/state/session-1 2.done`
   etc. keep appearing — Spotlight or Time Machine duplicates files
   when it sweeps `.bosun/`. Either ignore the ` 2.done` shape on
   read or document a `.bosun/.metadata_never_index` marker.
5. **Worktree gitdir corruption when agent dies mid-task.** Observed
   v0.6.2 session-1's `.git/worktrees/bosun-bosun-1/` reduced to
   just `index` (no HEAD/commondir/config) after agent crash. Cause
   unknown — may be agent's last action or post-crash cleanup race.
   `bosun rescue` should detect this case explicitly (currently
   treats it as recoverable but the gitdir state breaks all git
   operations on the worktree).
6. **Self-experimentation hazard.** v0.6.1 session-2 ran
   `git worktree remove` on its own worktree while testing the
   init-resume code — orphaned the branch+commits in git's view.
   Bosun working ON bosun specifically exposes this; not a real
   concern for external-repo trials.
7. **Lane-discipline lesson recurring.** Three rounds in a row,
   conflicts hit `cmd/bosun/scenarios_test.go` because every lane
   appends at EOF. Split into per-feature test files
   (`scenarios_merge_test.go`, `scenarios_predict_test.go`, etc.)
   so lanes append to disjoint files.

---

## How to pick up after reboot

In rough priority order:

1. **Verify nothing weird in `main`.** `cd ~/Documents/Homelab/bosun
   && git status && git log --oneline -5`. Should be clean at
   `c6bd72a` or whatever its hash is post-reboot (no rebooting
   shouldn't change commits).
2. **Confirm bosun binary still works.** `~/go/bin/bosun --help`
   and `~/go/bin/bosun config show`.
3. **Re-run trial #2** against `homelab-status-mcp` on a healthy
   machine (load < 5). Use `--resume` to test that feature, or
   delete `.bosun/init.state` for a clean Phase 1 start.
4. **Read `docs/v0.6-trial-protocol.md` Phase 2 + 3** for the
   gate-stressing exercises (force-close a Ghostty mid-edit to
   exercise CRASHED + rescue; try merge with live agent for
   refusal; run `merge --undo` to verify reflog reset; etc.).
5. **After trial passes:** write `docs/v0.6.2-trial-findings.md`,
   then decide whether to ship v0.1 publicly per `CLAUDE.md`
   release stance.
6. **Defer v0.7 candidates** until v0.1 ships. The
   compounding-not-commits feedback applies: don't pile on
   features before validating the safety contract publicly.

---

## What to NOT do after reboot

- **Don't** push bosun anywhere public yet (`CLAUDE.md` release
  stance).
- **Don't** delete the trial repo's `.bosun/init.state` without
  first trying `bosun init --resume` — it's a real test case for
  v0.6.1's resume work.
- **Don't** kick off v0.7 features while trial #2 is unresolved.
  Compounding > commits.
- **Don't** worry about the pre-reboot Spotlight/WindowServer/
  cloudd CPU — reboot drops it all.
