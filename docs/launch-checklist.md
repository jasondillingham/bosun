# Bosun public-launch checklist

The exact sequence for flipping bosun from a private maintainer
tool to a public-facing one. Live document — check items off as
they're done; don't skip.

Each step is paired with a verification command. If a step's
verification doesn't pass, **stop** and resolve before continuing.
The launch flip is not the place to discover that `make check`
fails.

---

## Phase 0 — Pre-flight (do this before scheduling the launch)

- [ ] **All v0.8 lanes landed.** `git log --oneline origin/main..HEAD` empty (everything pushed to the private origin).
- [ ] **`make check` clean on local machine.**
- [ ] **`make stress` clean.** Concurrency/race tests pass with `-race`.
- [ ] **`FUZZTIME=2m make fuzz` clean.** Short fuzz run on every seed corpus; no new failures.
- [ ] **`bosun doctor` on this repo shows zero FAIL.** WARNs from the iCloud-sync detection on the maintainer's machine are acceptable (the repo will be cloned elsewhere by users). FAILs are not.
- [ ] **CI passes on the latest commit.** Check `gh run list --limit 5` — every workflow green.
- [ ] **No accidentally-committed secrets.** Run `git log --all -p | grep -iE '(api[_-]?key|secret|token|password|aws_)' | grep -v RELEASES.md | head -20`. Anything that looks live needs `git filter-repo` before the flip — once a private repo goes public, history is exposed.

---

## Phase 1 — External-repo trial #2

The gate between v0.8 and the public flip per `CLAUDE.md`.

- [ ] **Read the protocol.** [`docs/v0.6-trial-protocol.md`](./v0.6-trial-protocol.md) is the authoritative checklist. The doctor + suggest additions are bonuses to verify but the core flow is unchanged.
- [ ] **Pick a clean external repo.** `~/Documents/Homelab/homelab-status-mcp` was the original target. If it's drifted (the maintainer worked on it directly since the v0.6 trial), either reset to a known-good SHA or pick a different one.
- [ ] **Pre-flight via `bosun doctor` in the trial repo.** Should be PASS or WARN (iCloud if applicable). Any FAIL = stop, fix bosun, retry.
- [ ] **System load < 5.** Per `STATUS.md`'s pre-reboot guidance, run `uptime` first; the trial under crushing load reproduces environmental drama rather than bosun bugs.
- [ ] **Run trial Phase 1 (init).** `~/go/bin/bosun init 3 --brief <plan.md>` against the trial repo. Expected: 3 worktrees, 3 branches, 3 BOSUN_BRIEF.md files. No surprises.
- [ ] **Run trial Phase 2 (multi-session round).** Launch each session; observe agents work; verify `bosun status` updates correctly; observe `bosun done` flowing through.
- [ ] **Run trial Phase 3 (resilience gates).** Force-close one Ghostty window mid-edit (exercise CRASHED + rescue). Try `bosun merge` with a live agent (expect refusal). Run `bosun merge --undo` to verify reflog reset.
- [ ] **Document the outcome.** Write `docs/v0.8-trial-findings.md` describing what happened — successes, failures, recovery paths. Even an uneventful trial deserves a one-paragraph "nothing surprising" doc; future maintainers will look for it.
- [ ] **Decide: ship or fix.** If the trial surfaced any safety-contract violations, fix them in a v0.8.x point release and re-run the trial. Don't proceed to Phase 2 with known unrepaired safety bugs.

---

## Phase 2 — Tag the release

- [ ] **Pick the tagging strategy.** Two reasonable shapes, decide before tagging:
  - **Granular:** `v0.7.0` at the v0.7 round-1 merge, `v0.7.1`/`v0.7.2` for bug-hunt waves, `v0.8.0` at the launch commit. More tags, more history.
  - **Coalesced:** `v0.7.0` at the last pre-v0.8 commit (single tag covering everything since v0.6.2), `v0.8.0` at the launch commit. Two tags. Simpler.
  - **Bias toward coalesced** — v0.7's bug-hunt + refactors all landed in this private development window; there's no consumer who installed a granular v0.7.1.
