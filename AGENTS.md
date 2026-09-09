# Working in this fork

Fork-local. **Never propose this file upstream**, and never include it in an
`upstream-pr/*` branch — it describes how *we* work against upstream, which is
meaningless in `foks-proj/go-foks`.

## The one rule that keeps this fork sane

**`SECO-UPSTREAM.md` is the source of truth for anything upstream-facing, and
you must leave it accurate before you finish.**

Update it in the *same change* that makes it stale — not "later", not in a
follow-up commit. A row that lags reality is worse than no row, because the
next person (or session) trusts it and re-derives, re-proposes, or duplicates
work that was already decided.

Update it when you:

- open a PR upstream → add a `Proposed` row with the number and a one-line why;
- see one merged, closed, or superseded → move it to `Upstreamed`/`Declined`
  the same day, saying which PR actually carried it;
- decide something is never going upstream → add a `Local` or `Declined` row
  **with the reason**, so it stays decided;
- park something → `Queued`, and **name the real blocker**;
- merge `upstream/main` → re-check every `Proposed` row before doing anything
  else.

Before proposing anything: read it first, and confirm the change is not already
proposed, already merged, or already deliberately declined.

## Parallel sessions

More than one agent session works in this repo at once, often on different
slices of the same feature. Before creating a branch or opening a PR:

1. `git worktree list` and `git branch -a` — someone may already have a branch
   for it. If so, coordinate rather than starting a second copy.
2. `gh pr list --repo foks-proj/go-foks --author <you> --state all` — including
   **closed** PRs. A closed duplicate can hold review comments that were never
   moved to the surviving PR.
3. Check whether the work overlaps another in-flight area. Shared foundations
   (error classification, probe/loader plumbing) should be proposed **once**,
   as their own PR, with the others stacked on it — not vendored into each.

Never check out a branch another worktree holds. Use your own worktree under
your session's scratch directory, or a scratch branch, and leave the other
session's checkout as you found it.

## Proposing upstream

Process lives in `SECO-UPSTREAM.md` ("How to use it"). The parts that bite:

- Branch from `upstream/main`, never from our `main` — ours carries fork-local
  deploy config, and cutting from it drags that into the PR.
- **Verify the diff contains only your change.** Our `main` runs ahead of
  upstream with other unmerged work; a branch cut carelessly will include it.
  Check with `git diff --name-only upstream/main...HEAD` and read the list.
- `git rebase --signoff` — CONTRIBUTING.md requires a DCO trailer.
- Expect a supersede-PR if review finds follow-up work: our fork is org-owned,
  so maintainer pushes to our branch 403 even with `maintainerCanModify`. He
  reopens carrying our commits with authorship preserved. Not a rejection.

## Writing for upstream review

- Don't assert what you haven't verified. If a claim is load-bearing for a
  design decision, test it — the codec, the DB behaviour, the error path —
  and say what you tested. A confident wrong claim in a PR description costs
  more than an open question.
- Keep comments plain. Metaphor-heavy phrasing ("load-bearing", "hostage",
  "poisoned", "masquerade") reads as sloppy in review; say the mechanism.
- Reference only files that exist in that PR. A pointer to a doc that ships in
  a later PR is a dangling reference to the reviewer.
