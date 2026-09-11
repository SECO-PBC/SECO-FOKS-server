# SECO fork ↔ upstream tracker

Every change this fork carries that upstream `foks-proj/go-foks` does not, and
what we decided to do about it. **One row per change, and every row has a
verdict.** A change with no row is a bug in this file, not a change with no
opinion.

Read this before proposing anything upstream. It exists so that nobody — human
or AI — has to re-derive a decision we already made, re-propose something
already proposed, or wonder why half a change went out and half did not.

Last synced against upstream: `d39371c`, merged into our `main` 2026-09-01.
`upstream/main` has since moved to `fd03098`.

Working conventions — including the rule that this file must be left accurate
by the change that makes it stale — are in `AGENTS.md`.

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
| RT offline mode (client) | [#359](https://github.com/foks-proj/go-foks/pull/359) | Durable send outbox, degraded reads, offline read-marks, `rt outbox` CLI. Opened 2026-09-09 once both its dependencies landed: [#349](https://github.com/foks-proj/go-foks/pull/349) (replay semantics its drain converges on) and [#347](https://github.com/foks-proj/go-foks/pull/347) (the classifier it uses to tell an outage from a refusal). Also fixes two live defects in the existing outbox stub: an orphan row leaked on every successful send, and a channel-keyed millisecond index that let two sends in the same millisecond overwrite each other. |
| RT offline cold-start bootstrap | [#361](https://github.com/foks-proj/go-foks/pull/361) | Serves verified local snapshots from the user, team and probe loaders, so a client starts and works with no network. Carries `verifiedAt` on `UserSigchainState` and `TeamChainState`, the offline-reads design doc, and a distinct "cannot verify server identity" signal on a repeat certificate failure against a host this device has verified before. **Stacked on [#359](https://github.com/foks-proj/go-foks/pull/359)** — opened against `main`, so its diff reads as the whole stack until #359 lands. Also carries a one-line test-harness fix: the fabricated merkle root epno seed was 1000, which the suite had outgrown, and the extra users these tests add pushed the real tree past it. |
| realtime presence (design only) | [#340](https://github.com/foks-proj/go-foks/pull/340) | Proposal doc for ephemeral presence/typing. No code — opened to get a design read before building, and still open. |
| KV small-file write idempotent on replay | [#352](https://github.com/foks-proj/go-foks/pull/352) | A retry after a lost ack hit the primary key and read as a raw pg error, so `kvPutSmallFileOrSymlink` could not be retried at all. Node IDs are client-chosen and fix the encryption nonce, so an honest retry is byte-identical and is now a no-op; a same-ID/different-bytes write gets `KV_RACE_ERROR`. Same reasoning as the merged #349. |

**One caveat on #359 worth watching in review**: it changes
`GetThreadRecentMsgs` and siblings to return `*ThreadReadResult` so a degraded
read can carry its stale flag. That is a source-compatible break for in-tree
callers, and the typed-message test from #341 is updated in the PR to match.

## Queued — decided yes, not yet opened

| Change | Waiting on | Notes |
|---|---|---|
| snapshot staleness surfacing | **the cold-start PR landing** | `lcl.TeamMembership`/`TeamRoster` carry the verification time outward and the CLI renders it (`Verified` column, `Snapshot verified` footer). Split from the field itself so the cold-start PR stays about the trust model rather than about presentation. Postdates `SECO-UPSTREAM-rt-offline.md`, which does not mention `verifiedAt` at all. |
| no-push channels | **a staging run proving it, and upstream's RT work settling** | Fork PR #30. A creation-time per-channel flag that drops the channel from the `push_outbox` fan-out on send; inbox-version wakes are untouched. Generic and small, so it is a reasonable upstream candidate — but the mechanism is only worth proposing once we have run it: the value claim is "control traffic stops waking phones", and we have not yet measured that end to end. Upstream is also mid-rebuild of realtime (`chat(1d)` series), and `RTChannelMetadata` is exactly the struct in motion, so expect `noPush @21` to be renumbered on the way in. Our own use is the member-DM first-contact handshake (`dm-handshake-over-rt` in SECO-FOKS), which needs a control channel that never buzzes a community. |

That row names a real blocker: it is the fourth of one body of work deliberately
split into a landing order. Nothing here is merely parked.

### Not going upstream (for now)

**KV offline mode (client half).** The server half is #352 above. The client
half stays local; `docs/offline-kv` is an *integration* branch carrying RT +
sigchain + KV together, not a KV-only slice. If any part is ever proposed,
split it against `upstream/main` rather than off that branch.

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
| RT `rtSend` idempotent on `msg_id` | our [#345](https://github.com/foks-proj/go-foks/pull/345) closed, superseded by [#349](https://github.com/foks-proj/go-foks/pull/349) — our commit carried, plus maxtaco's `wasReplay` field on the response. `messages_enc` already had `UNIQUE(msg_id)`; the conflict had surfaced as a raw pg error no client could read |
| transport-failure classification | [#347](https://github.com/foks-proj/go-foks/pull/347) — merged as ours, 2026-09-09. `core.IsTransportError` plus the four call sites that meant "transport" and only matched `ConnectError`. Carved out of the RT offline PR, where it was originally scoped; standing alone as a defect fix made both smaller |
| parallel explore waves | our [#346](https://github.com/foks-proj/go-foks/pull/346) closed, superseded by [#354](https://github.com/foks-proj/go-foks/pull/354) — our commit carried as the base with authorship preserved, then rebuilt on it as a concurrent worker pool rather than discrete waves |
| KV directory creation idempotent on replay | our [#353](https://github.com/foks-proj/go-foks/pull/353) closed, superseded by [#357](https://github.com/foks-proj/go-foks/pull/357) — our commits carried with authorship preserved, plus reporting the replay back to the caller |
| RT offline mode — duplicate | our [#360](https://github.com/foks-proj/go-foks/pull/360) closed as a duplicate of our own [#359](https://github.com/foks-proj/go-foks/pull/359), branch deleted. Both carried the same outbox work; #360 was opened without checking that #359 already existed, and was additionally incomplete (it excluded `tables.go`, so the offline notice never rendered). The cold-start PR was re-stacked onto #359. Second such duplicate after #348/#345 — check the open PR list before opening |
| social signup protocol + schema | [#339](https://github.com/foks-proj/go-foks/pull/339) — merged as ours; design doc only |
| RT typed (pegged) messages | [#341](https://github.com/foks-proj/go-foks/pull/341) — merged as ours |
| RT push fan-out + device tokens | [#342](https://github.com/foks-proj/go-foks/pull/342) — merged as ours, after a review round that typed the wire API: `platform` became the `RTPushPlatform` enum, `token` a typedef, `deviceKey` an `EntityID` the server validates with `ToDeviceID` |
| team invite accept after rekey | [#350](https://github.com/foks-proj/go-foks/pull/350) — merged as ours |
| roster by member names | our [#326](https://github.com/foks-proj/go-foks/pull/326) closed, superseded by [#332](https://github.com/foks-proj/go-foks/pull/332) — our commit carried unchanged, plus his fix for a cross-host hostname bug found in review; [#333](https://github.com/foks-proj/go-foks/pull/333) then replaced the member-load flag pair with an ordered `MemberLoadLevel` |
