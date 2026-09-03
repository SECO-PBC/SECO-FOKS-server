# Private channels via server-side ACL (fork-only)

**Status:** DRAFT spec, 2026-08-28. Server side first (SECO-FOKS-server fork),
client side in a follow-up spec.
**Scope:** fork-only. Not proposed upstream (Max: "not on the roadmap").
**Base:** fork `main` @ `0c076a0` (PR #22 typed sends + push outbox/token,
PR #24 upstream-aligned naming — no wire change). Untagged as of 2026-08-28;
the app pins `v0.1.9-seco.11`, latest tag is `seco.13` (= PR #22 head).
The typed-message and push-token work is also proposed upstream as
foks-proj/go-foks#341 and #342 — everything here must stay mergeable with
those (append-only enum value, new RPC numbers after @11, no renames).
**Decision record:** option (1) "server-enforced access control, no new teams
or keys" chosen 2026-08-28 over (2) Keybase-style subteams (Max's protocol
work, not ours) and (3) one FOKS team per private channel (works — it is how
our DMs already work — but leaves lifecycle to the app and leaks the
channel↔community association through app metadata).

---

## 1. What this gives, and what it does not

Every channel in a team is encrypted with the **team's** key at the channel's
read role — `sealMessage` derives the message key from
`PrivateKeysForRole(readRole)` and an app/type derivation that does not include
the channel (`client/librt/minder.go:785`, `client/librt/crypto.go:29`). A
"private" channel therefore shares its key with every team member at that role.

**Guarantee statement (put this in the product copy, not just here):**

- Against *outsiders* (not in the community): unchanged, cryptographic. A leaked
  database yields ciphertext they cannot read.
- Against *community members who are not channel members*: the server refuses
  to serve them channel data. **Community leaders are excepted by design** —
  they can see a private channel exists and may join it visibly (§7). This is
  stated in the product copy, not buried here. This is policy, not mathematics — the same level
  a Facebook group offers against a Facebook user who is not in it.
- A bug in the policy exposes the channel's **entire history** to any current or
  former member who kept their keys, silently and retroactively. There is no
  rekey on channel-membership change (team keys rotate only on *team*
  membership change), so a **revoked channel member can also decrypt future
  messages if they ever obtain the ciphertext**. The policy is the whole defence,
  forever. Hence §5 (one chokepoint) and §8 (a test per path, plus a guard that
  fails when a path is added without being classified).
- The SECO daemon is a key-holding team member; SECO infrastructure therefore
  holds both halves (key + ciphertext) for every private channel in a community
  the daemon is in. Whether the daemon is granted membership of private channels
  is a product decision (§7, Q3) — it is not implicitly a reader, the ACL
  applies to it like anyone else.

Non-goals: per-channel keys, rekey on revoke, hiding a private channel's
*existence* from team admins (see §6), any client UI (follow-up spec).

## 2. Existing building blocks (what the fork already has)

Everything below is in the fork today; the design in §4 extends it rather than
adding a parallel mechanism.

| Mechanism | Where | What it does today |
|---|---|---|
| **Channel tier** `bottom` / `admin` | `server/sql/foks_realtime.sql:52`, `proto-src/lib/realtime.snowp:140` (`RTChannelTier`) | Visibility floor for a channel's *existence and name*. Listing selects `tier = ANY(tiers)` where `tiers` = `{bottom}` (+ `admin` for admins) — `server/realtime/channels.go:116`. Changed-threads applies the same rule — `inbox.go:~200`. |
| **Read/write roles** | `channels` table `read_role_*`, `write_role_*` | Crypto floor + read gate. `getThreadGeneric` loads the read role and requires `AuthorizeUserForTeam(team) ≥ readRole` — `messages.go:1023`. Sender auth requires `≥ writeRole` — `messages.go:111`. |
| **`user_channels`** (one row per user × channel) | `foks_realtime.sql`, design doc §user_channels | Delivery membership + per-user inbox state (`inbox_version`, `read_through`, `hidden`, `muted`). Written by the channel-creation fan-out (`channels.go:fanoutToUser`) and the late-join fan-in. |
| **Fan-out on write** | `messages.go:fanoutInboxVersions` | A send bumps every `user_channels` row of the channel: inbox version, `push_outbox` row (sender excluded), long-poll wake. **Membership = the set of `user_channels` rows.** |
| **Late-join fan-in** | `fanin.go:reconcileUserChannels` (issue #301) | On `user_membership_vers` change, adds `user_channels` rows for channels of the user's teams they lack, if role ≥ read role (`fanin.go:267-311`). **Would add non-members to a private channel unless excluded.** |
| **Lingering rows** | `inbox.go:169` | `user_channels` rows are NOT deleted on team leave. Changed-threads re-authorizes per team and skips rows of teams the user left. Fan-out does not — see §6.3. |
| **`readThrough`** | `messages.go:547-566` | `UPDATE user_channels … WHERE uid=$3`; no row → no effect. Naturally gated by membership. |
| **`channel_parties`** | `messages.go:126`, `channels.go:201` | Sender attribution / last-sender preview; not an ACL. |
| **Schema patches** | `server/sql/patches/foks_realtime/pN.sql`, `server/shared/patch.go` | Numbered SQL patches, applied once, recorded in `schema_patches`. |

## 3. Threat model in one table

| Actor | Has key? | Gets ciphertext? | Reads plaintext? |
|---|---|---|---|
| Outsider (not in team) | no | only via DB breach | **no** |
| Team member, not channel member ("Keith") | **yes** | only via an ACL gap | only if an ACL gap exists |
| Revoked channel member | yes, forever (no rekey) | only via an ACL gap | only if an ACL gap exists — including *future* messages |
| Ex-team-member | yes, up to the team key gen at removal | only via an ACL gap (§6.3) | history up to removal |
| Team admin/owner, not channel member | yes | only via ACL gap | no — unless they grant themselves (visible, §6.2) |
| Server operator (SECO) with the daemon in the team | yes (daemon) + ciphertext | yes | **yes** — by construction of option (1) |

## 4. Design

### 4.1 Privacy is an orthogonal flag, not a third tier

**Design forced by a verified constraint.** Schema patches run inside a
transaction (`server/shared/patch.go:206-260`: `conn.Begin` → `tx.Exec(stmnt)`
→ commit). Postgres (17.9 here) permits `ALTER TYPE ... ADD VALUE` inside a
transaction but **forbids using the new value in that same transaction** — so
an enum-extending patch is fragile the moment any statement in it, or a
same-run later patch, references `'private'`.

That constraint pushed the design somewhere better anyway. Privacy is not a
third point on the tier axis; it is a **second, orthogonal axis**:

| Axis | Values | Means |
|---|---|---|
| `tier` (unchanged) | `bottom` / `admin` | which role floor may know the channel exists; which key seals its name |
| `private` (new) | bool | additionally requires an explicit `channel_acl` row |

Consequences, all good: the PG enum is untouched (less divergence from
upstream, cleaner merges with foks-proj/go-foks#341/#342); existing rows are
correct with `DEFAULT false`; the name-key role rule in `minder.go:255-290`
needs no special case (a private channel is an ordinary bottom-tier channel
that also demands an ACL row); and an *admin-tier private* channel — admins
only, further narrowed to a named subset — becomes expressible for free.

### 4.2 Authoritative membership: `channel_acl`

Do **not** reuse `user_channels` as the ACL. It is denormalized delivery state
that lingers after team leave, is self-healed by the fan-in, and doubles as
per-user prefs. Conflating "may read" with "is being delivered to" is exactly
the kind of ambiguity that produces the silent bug this document exists to
prevent.

```sql
-- server/sql/patches/foks_realtime/p2.sql
-- No enum change: see 4.1. Safe inside the patch runner's transaction.
ALTER TABLE channels ADD COLUMN private BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE channel_acl (
    short_host_id SMALLINT NOT NULL,
    channel_id    BIGINT   NOT NULL,
    uid           BYTEA    NOT NULL,
    acl_role      SMALLINT NOT NULL,   -- 0 = member, 1 = owner (may grant/revoke)
    granted_by    BYTEA    NOT NULL,   -- uid; audit trail
    ctime         TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (short_host_id, channel_id, uid),
    FOREIGN KEY (short_host_id, channel_id) REFERENCES channels(short_host_id, channel_id)
);
CREATE INDEX channel_acl_uid_idx ON channel_acl(short_host_id, uid);
```

**Invariant (tested):** for every channel with `private = true`, the set of
`user_channels` rows equals the set of `channel_acl` rows. Grant = insert ACL
row + `fanUserIntoChannel` (existing helper: bumps the user's inbox so the
channel appears on their next sync). Revoke = delete ACL row + delete
`user_channels` row + bump the user's `user_inbox` version so their client drops
the channel. Both in one transaction.

### 4.3 One chokepoint

```go
// authorizeChannel replaces the loadChannelForRead + AuthorizeUserForTeam pair.
// EVERY path that returns channel rows, message rows, or channel activity
// metadata calls it. No path may query `channels` or `messages_enc` for a
// caller without going through here.
func authorizeChannel(m, rtdb, userdb, channelID, want accessKind) (teamRole *core.RoleKey, err error)
//   1. load channel (team, tier, read/write roles)          -- RowNotFound if absent
//   2. role := AuthorizeUserForTeam(team)                    -- outer gate: must be a current team member
//   3. tier gate: admin tier requires IsAdminOrAbove
//   4. role gate: read requires role >= readRole; write requires role >= writeRole
//   5. if channel.private: require channel_acl row for (channel, m.UID())
//      -- for want == manage: require acl_role == owner OR role.IsAdminOrAbove()
//      -- for want == create (private): require role.IsAdminOrAbove()  [Q2b]
//   6. errors: a private channel the caller is not in returns the SAME error
//      as a non-existent channel (RowNotFound) -- existence is not disclosed
```

Set-based paths (listing, changed-threads) cannot call a per-row function; they
embed the equivalent predicate — and the test in §8.3 asserts both stay in
lock-step by running the same scenarios through each.

### 4.4 Wire changes (`proto-src/rem/realtime.snowp`, then `go generate ./proto/rem/`)

```
rtChannelGrant   @200 (channelID : lib.RTChannelID, uid : lib.UID, owner : Bool);
rtChannelRevoke  @201 (channelID : lib.RTChannelID, uid : lib.UID);
rtChannelMembers @202 (channelID : lib.RTChannelID) -> List(RTChannelAclEntry);
   RTChannelAclEntry { uid, owner : Bool, grantedBy : lib.UID, ctime }
```

Upstream RPCs currently end at `@11` (`rtSetPushToken`); `@200+` is reserved
here for fork-only methods. If any of this is ever proposed upstream, the
numbers get renegotiated at that point — a rename/renumber is a mechanical
change, a collision in the field is not.

`rtNewChannel @0` with `md.private == true` creates the channel with the
creator as ACL owner and fans out ONLY to the creator (not the team). Members
are added via `rtChannelGrant`. (Alternative considered: an initial members
list on create — rejected for v1; two RPCs beat a new arg struct, and
"create then invite" mirrors the UI anyway.)

`RTChannelMetadata` (`proto-src/rem/realtime.snowp:62-74`) gains
`private @64 : Bool`, alongside the existing `tier @11` / `unreadable @12`.

### 4.5 librt (`client/librt/minder.go`)

- `MakeChannel`: accept a `private` bool. Tier/name-key selection is
  **unchanged** (4.1) — a private channel is an ordinary bottom-tier channel.
  Skip the team-wide name-collision map when `private` (`minder.go:243-289`):
  the server cannot dedupe names it hides, and two private channels sharing a
  name is legitimate.
- New `Grant`, `Revoke`, `Members` wrappers.

## 5. Path inventory — the actual deliverable

Every server path that can return channel rows, message rows, or channel
activity metadata. "Gate today" is what protects a bottom/admin channel;
"Private gate" is the addition. The rightmost column is the integration test
that must exist (§8).

| # | Path | Returns | Gate today | Private gate | Test |
|---|---|---|---|---|---|
| 1 | `rtGetThread @4` → `getThreadGeneric` (`messages.go:1005`) | messages | team role ≥ readRole | + ACL row; non-member gets RowNotFound | `TestPrivateThreadDeniedToNonMember` |
| 2 | `rtGetThreadRecents @10` → `getThreadGeneric` (`messages.go:922`) | messages | same as 1 | same as 1 | `TestPrivateRecentsDenied…` |
| 3 | `rtSend @3` sender auth (`messages.go:111`) | — (writes) | team role ≥ writeRole | + ACL row | `TestPrivateSendDenied…` |
| 4 | `rtSend` fan-out (`fanoutInboxVersions`) | inbox bumps, `push_outbox`, wakes | targets all `user_channels` rows | rows are ACL-exact by §4.2 invariant; **re-validate each recipient against `team_members` at send time and prune stale rows** (§6.3) | `TestPrivateFanoutOnlyMembers`, `TestPrivatePushOutboxOnlyMembers` |
| 5 | `rtListAllChannelsForTeam @2` (`channels.go:116`) | channel metadata incl. encrypted name, last-msg | `tier = ANY(tiers)` | `OR (c.private AND EXISTS channel_acl row for caller)` | `TestPrivateChannelInvisibleInList` |
| 6 | `rtGetChannel @1` (`channels.go:37`) | one channel's metadata | team role (+ tier/read-role gate in export) | chokepoint; RowNotFound for non-members | `TestPrivateGetChannelDenied…` |
| 7 | `rtGetChangedThreads @6` (`inbox.go:119`) | per-channel inbox rows + metadata | per-team re-auth; admin-tier skip; read-role gate | + skip `private` rows without an ACL row (guards lingering rows) | `TestPrivateChangedThreadsAfterRevoke` |
| 8 | `rtPollInbox @8` / `rtGetInboxVersion @5` | inbox version (a number) | none needed (no channel identity) | none — but the version MUST NOT bump for non-members: follows from 4 | covered by 4 |
| 9 | Late-join fan-in (`fanin.go:267`) | adds `user_channels` rows | role ≥ readRole | `AND NOT c.private` | `TestPrivateNotFannedInOnJoin` |
| 10 | `rtReadThrough @7` (`messages.go:547`) | — | needs a `user_channels` row | unchanged (row deleted on revoke) | `TestPrivateReadThroughAfterRevoke` |
| 11 | `rtNewChannel @0` creation + fan-out (`channels.go:630`) | `user_channels` rows for the team | all team members ≥ readRole | creator must be admin+ (Q2b); fan-out to creator only | `TestPrivateCreateRequiresAdmin`, `TestPrivateCreateFansOutToCreatorOnly` |
| 12 | `rtChannelGrant/Revoke/Members @12-14` | ACL | — | chokepoint `want=manage` (owner or team admin) | `TestPrivateGrantRequiresOwnerOrAdmin` |
| 13 | `rtSelectVHost @9`, `rtSetPushToken @11` | — | n/a | n/a (no channel data) | inventory guard only |
| 14 | `channel_parties` / `lastSenderJoin` (`channels.go:201`) | last sender in listing | rides on 5 | rides on 5 | covered by 5 |
| 15 | push relay (`seco-server/push-relay`) | APNs wake | reads `push_outbox` | content-free; correct iff 4 is | covered by 4 |

**Completeness (verified 2026-08-28, not assumed).** A repo-wide grep for
reads of `channels`, `messages_enc`, `messages_clear`, `user_channels`,
`channel_parties`, `channel_sets` finds exactly four non-test Go files —
`server/realtime/{channels,messages,inbox,fanin}.go` — containing 15 query
sites, every one of which maps to a row above. No admin tool, CLI, export,
backup or federation path touches these tables. The two sites not named
individually above are internal metadata loads inside listed paths:
`messages.go:56` (`lockChannel`, the send path, row 3) and `messages.go:485`
(`loadChannel` in `readThroughMarker`, row 10).

Anything not in this table that reads `channels`, `messages_enc`,
`messages_clear`, or `user_channels` for a caller is a bug. §8.3 makes adding a
new RPC without classifying it a test failure.

## 6. Lifecycle & authority

### 6.1 Who manages membership
Creation is admins/leaders only (Q2b). Membership is managed by channel
**owners** (the creator; owners may promote members to owner) and by **team
admins/owners**. Admins are *not* implicit readers (§7.2): to read, an admin
grants themselves, which writes an ACL row with `granted_by = self` that every
member sees via `rtChannelMembers`. Leaders' access is therefore transparent
rather than invisible — and the product copy says so up front (§7).

### 6.2 Existence
Team admins can list private channels' *existence* (id, ctime, member count —
not the name box, not last-message data) via `rtListAllChannelsForTeam` when
`IsAdminOrAbove`. Rationale: an admin must be able to find a channel to
moderate it, and a community whose admins cannot enumerate its channels cannot
enforce its agreements. Non-admin non-members learn nothing.
**Open decision (Q1):** hide existence from admins too, and rely on a member
reporting a channel? Recommendation: no — see rationale.

### 6.3 Team leave / removal (the lifecycle problem)
The outer gate (`AuthorizeUserForTeam`, chokepoint step 2) already denies every
*read* to an ex-member — that is the win of option (1). What the outer gate does
not cover is **delivery**: `fanoutInboxVersions` targets lingering
`user_channels` rows, so an ex-member keeps receiving inbox bumps and
content-free push wakes for every channel they were in. This is a pre-existing
upstream behaviour for public channels too (activity-metadata leak to
ex-members — **report to Max**, see §9).

For private channels the fix is exact and cheap because private channels are
small: at send time, after selecting the recipient rows, verify each recipient
against `team_members` (users DB, one indexed lookup per recipient) and, for
any that fail, delete their `channel_acl` + `user_channels` rows in the same
transaction before fanning out. A revoked-by-team-leave member thus stops
receiving anything from the first message after their removal. `channel_acl`
rows also carry `granted_by` so a member removed and later re-admitted to the
team is NOT silently back in the channel — re-grant is explicit.

### 6.4 Channel owner leaves the team
ACL row pruned by §6.3. If no owner remains, team admins retain management
(§6.1); a channel with no owner and no admin action is simply read-only-frozen
for its members until an admin acts. No automatic ownership transfer.

### 6.5 Revoke semantics for the revoked user's client
Revoke bumps the user's `user_inbox` version with the channel absent from
their next changed-threads page. Client rule (follow-up spec): a channel that
was present and is now absent from a full sync is removed locally; cached
plaintext is deleted. The server cannot enforce client-side deletion — this is
documented as part of the guarantee statement.

## 7. Decisions — RESOLVED 2026-08-28 (Stefan)

| # | Question | Decision |
|---|---|---|
| Q1 | May team admins see that a private channel *exists*? | **Yes.** |
| Q2 | May team admins grant themselves membership? | **Yes**, and the grant is visible to members (`granted_by` via `rtChannelMembers`). |
| Q2b | **New rule:** who may *create* a private channel? | **Admins/leaders only** — enforced server-side (§7.1), not just in the UI. |
| Q3 | Is the daemon ever a member? | Not by default; an owner may grant it like any member. Its summaries/corpus then include the channel — surface that in the grant UI. |
| Q4 | Rekey the team on revoke? | **No** for v1; the consequence is stated in §1 and in the product copy. |
| Q5 | Tag/pin plan | Tag today's `main` (`0c076a0`) `v0.1.9-seco.14`, bump the three go.mod pins off `seco.11`; this work lands as `seco.15`. |

**Rationale for Q1/Q2 (Stefan's, and it is the better one).** The earlier
argument was "admins need moderation and recovery powers". The stronger
argument is honesty: private channels are a *leadership* tool, so the only
model that never misleads anyone is one where leaders are transparently peers.
A member must never be able to believe a channel is private *from leaders* when
it is not. That gives one sentence of product copy that is simply true:

> Private channels are hidden from other members. Community leaders can see
> that they exist and can join them.

### 7.1 Consequence of Q2b, accepted deliberately

Restricting creation to admins means **members cannot create private subgroups**
— and SECO has no fallback for that today: DMs are 1:1 only, and group ad-hoc
teams are unavailable to us (they require open viewership; see the DM /
adhoc-team findings). A member wanting a private space with two friends has no
path in v1. **Confirmed acceptable for v1 (Stefan, 2026-08-28)** — the simpler,
more honest scope. Revisit only if the need shows up in practice; the likely
answer then is group DMs rather than member-created private channels.

Enforce it in the chokepoint (`want == create` requires `IsAdminOrAbove`), not
only in the UI: a UI-only rule is not a rule, and it costs ~3 lines to make the
invariant real. Blast radius if it were UI-only is admittedly small — an
unauthorized private channel would still be visible and joinable by every admin
(Q1/Q2) — but "the server enforces what the product promises" is worth more
than the three lines.

### 7.2 Why NOT implicit admin membership

A tempting simplification of Q1/Q2 is "admins are implicit members of every
private channel" (the Keybase property). Rejected: it would couple channel
membership to team-role changes — every promotion/demotion would have to
rewrite `user_channels` across every private channel in the community, a new
lifecycle cascade of exactly the kind this design is trying to avoid — and it
would give admins delivery and push wakes for channels they never opened.
Explicit, visible self-grant achieves the same transparency with none of that.

## 8. Test plan (`integration-tests/lib/`, pattern: `rt_push_token_test.go`)

**Harness (verified 2026-08-28):** `integration-tests/lib/` already supports
exactly this shape — `tew.NewTestUser(t)` per actor, `tew.makeTeamForOwner`,
and `tm.makeChanges(t, m, owner, []proto.MemberRole{u.toMemberRole(t, role,
hepks)}, removals)` to add/remove members at chosen roles
(`rt_test.go:20-32`). No harness work needed.

### 8.1 One negative test per path (§5 table)
Each test: team with users A (creator/owner), B (granted member), C (team
member, not in channel), D (team admin, not in channel), E (removed from team
after being granted). Assert per path: A and B succeed; C gets RowNotFound (not
PermissionError — existence hidden); D gets existence-only where §6.2 allows,
RowNotFound elsewhere until self-grant; E gets nothing and, after one send,
has no `user_channels`/`channel_acl` rows.

### 8.2 Invariant tests
- `TestPrivateAclEqualsUserChannels`: after create, grant×n, revoke×m,
  team-remove, the two row sets are equal.
- `TestPrivateNoInboxBumpForNonMember`: C's `user_inbox` version and
  `push_outbox` rows unchanged across sends into the private channel.
- `TestPrivateSameErrorAsMissingChannel`: C's error for a private channel is
  byte-identical to the error for a random channel id.

### 8.3 Protocol inventory guard (the "bug in three years" test)
`TestRealtimeRpcInventory` parses `proto-src/rem/realtime.snowp`, extracts every
`name @N` method, and asserts it is present in an explicit map
`rpcAccessClass[name] = {noChannelData | channelData | channelWrite}` with a
pointer to the test that covers it. A new RPC fails this test until it is
classified and covered. Same for any new `SELECT … FROM channels|messages_enc`
in `server/realtime/*.go` outside `authorizeChannel` and the two set-based
predicates — enforced by a source-grep test with an allowlist of call sites.

## 8.4 Ready-to-build checklist

Resolved (no further research needed): path inventory complete (§5), sparse
numbering verified against real codegen (§4.4), PG enum constraint dodged by
design (§4.1), test harness confirmed sufficient (§8), fork base and tag state
established (§Base, Q5).

Blocking, and all of them are Stefan's calls rather than research:

- [ ] **Q1–Q4** (§7). Q1/Q2 in particular gate the chokepoint's step 5 — the
      code cannot be written without them.
- [ ] **S0**: tag fork `main` (`0c076a0`) `v0.1.9-seco.14`, bump the three
      go.mod pins from `seco.11`, re-run gates. Independent of this spec and
      unblocks the RT flip on its own.
- [ ] **Product copy** for the guarantee statement (§1): what a private channel
      claims, in user-facing words. This is the thing that makes option (1)
      honest rather than misleading, so it ships *with* the feature, not after.

Not blocking the server build, needed before the feature is usable:

- [ ] Client follow-up spec: per-channel transport keys (the app transport is
      hardcoded to the default channel today — `rtGetThread(team, "", …)`,
      store keyed by team, wake loop per team), bridge surface across the nine
      places a method must land, channels UI, daemon channel kind.

## 9. Implementation plan (server)

| Phase | Work | Est. |
|---|---|---|
| S0 | Tag fork `main` as `seco.14`, bump app pins, confirm parity/gates green on it (unblocks the flip independently of this spec) | ½ day |
| S1 | Patch `p2.sql` (tier value + `channel_acl`), proto enum + 3 RPCs, codegen | ½ day |
| S2 | `authorizeChannel` chokepoint; migrate paths 1, 2, 3, 6, 12 to it | 1 day |
| S3 | Set-based gates: listing (5), changed-threads (7), fan-in exclusion (9); creation fan-out to creator only (11) | 1 day |
| S4 | Send-time recipient re-validation + prune (4, §6.3); grant/revoke transactions with inbox bumps | 1 day |
| S5 | Tests §8.1–8.3 (write these alongside S2–S4, not after) | 1–2 days |
| S6 | librt wrappers; fork PR; tag `seco.15`; bump pins; CLI smoke (`foks` CLI can create/grant/read) | ½ day |

Client work (bridge ×9 surfaces, transport per-channel keys, channels UI,
daemon channel kind) is the follow-up spec and is NOT on this critical path.

## 10. What actually needs Max (and what does not)

Answered ourselves, do NOT spend his time on:
- *Did we miss a path?* — settled by enumeration (§5 completeness note).
- *Is a fork enum value safe?* — our call; sparse numbering verified against
  the real codegen (§4.4).

Worth sending him, as a **report** rather than a question: §6.3's pre-existing
ex-member delivery leak — `fanoutInboxVersions` bumps lingering `user_channels`
rows, so a user removed from a team keeps receiving inbox bumps and push wakes
for its channels. Evidence it is an oversight rather than a decision: the
changed-threads path guards exactly this class ("membership rows alone must not
leak channel activity", `inbox.go:169`) and the fan-out does not.

Genuinely only-Max, in priority order: (1) review/merge foks-proj/go-foks#341
and #342 — unblocks our flip; (2) the rekeyed-team join failure in `openCert`'s
stacked-signature branch when `team_certs.gen > 1`; (3) the subteam design
sketch, as the durable artifact for a later, better version of this feature.

## Appendix — code references (fork `main` @ 0c076a0; line numbers from the 2026-08-28 read)

- Key derivation: `client/librt/minder.go:785`, `client/librt/crypto.go:29-48`
- Tier at creation: `client/librt/minder.go:255-290`
- Read gate: `server/realtime/messages.go:1005-1040` (`getThreadGeneric`, `loadChannelForRead` @654)
- Send gate: `server/realtime/messages.go:111-120`
- Fan-out: `server/realtime/messages.go:245-350`
- Creation fan-out: `server/realtime/channels.go:630-705`
- Listing tier gate: `server/realtime/channels.go:105-150`; read-role gate `applyReadRoleGate` @322
- Changed-threads: `server/realtime/inbox.go:119-220`
- Fan-in: `server/realtime/fanin.go:1-30, 224-311`
- readThrough: `server/realtime/messages.go:547-566`
- Team auth: `server/realtime/auth.go`
- Team-change signal: `server/shared/team.go:62-95`
- Schema: `server/sql/foks_realtime.sql` (`channel_tier` @52, `channels`, `user_channels`, `user_channels_inbox_idx` @195); patches `server/sql/patches/foks_realtime/`
- Design intent: `docs/chat-server-design.md` §user_channels (446), §Inbox View (563)
