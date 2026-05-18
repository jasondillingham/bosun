<!--
Thanks for the PR. A short description above the checklist is
ideal — what changed and why. The checklist is the hard part;
the description is for the reviewer's benefit.
-->

## Summary

<!-- 1-3 sentences. What does this change do? Link issues with #N. -->

## Checklist

- [ ] `make check` passes locally (vet + race tests + demo dry-run)
- [ ] Added or updated tests for any behavior change
- [ ] Updated user-facing docs (`README.md`, `docs/`) if the change is user-visible
- [ ] No `--no-verify` / `--no-gpg-sign` / skipped hooks
- [ ] If touching the safety contract (anything in `README.md`'s
      "Safety contract" section), the README update is in this PR

## Scope

`SPEC.md` is authoritative for v0.1 scope. `CLAUDE.md` documents
the conventions agents (and humans) should follow. If this PR
goes beyond either, please flag it.

## Notes for the reviewer

<!-- Anything the reviewer should know — design tradeoffs, things you considered
     and rejected, manual testing you did, future follow-ups you'd queue. -->
