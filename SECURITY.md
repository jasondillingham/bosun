# Security policy

## The safety contract is the load-bearing trust signal

Bosun's [safety contract](./README.md#safety-contract--what-bosun-does-to-your-repo)
is what makes it safe to run on a repo you care about. The contract
promises, among other things:

- `main` never advances except through `bosun merge`.
- Bosun writes only inside `<repo>` and the `<repo>-bosun-*` sibling
  worktrees — never outside.
- Bosun never pushes, fetches, or talks to a forge.

Anything that violates these guarantees is the highest-priority class
of bug this project tracks. Please report it.

## Reporting a vulnerability

**Preferred: private email.**
Send the report to **jasonmdillingham@gmail.com**.

**Acceptable: GitHub Security Advisory.**
Open a private advisory at
https://github.com/jasondillingham/bosun/security/advisories/new.

Either path goes to the maintainer. Use email if you want
acknowledgement faster than GitHub's notification cadence.

Please include:

- The version of bosun (`bosun --version`).
- Your OS and Git version (`bosun doctor` output captures both).
- The minimal sequence of commands that reproduces the issue.
- What you expected to happen and what actually happened.
- If the issue could affect anyone else's repo, indicate that —
  it changes the disclosure timeline.

We aim to acknowledge reports within a few days. If you haven't heard
back in a week, please send a follow-up — email occasionally goes to
spam.

## What qualifies as a safety issue

Anything that breaks the safety contract:

- Bosun advances `main` (or any other branch you didn't explicitly
  ask it to merge into) outside `bosun merge`.
- Bosun writes a file outside `<repo>` and the `<repo>-bosun-*`
  sibling worktrees.
- Bosun pushes to a remote, fetches from one, or contacts a forge
  (GitHub, GitLab, etc.) without you asking.
- Bosun modifies your global git config, your `user.{name,email}`,
  or repo-level git config beyond what `git worktree add` does.
- Bosun discards committed work that wasn't explicitly targeted by
  `bosun cleanup --purge` or `bosun remove --force`.
- Path-traversal or command-injection through any user-supplied
  input (session labels, brief paths, plan content).
- A worktree's claim or DONE state can be forged from outside the
  session it represents in a way that causes data loss at merge.

Code that allows an attacker who can write to your filesystem to
escalate via bosun (e.g. by planting a malicious brief or plan file)
also qualifies — bosun should fail safely on hostile input.

## What does not qualify

Bug reports about these are welcome via the public issue tracker,
not the security path:

- Style preferences, naming choices, or output-formatting nits.
- Feature requests (use `.github/ISSUE_TEMPLATE/feature_request.md`).
- Crashes that don't violate the contract (lose your session, sure;
  but don't corrupt `main` or leak outside the worktree).
- Performance issues.
- Documentation errors that don't misrepresent the safety contract.

If you're unsure which bucket your finding falls into, err toward
private email — we'd rather triage a non-issue privately than have
a real safety violation discussed in a public issue first.

## Supported versions

Bosun is pre-1.0. Security fixes target the current minor release
(currently the v0.11 line). Older versions are not patched; the fix
will be available by upgrading.

## Disclosure

After a fix lands, we'll add a note to `RELEASES.md` describing the
issue and crediting the reporter (unless you ask us not to). For
issues that affected other users' repos, we'll publish a security
advisory with the upgrade path.
