# SECO fork ↔ upstream tracker

Every change this fork carries that upstream `foks-proj/go-foks` does not, and
what we decided to do about it. **One row per change, and every row has a
verdict.** A change with no row is a bug in this file, not a change with no
opinion.

Read this before proposing anything upstream. It exists so that nobody — human
or AI — has to re-derive a decision we already made, re-propose something
already proposed, or wonder why half a change went out and half did not.

Last synced against upstream: `d39371c`, merged into our `main` 2026-09-01.
`upstream/main` has since moved to `e174ca6` (checked 2026-09-21).

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
| channel mutation: rename, edit description, archive | [#376](https://github.com/foks-proj/go-foks/pull/376) | Opened 2026-09-22 from `upstream-pr/channel-mutation` (off `e174ca6`), as a proposed answer to our own [#351](https://github.com/foks-proj/go-foks/issues/351), still unanswered a week after it was filed — proposing the code rather than waiting longer, per the private-channel precedent. A channel's metadata was write-once: no RPC updated one, `channels.seqno` was stamped `1` at creation and never incremented, and nothing could be closed. `rtUpdateChannel` CAS-updates the boxes on `seqno`; `rtSetChannelArchived` is a **tombstone, not a purge** — an archived channel keeps every row, stays in the channel set so its name stays reserved, and leaves the inbox. Design in `docs/rt-channel-mutation.md`, which is fork-only and deliberately **not** in the PR (it cites `kv-channel-acl` and the `@200+` block, neither of which exists upstream). **Fork half merged as [#46](https://github.com/SECO-PBC/SECO-FOKS-server/pull/46) on 2026-09-22 and tagged `v0.1.9-seco.21`** for the app to pin. **Numbering actually shipped, and it differs on both sides — reconcile by CONTENT at the next upstream merge, never by number:** (1) RPCs — fork `@207/@208` (the reserved fork block), upstream PR **`@16/@17`**, leaving `@12–@15` free for our own open [#370](https://github.com/foks-proj/go-foks/pull/370) (push holds), which claims them against the same interface; they move down if #370 is declined. (2) `RTChannelMetadata.archived` — fork `@22`, upstream PR **`@14`**, because upstream's `noPush` merged at `@13` (#365) where ours sits at `@21`. (3) `clientRT*` — fork `@8/@9`, upstream PR **`@9/@10`**, because upstream's outbox work (#359) took `@5–@8`. (4) Schema patch — fork `p9`, upstream PR **`p8`**, leaving `p6`/`p7` to #370 for the same reason as the RPC slots and to avoid an add/add conflict on the same filename; the applier sorts the ids it has and tolerates the gap. The ids had already diverged at no-push (upstream p5 is no-push, fork p5 is `channel_acl`). (5) **A collision the fork already shipped, in `v0.1.9-seco.21`:** the fork's `RT_CHANNEL_ARCHIVED_ERROR @12008` is upstream's merged **`RT_MSG_QUEUED @12008`** (#359, in `e174ca6`), and upstream also holds `RT_OUTBOX_FULL @12009`. Our last upstream merge was `d39371c` (09-01), so the fork allocated into a range upstream had already used and nothing caught it. The upstream PR uses the free **`@12010`**. Harmless while the fork does not speak to an upstream server, and harmless in the tag, but the next upstream merge must renumber the fork's constant and re-run `make proto` — the merge will not conflict on it, because the two names sit on different lines. |
| kv-store: `kvGetNode` panics on an unloadable node ID | [#374](https://github.com/foks-proj/go-foks/pull/374) | Opened 2026-09-21 from `upstream-pr/kv-getnode-nil-panic` (off `e174ca6`). `loadNode` could return `(nil, nil)` and `KvGetNode` dereferences it, so the process died — **two** routes, both reachable by any authenticated caller with nothing but a node ID. (1) A missing small file or symlink: `mLoadSmallFilesOrSymlinks` appends a map lookup per key, so an absent row is a nil ENTRY, not a short slice, and `loadSmallFileOrSymlink` checked only the length. (2) A tombstone: `KVNodeType_None` is legal and `Type()` returns it without error, but `loadNode`'s switch had no arm and no default — **found by the /code-review pass on the first fix**, which would otherwise have shipped a 'don't panic on kvGetNode' PR that still panicked on kvGetNode. Fixed with a `default` so a future enum value fails loudly rather than panicking. Each guard mutation-checked at its own crash site. The fork carries both on `feat/kv-channel-acl` (route 1 inside the K2 commit, route 2 as its own); they deduplicate at the next upstream merge. |
| large-file read-role checks (kv-store) | [#371](https://github.com/foks-proj/go-foks/pull/371) | Opened 2026-09-21. `loadLargeFileMetadata` and `getChunk` were the only read paths in `server/kv-store` that never compared the caller's role to the node's; `getDir`, `listDir`, `loadDirent` and `mLoadSmallFilesOrSymlinks` all do. Not a data leak — the key box is sealed to the PTK at the file's read role — but existence, size and chunk layout went to any party member who knew the file ID, leaving the crypto as the only gate. Also reorders the check ahead of the status switch, which otherwise told a below-role caller whether a file was mid-upload, deleted or live. Four integration tests, each pinned by mutation; the chunk one issues `kvGetEncryptedChunk` directly because libkv loads metadata first and a client-API test pins nothing. Prerequisite for the fork-only `docs/kv-channel-acl.md`, but stands alone upstream. **The fork now carries this commit**: cherry-picked (`-x`, from `9ae9458`) onto `feat/kv-channel-acl` on 2026-09-21 so the ACL work is not blocked on review; when #371 merges and the next upstream merge lands, the cherry-pick deduplicates and this note comes off. |
| RT offline cold-start bootstrap | [#361](https://github.com/foks-proj/go-foks/pull/361) | Serves verified local snapshots from the user, team and probe loaders, so a client starts and works with no network. Carries `verifiedAt` on `UserSigchainState` and `TeamChainState`, the offline-reads design doc, and a distinct "cannot verify server identity" signal on a repeat certificate failure against a host this device has verified before. **Was stacked on [#359](https://github.com/foks-proj/go-foks/pull/359)**, which merged 2026-09-18, so its diff against `main` no longer carries the stack. Rebase onto current `upstream/main` before the next review pass. Also carries a one-line test-harness fix: the fabricated merkle root epno seed was 1000, which the suite had outgrown, and the extra users these tests add pushed the real tree past it. |
| libkv stale-cache retry fixes | [#368](https://github.com/foks-proj/go-foks/pull/368) | Two defects in `cacheRaceLoop`, opened 2026-09-15 after fork PR #35 merged. (1) `CacheAccess.clear()` dropped the party, so after any successful precondition-checked call a later `KVStaleCacheError` in the same loop cleared nothing, used up all 501 retries and surfaced "cached values were stale" (what `foks-gomobile/kv_helpers.go` `retryOnStaleCache` works around; remove it once this is in a pinned tag). (2) a cached tombstone answered `KVNoentError` with no cache check, so a file removed and written again by someone else stayed "not found"; the same commit clears the record on a server-side noent so missing-file reads don't pay an extra `KvCacheCheck`. Checked against [#367](https://github.com/foks-proj/go-foks/pull/367) (PartyLoaderCache deadlock): different files, no overlap. The fork-only commit from #35 (serve a cached noent offline) is **Local**: it patches the KV offline carve-out, which is not upstream. |
| libkv writes over a removed name | [#369](https://github.com/foks-proj/go-foks/pull/369) | Opened 2026-09-15 from `upstream-pr/kv-put-over-tombstone`; fork branch `fix/kv-put-over-tombstone` carries the same two patches. (1) After an unlink, put, `mkdir -p` and mv to that name failed with `no such file or directory` from any client without the tombstone cached: `lookupDirent` returned `KVNoentError` for a server-returned tombstone even for a put. `mvInner` also mishandled a tombstone destination: a directory could not move onto it, and a move onto a symlink whose removed target sits in another directory wrote a dirent under the wrong key. (2) `lookupDirent` never checked that the returned dirent carries the requested name MAC, so a server could substitute a sibling. `TestWriteOverTombstone` (one subtest per case) fails without (1) on `37f4cdd` and on our `main`. Write side of the flow whose read side is #368 just above; SECO-FOKS channel Leave needs both before it can remove its follow file. |
| push holds (delegated push release) | [#370](https://github.com/foks-proj/go-foks/pull/370) | Opened 2026-09-17 from `upstream-pr/push-holds` (off `26fff2d`); fork copy is PR #42, tagged `v0.1.9-seco.20`. A team admin makes one member the holder; sends into channels the holder can read write `held` push rows; the holder releases (keep / drop / newest per member) or queues `system` pushes with an opaque handle. SECO's daemon is the holder (thread-push: muted threads never push). **Differs from the fork in ways that matter at the next merge:** (1) patch ids: upstream p6 (`held` enum value) + p7 (`push_holds`), fork p7 + p8, since fork p5/p6 are private channels and no-push, so reconcile by CONTENT as with no-push; (2) RPC numbers: upstream @12..@15, fork @203..@206; (3) no private-channel vocabulary upstream: the fork's "holder has an ACL row" condition, the release on `RevokeChannelMember` and the `rt_inventory_test` entries stay **Local**; (4) upstream adds its own `activeTeamMembers` in `auth.go` and finds the holder by joining `push_holds` to `channels`. The fork's daemon-order contract test became a generic partial-release test. |
| background job jitter always a no-op | [#372](https://github.com/foks-proj/go-foks/pull/372) | Opened 2026-09-21 from `upstream-pr/bg-jitter` (off `e174ca6`). `BgTiming.applyJitter` divided a factor in `[-jitter, jitter-1]` by 100 in integer arithmetic, so it truncated to 0 and every client's `BgUserRefresh` (13 min) and `BgCLKR` (17 min) ran on the same deterministic schedule — the thundering herd the setting exists to prevent. Jitter was dead for any value below 101; at 101 or more the factor became -1/0/1, giving 0, base or 2*base; and `2*c.jitter` in `uint16` wraps to 0 at 32768, panicking `rand.Intn`. Fix computes the offset as a percentage of base, **capped at 100** — the uncapped multiply overflows int64 past ~39h of base, and a wrapped negative offset clamps to a 1ns sleep, which `BgCLKR.Reschedule()` turns into a busy loop (measured before the cap: 99926 of 200000 calls sub-ms at `base=48h, jitter=65535`). Flagged the cap in the PR body in case maxtaco would rather reject an out-of-range jitter at config load. Carries `client/libclient/config_test.go`, new — the package had no test for this function. **Pure upstream defect, nothing fork-local**; our copy is whatever the next merge brings, so no fork branch. |
| realtime presence (design only) | [#340](https://github.com/foks-proj/go-foks/pull/340) | Proposal doc for ephemeral presence/typing. No code — opened to get a design read before building, and still open. |

**One caveat on #359, now merged and therefore a next-merge concern**: it changes
`GetThreadRecentMsgs` and siblings to return `*ThreadReadResult` so a degraded
read can carry its stale flag. That is a source-compatible break for in-tree
callers, and the typed-message test from #341 is updated in the PR to match.

## Queued — decided yes, not yet opened

| Change | Waiting on | Notes |
|---|---|---|
| base-schema patch-record guard (test) | **nothing — ready to slice** | `server/sql/schema_patches_test.go` (fork PR #34). Upstream hit the identical miss on the no-push port (fixed one-line in `ed089bb` on `upstream-pr/no-push-channels`, no guard added), and the test reads only `SQL`/`Patches` plus `shared.SplitSQLStatements`, all of which exist upstream — `foks_users` (1–7) and `foks_server_config` (1–3) already pass. Branch it off `upstream/main` with the usual DCO signoff. **Slice only `TestBaseSchemaRecordsEveryPatch`.** The same file now also carries `TestDeployScriptPatchesEveryPatchedDB` (fork PR #48), which reads `scripts/deploy/server-deploy.sh` — fork-only infrastructure, `Local` row below. It would **compile** upstream — it uses only stdlib plus `shared.ParseDbType` and `sqlpkg.Patches` — and fail at run time, on the `os.ReadFile` of a script that does not exist there. It was added after `foks_kv_store/p1` shipped in `v0.1.9-seco.21` to a database the deploy script had never patched, because it was excluded from `DBS` back when it had no patches; the kv-store server SELECTs p1's `channel_id` on every read path, so the release deployed green and every KV read failed. |
| snapshot staleness surfacing | **the cold-start PR landing** | `lcl.TeamMembership`/`TeamRoster` carry the verification time outward and the CLI renders it (`Verified` column, `Snapshot verified` footer). Split from the field itself so the cold-start PR stays about the trust model rather than about presentation. Postdates `SECO-UPSTREAM-rt-offline.md`, which does not mention `verifiedAt` at all. |

**Snapshot staleness** names a real blocker: it is the fourth of one body of
work deliberately split into a landing order. Nothing here is merely parked.

(It read "that row" until 2026-09-23, from when this section held only that
one row. Name the row if a third is ever added between them.)

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
| KV channel-scoped storage (server + client) | Fork-only, and it always will be: it is built on `channel_acl`, which is the fork's private-channel ACL, and Max has said private channels are not on the upstream roadmap. `feat/kv-channel-acl` — p1 schema patch, `channelID` on three creation RPCs plus `kvChannelMkRoot @200`, the `authorizeKVNode*` chokepoint, creation tagging and containment, an inventory guard, and `lcl.KVConfig.channelID` through libkv. Design and decisions in `docs/kv-channel-acl.md`. Four defects found while building it went upstream on their own branches and are NOT fork-local: [#371](https://github.com/foks-proj/go-foks/pull/371) (large-file read checks, cherry-picked onto this branch — see the row above), [#374](https://github.com/foks-proj/go-foks/pull/374) (`kvGetNode` panicked the server on any unloadable node ID), [#372](https://github.com/foks-proj/go-foks/pull/372) (background-job jitter was always a no-op) and [#373](https://github.com/foks-proj/go-foks/pull/373) (CLKR loaded every team it could not act on). #372 has its own row above; #373's lands with `docs/upstream-clkr-skip`, which is not merged yet, so it is named here rather than cross-referenced. |
| `.github/workflows/deploy.yml`, `scripts/deploy/*` | Our Hetzner deploy. Means nothing upstream. |
| `.github/workflows/ci.yml` package scoping | Ours. `integration-tests/` is linted and run: the suite brings up postgres itself via testcontainers, so no `services:` block was needed -- only removing the exclusion. **Both integration jobs are advisory** (`continue-on-error`). `cli` always was, for signup/provisioning flakes upstream sees too. `lib` was a gate for one day (PR #46) and was demoted in #48: `TestLocks` overrides a held lock after 1ms and fails intermittently on a loaded runner, which blocked a PR that touched only a shell script and two docs. Re-promote `lib` once `TestLocks` is stable. Upstream runs plain `./...`. |

## Upstreamed — merged, ours only until the next merge

| Change | PR |
|---|---|
| RT offline mode (client) | [#359](https://github.com/foks-proj/go-foks/pull/359) — merged as ours 2026-09-18 as `e174ca6`, now `upstream/main` HEAD. Durable send outbox, degraded reads, offline read-marks, `rt outbox` CLI; also fixed the orphan row leaked on every successful send and the millisecond-keyed index that let two sends in the same millisecond overwrite each other. The `*ThreadReadResult` return on `GetThreadRecentMsgs` and siblings is now upstream; the caveat noted under the Proposed table is a next-merge concern for us, no longer a review one |
| no-push channels | [#365](https://github.com/foks-proj/go-foks/pull/365) — merged 2026-09-12. Upstream's port differs from our fork copy in ways that matter at the next merge: field `noPush @13` (ours is @21), no private-channel vocabulary, a new `MakeChannelOpts`, **and its own patch numbering for `foks_realtime`** — patch identity is a bare integer with no content hash (`server/shared/patch.go` keys on id alone), and our base schema now stamps ids 5 (private/ACL) and 6 (no_push) as applied, so upstream patch files taking those ids must be reconciled by CONTENT, renumbering whichever side's file diverges, or a fresh fork DB will skip upstream DDL silently. |
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
| KV small-file write idempotent on replay | our [#352](https://github.com/foks-proj/go-foks/pull/352) closed, superseded by [#358](https://github.com/foks-proj/go-foks/pull/358) — same shape as #353/#357: our commit carried, plus a bool reporting the replay to the caller and a `FOR UPDATE` anchoring the comparison against a concurrent GC |
| RT offline mode — duplicate | our [#360](https://github.com/foks-proj/go-foks/pull/360) closed as a duplicate of our own [#359](https://github.com/foks-proj/go-foks/pull/359), branch deleted. Both carried the same outbox work; #360 was opened without checking that #359 already existed, and was additionally incomplete (it excluded `tables.go`, so the offline notice never rendered). The cold-start PR was re-stacked onto #359. Second such duplicate after #348/#345 — check the open PR list before opening |
| social signup protocol + schema | [#339](https://github.com/foks-proj/go-foks/pull/339) — merged as ours; design doc only |
| RT typed (pegged) messages | [#341](https://github.com/foks-proj/go-foks/pull/341) — merged as ours |
| RT push fan-out + device tokens | [#342](https://github.com/foks-proj/go-foks/pull/342) — merged as ours, after a review round that typed the wire API: `platform` became the `RTPushPlatform` enum, `token` a typedef, `deviceKey` an `EntityID` the server validates with `ToDeviceID` |
| team invite accept after rekey | [#350](https://github.com/foks-proj/go-foks/pull/350) — merged as ours |
| roster by member names | our [#326](https://github.com/foks-proj/go-foks/pull/326) closed, superseded by [#332](https://github.com/foks-proj/go-foks/pull/332) — our commit carried unchanged, plus his fix for a cross-host hostname bug found in review; [#333](https://github.com/foks-proj/go-foks/pull/333) then replaced the member-load flag pair with an ordered `MemberLoadLevel` |
