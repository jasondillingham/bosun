---
name: Safety contract violation
about: Bosun did something it isn't supposed to do — touched main, wrote outside the worktree, pushed to a remote, etc.
title: "[SAFETY] "
labels: ["bug", "safety-contract"]
assignees: ""
---

## ⚠️ Priority

Safety-contract violations are the highest-priority class of bug
this project tracks. The contract is the trust signal that lets
people run bosun on repos they care about.

**Maintainer commits to:** acknowledge within 48 hours, prioritize
investigation, and ship a fix or formal advisory before any
unrelated work.

If you'd prefer to report privately, see [`SECURITY.md`](../../SECURITY.md).

## Which guarantee broke?

The safety contract is in `README.md` under "Safety contract — what
bosun does to your repo." Pick the one(s) that fired:

- [ ] `main` advanced without an explicit `bosun merge`
- [ ] Bosun wrote outside `<repo>` or the `<repo>-bosun-*` worktrees
- [ ] Bosun pushed, fetched, or talked to a forge (GitHub, GitLab, …)
- [ ] Bosun modified global git config or `user.{name,email}`
- [ ] Bosun deleted a session's worktree/branch without `cleanup --purge`
- [ ] Other (describe below)

## What happened

<!-- Describe the violation. What did bosun do that it shouldn't have? -->

## Reproduction

<!-- Exact commands. If you can pin to one bosun command that did it, even better. -->

```sh
bosun ...
```

## Evidence

<!-- Anything that helps prove the violation:
     - `git reflog` showing main advancing
     - `find` output showing writes outside expected paths
     - Network capture showing forge talk
     - Screenshots
     Be careful not to paste any secret material. -->

## Environment

```sh
bosun --version
bosun doctor
```

OS: <!-- macOS / Linux / Windows version -->
