# SECO fork ↔ upstream tracker

Every change this fork carries that upstream `foks-proj/go-foks` does not, and
what we decided to do about it. **One row per change, and every row has a
verdict.** A change with no row is a bug in this file, not a change with no
opinion.

Read this before proposing anything upstream. It exists so that nobody — human
or AI — has to re-derive a decision we already made, re-propose something
already proposed, or wonder why half a change went out and half did not.

Last synced against upstream: **v0.1.9** (`cb2d8ac`), 2026-08-25.
`upstream/main` has since moved to `d39371c` (11 commits, 2026-09-01) — every
one of our then-open proposals among them. We have **not** merged it yet, so we
still carry local copies of everything under Upstreamed until we do.

## How to use it

- **Adding a fork change?** Add a row, even if the verdict is "never upstream".
- **Proposing upstream?** Branch `upstream-pr/<topic>` off `upstream/main` (not
  our `main`), push to SECO-PBC, then
  `gh pr create --repo foks-proj/go-foks --head SECO-PBC:<branch>`.
- **Sign off first.** `git rebase --signoff`. CONTRIBUTING.md requires a DCO
  trailer and none of our fork-authored commits carry one by default.
- **Expect a supersede-PR if review turns up follow-up work.** Our fork is
  org-owned, so maintainer pushes to our PR branch 403 even with
  `maintainerCanModify` set. When maxtaco wants a fix or a test on top he
  cannot push to us: he opens a new PR carrying our commits with authorship
  preserved plus his work, and closes ours. That is what happened to #323,
  #324 and #326 — not a rejection, and the credit survives. If we would rather
  have fixes pushed onto our own branch, propose from a personally-owned fork
  instead of SECO-PBC.
- **PR closed or merged?** Update the row the same day. A stale row is worse
  than no row, because it gets trusted.
- **After each upstream merge?** Re-check every `Proposed` row: merged ones
  become `Upstreamed` and drop out of our diff.

The sorting rule: **propose it if it fixes a defect or inefficiency any FOKS
user hits; keep it local if it encodes how SECO deploys or what SECO builds.**

## Status vocabulary

| Status | Meaning |
|---|---|
| `Upstreamed` | Merged upstream. Ours only until the next merge drops it. |
| `Proposed` | PR open. Link it. |
| `Queued` | Decided yes, not yet opened. **Must name what it waits on.** |
| `Local` | Deliberately never upstream. **Must say why.** |
| `Declined` | Considered and rejected. **Must say why**, so it stays rejected. |

---

## Proposed — open upstream

| Change | PR | Notes |
|---|---|---|
| RT `rtSend` idempotent on `msg_id` | [#345](https://github.com/foks-proj/go-foks/pull/345) | `messages_enc` already had `UNIQUE(msg_id)`; a conflict surfaced as a raw pg error no client could read. First of the three-PR RT offline series — see Queued and `SECO-UPSTREAM-rt-offline.md`. |

The previous eight all resolved on 2026-09-01: five merged as themselves, three
superseded by maxtaco's own PRs carrying our commits (see Upstreamed, and the
supersede-PR note in "How to use it").

## Queued — decided yes, not yet opened