- [ ] **Tag v0.7.0 (annotated, signed if you sign tags).**
  ```sh
  cd ~/Documents/Homelab/bosun
  git tag -a v0.7.0 <SHA-of-last-v0.7-commit> -m "v0.7 — polish round + bug-hunt + refactors + future-bug-hunting test suite (see RELEASES.md)"
  ```
- [ ] **Tag v0.8.0 at HEAD.**
  ```sh
  git tag -a v0.8.0 HEAD -m "v0.8 — public-launch readiness (see RELEASES.md)"
  ```
- [ ] **Push tags to private origin first.** `git push origin v0.7.0 v0.8.0`. Verify both show in `gh release list` (or `git ls-remote --tags origin`).

---

## Phase 3 — Repo-state final pass

- [ ] **Update `CLAUDE.md` release stance.** Remove the "Bosun is NOT public yet" section, or rewrite it to reflect the launched state ("Bosun is public under Apache 2.0; here's what contributors should know"). Commit + push to origin.
- [ ] **Confirm README accuracy.** `bosun init --help`, `bosun doctor --help`, `bosun --help` outputs match what the README documents. Discrepancies caught now are easier than after the HN landing.
- [ ] **Confirm `RELEASES.md` covers every tag.** `git tag --list 'v*'` + a manual scan of `RELEASES.md` headings.
- [ ] **Verify `LICENSE` is present, valid Apache 2.0, and the copyright year + name are correct.**

---

## Phase 4 — The actual flip

The one-line command that's the actual launch. **Don't run this until everything above is checked.**

- [ ] **Flip visibility.**
  ```sh
  gh repo edit jasondillingham/bosun --visibility public --accept-visibility-change-consequences
  ```
  Output: `✓ Edited repository jasondillingham/bosun`. The repo is now public — all history is exposed.

- [ ] **Push tags (in case the private push earlier got rolled back).**
  ```sh
  git push origin v0.7.0 v0.8.0
  ```

- [ ] **Create GitHub Releases for both tags.** Pulls release notes from `RELEASES.md`. Optional but adds discoverability.
  ```sh
  gh release create v0.7.0 --notes-from-tag --title "v0.7 — polish + bug-hunt + test suite"
  gh release create v0.8.0 --notes-from-tag --title "v0.8 — public launch"
  ```

- [ ] **Sanity-check from a fresh clone.** In a new terminal:
  ```sh
  cd /tmp && rm -rf bosun-sanity && git clone https://github.com/jasondillingham/bosun.git bosun-sanity && cd bosun-sanity && go build ./cmd/bosun && ./bosun doctor
  ```
  Doctor on the freshly-cloned repo should be all PASS (no iCloud since `/tmp` is outside `~/Documents`).

---

## Phase 5 — Announce (optional, low priority)

The launch itself is the milestone. Anything below is amplification.

- [ ] **HN "Show HN: bosun" post.** Title ideas in `docs/blog/`.
- [ ] **Blog post.** Drafts in `docs/blog/`.
- [ ] **Social.** X / Mastodon / Bluesky as preferred.

---

## Phase 6 — Hold

After the flip and any announcements: **stop building for a week.** Per `docs/v0.8-roadmap.md`'s "After v0.8" section:

> Watch issues, watch PRs, watch any community engagement. v0.9 priorities come from what real users actually hit, not from another internal round.

Resist the urge to ship a v0.8.1 in the first 24 hours unless something genuinely safety-affecting surfaces. The compounding-not-commits principle is at its strongest right after a launch.

---

## Aborting

If any of the trial findings reveal a safety-contract violation (bosun corrupted the trial repo, touched main without `bosun merge`, leaked branches outside `bosun/` prefix), **stop the launch entirely**. Fix in v0.8.x, re-run the trial, re-check the list. The contract is the load-bearing piece; everything else can wait.

If the flip happens and an emergency surfaces in the first hours: the repo can be flipped back to private:

```sh
gh repo edit jasondillingham/bosun --visibility private
```

Note this doesn't un-publish anyone's local clones. But it does stop the leak surface and gives breathing room to ship a real fix.