| Change | Waiting on | Notes |
|---|---|---|
| parallel explore waves | **nothing — unblocked 2026-09-01, open it** | 4533→1367ms. Its blocker (#325) merged. Per this file's own rule it should be opened rather than parked; it is listed here only until someone cuts the branch. |
| RT offline mode (client) | **[#345](https://github.com/foks-proj/go-foks/pull/345) landing** | Durable outbox, degraded reads, offline read-marks, `rt outbox` CLI. Its drain relies on the replay semantics the first PR defines; opening both at once would review one against unmerged behaviour. |
| RT offline cold-start bootstrap | **the offline-mode PR landing (which waits on [#345](https://github.com/foks-proj/go-foks/pull/345)), and maxtaco's read on the loader altitude** | Serves verified local snapshots from user/team/probe loaders. Touches shared loaders, so it follows rather than leads. Two trust-model questions already answered by maxtaco (cached PTKs offline; view token not needed offline) — recorded in `SECO-UPSTREAM-rt-offline.md`. |

Parallel explore waves is ready to open right now — parked only until someone
cuts the branch. The two RT rows name real blockers: with #345 open, they are
the second and third of one body of work deliberately split into a landing
order, and the proposal text for all three lives in
`SECO-UPSTREAM-rt-offline.md`.

## Declined — considered, rejected, stays rejected

### RpcStats scope reporter
Was upstream [#318](https://github.com/foks-proj/go-foks/pull/318); we closed it
ourselves. **Keep it local.**

The counting is general — one context-scoped counter at `RpcClient.Call2`, the
funnel every generated client passes through, nil-safe and ~free when nothing
collects. But the *reporter hook* exists for a mobile-only reason: go-foks logs
inside the iOS app container where no device capture reaches, and the gomobile
bridge cannot open a scope itself because `RpcStats` travels by context value
and the bridge talks to the agent over loopback RPC. A CLI or server operator
has zap and needs none of this.

**Thread closed 2026-09-01.** #325 merged without its round-trip budget test,
and no comment or review on it asked for the hook. The standing condition —
"if maxtaco asks, that is a request, not a re-proposal" — is unmet, so the
decision holds unchanged. Do not re-raise it unprompted.

### `TeamLeaveSelf`
**Local pending [#331](https://github.com/foks-proj/go-foks/issues/331)**, the design
question we raised upstream rather than proposing an API for. Revisit when it
gets an answer; do not propose before then.

It writes only the member's own membership chain. It does *not* update
`team_members` or rotate PTKs — that needs an admin `EditTeam` with role `NONE`.
So a member who "leaves" still holds the current PTK and can still read team
data written afterwards, and still appears in everyone's roster. Our app is safe
because it pairs the call with a signal to the owner; as general public API the
name invites a security-relevant misreading.

Worse since upstream #319: **a sole owner** can self-attest leaving but can never
be removed, because removing the last owner is now refused server-side. The team
is stuck with an owner who believes they left.

### Upstream release tooling (`make/server.mk`, `release.md`)
cubic flagged a hardcoded `maxtaco` GHCR username and inconsistent `cd ../pkgs`
paths on our merge PR #21. Both are upstream's own release process; our deploy
does not use `server.mk` at all. **Not ours to patch.**

### `HasMemberRole` / `MakeChange` host-guard mismatch
cubic finding on #21. **Not a defect.** The premise is that `TeamAdmit` builds
rows whose `Id.Host` is non-nil and equal to the home host — the shape
`MakeChange` rejects. But `AtHost(h)` sets `Host = nil` exactly when the host
equals `h`, so that shape cannot arise. If it could, every admit would fail.

### `team_admin.go` "edit" vs "change" wording
cubic finding on #21. Real but cosmetic — same error type, one word apart, in a
subsystem unrelated to anything we are proposing. Not worth the review cost.

## Local — deliberately ours forever

| Change | Why |
|---|---|
| `.github/workflows/deploy.yml`, `scripts/deploy/*` | Our Hetzner deploy. Means nothing upstream. |
| `.github/workflows/ci.yml` `GO_PKGS` scoping | We exclude `integration-tests/` because this workflow provisions no postgres. Upstream runs plain `./...`. |

## Upstreamed — merged, ours only until the next merge

| Change | PR |
|---|---|
| lockstate propagation | [#287](https://github.com/foks-proj/go-foks/pull/287) — upstream tightened it to run only after `UnlockKeys` succeeds; our unguarded copy is deleted |
| iOS `sharedHome` nil guard | [#289](https://github.com/foks-proj/go-foks/pull/289) |
| libclient public Config setters | [#290](https://github.com/foks-proj/go-foks/pull/290) |
| rejoin partial unique index | [#316](https://github.com/foks-proj/go-foks/pull/316) — our #288 was closed as superseded; upstream took our text verbatim as `p7.sql` |
| explore double-load | [#325](https://github.com/foks-proj/go-foks/pull/325) — merged as ours. Shipped without its round-trip budget test; see RpcStats above |
| `--vhost` strict lookup | [#327](https://github.com/foks-proj/go-foks/pull/327) — merged as ours |
| `patch-db --yes` | [#328](https://github.com/foks-proj/go-foks/pull/328) — merged as ours |
| `Config.RPCLogOptions` via env/config | [#329](https://github.com/foks-proj/go-foks/pull/329) — merged as ours |
| `TeamCancelRequest` + `TeamReject` | [#330](https://github.com/foks-proj/go-foks/pull/330) — merged as ours; maxtaco added the cancel-reject-reaccept test arc in [#338](https://github.com/foks-proj/go-foks/pull/338). Still excludes `TeamLeaveSelf` — [#331](https://github.com/foks-proj/go-foks/issues/331) is **open** |
| merkle loop DB resilience | our [#323](https://github.com/foks-proj/go-foks/pull/323) closed, superseded by [#344](https://github.com/foks-proj/go-foks/pull/344) — our commit carried unchanged with authorship preserved, plus his fix for a cancellation hazard review found |
| libkv stale VO bearer token re-mint | our [#324](https://github.com/foks-proj/go-foks/pull/324) closed, superseded by [#343](https://github.com/foks-proj/go-foks/pull/343) — our two commits carried unchanged, plus a typed-error refactor (`TEAM_VO_BEARER_TOKEN_NOT_FOUND_ERROR`) and e2e regression coverage |
| roster by member names | our [#326](https://github.com/foks-proj/go-foks/pull/326) closed, superseded by [#332](https://github.com/foks-proj/go-foks/pull/332) — our commit carried unchanged, plus his fix for a cross-host hostname bug found in review; [#333](https://github.com/foks-proj/go-foks/pull/333) then replaced the member-load flag pair with an ordered `MemberLoadLevel` |
