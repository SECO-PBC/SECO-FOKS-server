# Channel mutation: rename, edit description, archive (fork-only)

**Status:** IN PROGRESS on `feat/rt-channel-mutation`. Drafted and revised
2026-09-22; steps 0–2 of §8 are built (bug fixes, the `touchChannelSet` move,
p9, and the archived gate). Corrections found while building are marked
**As built** inline.
**Scope:** fork-only for now, but written to be proposable upstream —
asked as [foks-proj/go-foks#351](https://github.com/foks-proj/go-foks/issues/351),
open and unanswered since 2026-09-15.
**Base:** fork `origin/main` @ `796c187` (2026-09-22, PR #45 merged), released
state `v0.1.9-seco.20`. Schema at `foks_realtime` patch p8; the members-only
KV store (`docs/kv-channel-acl.md`) is in.
**Product driver:** `blueprints/channels/CHANNELS.md` §5.1 — the `✏️` rename /
edit-description control and the **Archive** foot action. Tracked in
`REVIEW-CHANNELS.md` as blocked on the server since 09-15.
**Precedent:** this extends the machinery built for private channels
(`docs/rt-private-channel-acl.md`). Read §§4.3–4.4 of that document first; the
chokepoint, the set-based predicate, the RPC/field numbering rules and the
inventory guard are all reused here rather than re-invented.
**Upstream tracking:** `SECO-UPSTREAM.md`, *Queued*, row "channel mutation".
Per `AGENTS.md` that row is the source of truth and must be updated in the
same change that makes it stale — including when #351 is answered, when either
half opens as a PR, and whenever the numbering in §9.4 moves.

---

## 1. What this gives, and what it does not

Three mutations, on a channel that today is write-once:

- **Rename** — replace `name_box` with a fresh sealing of a new name.
- **Edit description** — replace (or clear) `desc_box`.
- **Archive / unarchive** — close the channel to new activity and drop it out
  of the inbox, reversibly, while keeping it in the team's channel set so its
  name stays reserved (§3.2).

**What "archive" means here, exactly.** The blueprint's own words: *"a
container cannot truly be deleted, so archive is a hide"*. That is not a
concession, it is the design. An archived channel keeps every row it had —
messages, `channel_parties`, `channel_acl`, `user_channels`. Nothing is
destroyed, nothing is purged, and no background reaper exists or is wanted.
The channel stops accepting writes and leaves the active view.

**Non-goals**, each deliberate:

- **No hard delete.** Purging `messages_enc` for a busy channel is unbounded
  work inside one transaction, so it would need a reaper and a retention
  policy — and it still would not remove the copies members already hold in
  their local soft DB, so it does not deliver the guarantee its name implies.
  Out of scope, and the product never asked for it.
- **No rekey.** Renaming does not rotate the PTK. Anyone who could read the
  old name can read the new one; anyone who held ciphertext keeps it. Same
  trade as private channels (`rt-private-channel-acl.md` §1), for the same
  reason.
- **No per-user hide or mute.** `user_channels.hidden` and `.muted` stay
  unwritten. Per-channel muting is explicitly out of MVP
  (`CHANNELS.md`, Deferred Decisions); the MVP mute is per-**thread** and
  belongs to `THREADS.md`. The per-member "get it out of my list" action is
  **Leave**, which already ships on KV follow records and on
  `rtChannelRevoke @201` for private channels.
- **No channel-set pagination.** Unchanged from Stage 1a — and note archive
  makes it very slightly *worse*, not better, since archived channels stay in
  the set (§3.2). The set grows by one row per channel ever created. That is
  the accepted cost of reserving names, and it is bounded by how many channels
  a community creates, not by traffic.
- **Archive does not reach the members-only KV store.** Since
  SECO-FOKS-server #45 (merged 2026-09-22, `docs/kv-channel-acl.md`) a private
  channel holds files in the community team's KV store, every node tagged with
  the channel and gated by the same `channel_acl`. Archiving a channel does not
  touch those nodes — the right default for a reversible hide, and a second
  reason a hard delete would have been far larger than it looked: a purge
  would have had to cascade across two stores. The consequence to state in the
  app spec: **a member of an archived private channel can still reach its
  stored files**, and not only in theory — addressing a node directly by ID is
  the *normal* mode in that store, not an edge case (`kv-channel-acl.md` §2.1:
  "the server never sees paths"), and the ACL that gates it is untouched. If
  archive should also close the store, that is a `kv-channel-acl` change, not
  this one (§6 Q5).

## 2. Existing building blocks — nearly all of this is already built

The private-channel work built, for grant and revoke, almost exactly the
machinery these three mutations need. This is the single most important fact
about the size of this change.

| Mechanism | Where | What it already does |
|---|---|---|
| **`channels.seqno`** | `foks_realtime.sql:66` | Documented "metadata seqno; CAS on update". Written as `1` at creation (`channels.go:insertChannel`) and **never incremented by anything**. It exists for precisely this feature and has been dormant since Stage 1a. |
| **`touchChannelSet`** | `acl.go` | Bumps `channel_sets.vers` and stamps `channels.updated_at_set_vers` at the new version, in the caller's transaction. Built so a grant/revoke surfaces in the incremental listing. **Rename and archive call it unchanged.** |
| **Incremental set merge** | `client/librt/minder.go` | The client merges each delta by channel id and stomps its cached copy, so a row whose `archived` flag flipped updates in place. Archive needs nothing more than that. (The adjacent full-re-list path at `:689` exists for private-channel *revoke*, where the row genuinely vanishes; archive does not use it.) |
| **`authorizeChannel`** | `acl.go` | The one chokepoint for per-channel access, with `accessRead / accessWrite / accessManage / accessRoster`. Loads team, tier, roles, `private`, `no_push` and the caller's current team role; optional `FOR UPDATE`. |
| **`privateVisibleToCaller`** | `acl.go` | The set-based equivalent of the chokepoint's private gate, embedded by the two paths that read many channels at once. The pattern archive's predicate follows. |
| **`wakeInboxPollers`** | `messages.go` | Post-commit wake of parked long-pollers. |
| **"Bump, don't stamp"** | `acl.go:dropChannelMember` | Bumping `user_inbox` without stamping a `user_channels` row, to force the revoked member's next sync into a full sync. The exact trick archive needs for members of an archived channel. |
| **Inventory guards** | `server/realtime/rt_inventory_test.go` | `TestRealtimeRpcInventory` fails until a new RPC is classified in `rpcAccessClass`; `TestRealtimeProtectedTableQueries` fails until a new query against a protected table is allowlisted **with its count**. Both run in CI (no database needed). |
| **Schema patches** | `server/sql/` | p1–p8 applied; next is **p9**. |

What is genuinely new: one column, two RPCs, one predicate, and the client
re-seal path.

## 3. Design

### 3.1 Rename is a metadata CAS, not a new mechanism

`rtUpdateChannel` takes the channel id, the caller's expected `seqno`, and the
new boxes. The server authorizes, CAS-updates `channels` on
`seqno = expected`, and calls `touchChannelSet`.

```sql
UPDATE channels
SET name_box=$3, name_box_ptk_gen=$4, desc_box=$5, desc_box_ptk_gen=$6,
    seqno=seqno+1, mtime=NOW()
WHERE short_host_id=$1 AND channel_id=$2 AND seqno=$7
```

`RowsAffected() != 1` → `core.RTRaceError{Which: "channels"}`, which librt's
existing 5-try backoff loop (`MakeChannelWithTestHooks`) already knows how to
retry. Two devices renaming at once: one wins, the other re-reads and retries.

**The name must be re-sealed at the tier's name role, not the caller's.**
`minder.go:308–327` derives `nameRole = MinRTRole` for a bottom-tier channel
and `AdminRole` for an admin-tier one, then seals with
`PrivateKeysForRole(nameRole)`. A rename that seals at the caller's own role
instead would either leak an admin channel's name to ordinary members or seal
it so members who could read the old name cannot read the new one. **Extract
the tier→nameRole mapping into one function and call it from both create and
rename** — this is the highest-consequence line of the whole change, and
duplicating it is how it eventually diverges.

The description is sealed at the channel's read role, as at create.
`desc_box` and `desc_box_ptk_gen` are both nullable, so clearing a
description is `NULL, NULL` and needs no separate RPC.

**Collision detection stays client-side and unchanged.** The server cannot
compare ciphertext names. librt already builds a `{name, tier}` map from the
listing before creating; rename does the same before mutating, and returns
`RTChannelExistsError`. Private channels skip the map, exactly as at create
(`rt-private-channel-acl.md` §4.5) — and the pre-existing ambiguity that
skipping creates is unchanged by this work, not worsened.

**Renaming re-seals at the current PTK generation**, which is a free side
benefit: a channel whose name box is pinned to an old generation gets
refreshed whenever anyone renames it. It is not a rotation mechanism and
should not be described as one.

### 3.2 Archive is a flag; the channel stays in the set and leaves the inbox

```sql
ALTER TABLE channels ADD COLUMN archived_at TIMESTAMPTZ;   -- NULL = live
```

A **nullable timestamp, not a boolean**, for the same reason `channel_acl`
carries `granted_by`: the interesting question after the fact is always *when*,
and the column is free. `archived_at IS NOT NULL` is the predicate everywhere.

**The split that makes the rest of this design work.** An archived channel
stays in the **channel set** and disappears from the **inbox**:

| Surface | Archived channel | Why |
|---|---|---|
| `rtListAllChannelsForTeam` (the set) | **present**, flagged `archived` | The set is the team's name registry as well as its channel list. Keeping the row is what reserves the name (§3.3). |
| `rtGetChangedThreads` (the inbox) | **delivered once, carrying `archived`** | A delta of rows cannot express a removal, so there is no server-side filter here. The archived channel arrives one final time with the flag set and the CLIENT drops its stored row (`SyncInbox`); after that it never bumps again, so it never reappears. Filtering server-side would leave it in every member's inbox forever. |
| Late-join fan-in | **skipped** | Otherwise every sync re-fans members into it forever. |
| `rtSend` | **refused** | "Closes the channel to new activity." |

This is non-obvious enough to be worth stating twice: the listing and the
inbox take *opposite* decisions about the same row, deliberately.

Because the row stays in the set, the archive transition rides the ordinary
incremental delta. `touchChannelSet` bumps `channel_sets.vers` and stamps
`updated_at_set_vers`, so the archived row comes down on the next sync with
`archived = true` and the client stomps its cached copy — no tombstone
protocol, and no reliance on the full-re-list path at `minder.go:689` (that
still exists for private-channel revoke, where the row genuinely does vanish).
`archived @22` on the wire is therefore required, not speculative: the
client needs it to hide the channel from the UI while keeping it in the
name-collision map.

An archived private channel's set visibility is unchanged — gated by
`channel_acl`, so only its members see it, and a non-member admin still sees
existence-only. Archive does not widen anything.

### 3.3 Archiving reserves the name, which is what makes revive safe

Reviving is flipping `archived_at` back to `NULL` and calling
`touchChannelSet`. Everything downstream is derived: the channel reappears in
the inbox, the fan-in re-populates through the existing path
(`fanin.go:reconcileUserChannels`), the full history is intact because nothing
was removed, and a private channel's `channel_acl` comes back untouched.

The collision hazard that threatened this is closed by §3.2. Because an
archived channel stays in the listing, it stays in the client's
`{name, tier}` collision map, so **nothing can take its name while it is
archived** — a create or rename onto that name is refused with
`RTChannelExistsError` exactly as it would be against a live channel. Revive
therefore cannot collide with a channel created in the meantime, and it needs
no special check of its own.

This is Slack's model, and worth adopting for the same reason Slack has it:
the alternative — freeing the name on archive — means every revive is a
potential collision, and a collision is unresolvable server-side because names
are ciphertext. Reserving is the cheaper half of the trade.

The cost, stated so nobody is surprised: **you cannot reuse an archived
channel's name.** The escape hatch is rename, which this same change
introduces — rename the archived `#2025-planning` to `#2025-planning-old` and
the name is free again. Rename and archive are better together than either
alone, which is an argument for shipping them in one release rather than two.

Two things this does **not** fix, both pre-existing and neither made worse:

- **A private channel's name is invisible even to admins** (`granted` names
  are in `name_box`; `rt-private-channel-acl.md` §6.2 gives admins
  existence-only). So an admin unarchiving a channel cannot check its name
  against private channels they are not in. This is the standing private/open
  collision, open for Jay, and the revive case is a strict subset of it —
  which is exactly why it should not get a separate mechanism.
- **Server-enforced global name uniqueness is not achievable** under the
  current crypto design, and it is worth writing that down so it stops being
  re-proposed. The server cannot compare ciphertext names. A deterministic
  name hash would let the server (and a self-hosting operator) dictionary-
  attack low-entropy channel names. A keyed hash under the PTK fixes that but
  breaks on rotation, since two channels sealed at different generations
  produce different digests for the same name and the unique index stops
  catching them. Client-side reservation, which is what §3.2 gives, is the
  best available answer.

Whether the app exposes unarchive at MVP is a product question
(`CHANNELS.md` does not ask for it) — but the server should support it either
way, because the cost is one `Bool` and the alternative is a wire change later.
The RPC therefore takes a **target state**, `rtSetChannelArchived(chid, seqno,
archived)`, not a one-way verb.

`user_channels` rows are **left in place** on archive and filtered at read
time, never deleted. Deleting them would make revive an O(members) re-fan with
one inbox-version allocation per row — `user_channels_inbox_idx` is UNIQUE and
forbids batch-stamping — and would throw away every member's `read_through`.
Leaving them costs nothing, because every read path already filters archived.

### 3.4 The chokepoint learns one new gate and one new access kind

`authorizeChannel` gains `archivedAt *time.Time` on `channelAuth` (from
`channelAuthCols`) and one gate, placed **after** the private existence gate
and **before** the tier gate:

```go
// An archived channel is closed to new activity but still readable by id.
// Placed after the private gate so it cannot be used to probe for the
// existence of a private channel the caller is not in.
if ca.archivedAt != nil {
    switch want {
    case accessWrite, accessManage:
        return nil, core.RTChannelArchivedError{}
    case accessRead, accessRoster, accessMutate:
        // allowed; reads by id still work (Q1), and accessMutate must pass
        // or unarchive could never run.
    }
}
```

Plus a new kind, `accessMutate`, for the two new RPCs themselves: requires
admin-or-above and passes the archived gate. **It must not inherit
`accessManage`'s private-only restriction** — `accessManage` rejects a
non-private channel outright (`"channel is not private; it has no ACL"`), and
rename/archive apply to every channel. That is the whole reason it cannot
simply reuse `accessManage`, and it is the easiest thing to get wrong when
implementing.

**Decision, needs sign-off (§6 Q1): an archived channel stays readable by
explicit channel id.** Writes and ACL management are refused; `rtGetThread`,
`rtGetThreadRecents` and `rtGetChannel` still serve it. The reasoning: the
channel is hidden, not destroyed; in practice no client can reach it because
the id is gone from every listing; and keeping reads open is what lets a
future "browse archived channels" admin surface ship without another server
change. The alternative — deny reads too — is one line, and is the right call
if the product wants archive to read as closer to deletion.

**Archiving must also settle the channel's pending pushes.** Two rows'-worth
of state would otherwise be stranded, and neither is hypothetical:

- **Held rows.** Under a push hold (`pushhold.go`), a send writes
  `push_outbox` rows with status `'held'`, which only the holder may decide.
  Archive the channel and nobody ever will: the rows sit `'held'` forever,
  never delivered, never reaped.
- **Queued rows.** Rows already `'queued'` will still fire, buzzing members
  about a channel that has just closed.

**As built: both are DELETED**, by `dropChannelPushes` in the archive
transaction. `'sending'` rows are left alone — the relay owns them, and racing
it is worse than one late push.

Deleting rather than releasing is the part worth stating, because
`RevokeChannelMember` does the opposite: it calls `releaseChannelHeld`, moving
held rows to `'queued'`, which is right there because the channel is still
live and those members still want their notifications. Archive must not reuse
that helper. Releasing a held row on archive would fire a notification for a
room that has just closed — the exact thing deleting the queued rows is there
to prevent.

Unarchive does neither; there is nothing to restore, and a resurrected push
would be a message from the past.

`no_push` is orthogonal and needs no interaction: a no-push channel simply has
fewer rows to clean up.

Set-based paths cannot call the chokepoint, so they embed the predicate,
exactly as private channels do. **As built,** both helpers live in
`channels.go` rather than `acl.go`, and `archivedBlocks` takes the loaded
timestamp rather than an `accessKind`, so neither depends on the fork-only
authorization vocabulary and both can move upstream unchanged (§9.3):

```go
// notArchived is the set-based equivalent of the chokepoint's archived gate.
// Keep in lock-step with it; the archive tests run the same scenarios through
// both, which is what keeps them honest.
func notArchived(chAlias string) string {
	return chAlias + `.archived_at IS NULL`
}
```

### 3.5 Wire changes

`proto-src/rem/realtime.snowp`, then `go generate ./proto/rem/`:

```
rtUpdateChannel      @207 (chid : lib.RTChannelID, seqno : lib.RTChannelSeqno,
                           nameBox : lib.RTBoxRG, descBox : Option(lib.RTBoxRG));
rtSetChannelArchived @208 (chid : lib.RTChannelID, seqno : lib.RTChannelSeqno,
                           archived : Bool);
```

`RTChannelMetadata` gains `archived @22 : Bool`.

Both numbers follow the rules the private-channel work established and
verified, which are **not** the same rule for methods and fields:

- **RPC numbers are method ids and cost nothing to scatter.** The fork's
  reserved block is `@200+`; `@200`–`@206` are taken by grant/revoke/members
  and the push-hold quartet, so these are `@207` and `@208`. The upstream
  proposal uses different numbers — see §9.4, which is the one place the three
  sequences are reconciled; do not re-derive them here.
- **Field numbers are positional** (the codegen emits `codec:",toarray"`), so
  a gap is nil padding on every encoded record. The fork's field block starts
  at `@20`; `private @20` and `noPush @21` are taken, so `archived` is `@22`.
  Do not reach for a high number here — `private @64` measured 133 bytes
  against 89 at `@20`, on a struct returned once per channel in every listing
  and every inbox page.

One new status code, wired per the `add-foks-error-type` path (enum in
`proto-src/lib/status.snowp`, variant arm, `make proto`, then the type and
both conversion switches in `lib/core/errors.go`):
`RT_CHANNEL_ARCHIVED_ERROR @12008` → `core.RTChannelArchivedError`. The
existing RT block runs `@12001`–`@12007`.

### 3.6 librt and the agent

- `UpdateChannel(m, team, appID, chSpec, newName, newDesc)` — resolves the
  channel from the listing (so it has `seqno`), re-runs the collision map,
  re-seals via the extracted tier→nameRole helper, and calls `@207` inside the
  existing `RTRaceError` retry loop.
- `SetChannelArchived(m, team, appID, chSpec, archived Bool)` — same shape,
  and **no collision check on unarchive**: §3.3 reserves the name for the
  whole time the channel is archived, so there is nothing to collide with.
- **The listing filter is the caller's, not the agent's. As built,**
  `ListAllChannelsForTeam` returns archived channels alongside live ones,
  each carrying `archived`, and does *not* partition them. Deciding what a
  channel list shows is a product question, and the agent is the wrong place
  to settle it; the CLI marks archived rows, and the app must filter on the
  flag when it wires this up. An earlier draft of this section described a
  partition that was never built — recorded here rather than quietly dropped,
  because the app work depends on which of the two is true.
- Both drop through to agent (`lcl`) RPCs following the `clientRTChannelGrant
  @5` / `Revoke @6` / `Members @7` precedent → `clientRTUpdateChannel @8`,
  `clientRTSetChannelArchived @9`, addressing the channel by
  `RTChannelSpecifier` like every other agent RT call.
- **`#general` is refused by the CLIENT, and only by the client.** The
  blueprint says the default channel is never archived, and `#general` *can*
  be renamed. librt refuses to archive a channel whose name is empty, which is
  what the default channel carries — an exact check, because the client can
  read the name.
  **As built (§6 Q6): the server has no opinion.** An earlier version had it
  refuse the team's oldest public bottom-tier channel, as an approximation of
  "the default one". That was wrong twice over: it guessed wrong whenever
  anything public was created before `#general` — protecting the wrong channel
  while leaving the real one archivable — and "there is a default channel" is
  a SECO product rule that a general-purpose realtime server should not be
  carrying at all. Removed.

### 3.6.1 Opt-in duplicate names (fork-only, `seco.22`)

**As built (2026-09-23).** The SECO app now identifies channels by id and treats names as display text: two channels may share a name, and an archived channel's name is free again. `MakeChannelOpts.AllowDuplicateName` and `UpdateChannel`'s `allowDuplicateName` argument (both crossing the agent RPC as `allowDuplicateName` on `clientRTMakeChannel @4` / `clientRTUpdateChannel @3`) skip:

- the `{name, tier}` collision check;
- the refusal of the name `general`.

The empty name is never covered: `None`-specifier lookups resolve it, so it stays refused on update, and on create it keeps its collision check even with the flag set. Either would otherwise make a second nameless default channel.

**Off by default, on purpose.** A caller that creates a channel on demand relies on the collision check to create it exactly once. Two such callers exist: the app's `seco-ctl` control channel, and a DM team's nameless default channel. Only the app's user-facing create and rename paths set the flag. Test: `TestAllowDuplicateNameOnCreateAndRename`.

This changes §3.3 for callers that set the flag: for them an archived channel no longer reserves its name. Callers that don't set it keep §3.3 as written.

### 3.7 Two existing bugs sat on exactly this path — fixed first

**As built: both are fixed**, in `318f2fb`, ahead of everything else on this
branch. They were live on `origin/main` when this was written, and both were
two-line fixes. Creation was the only thing that exercised this code, so they
were nearly unreachable; rename and archive would have driven them on every
mutation. Coordinates below are the pre-fix ones.

- **`channels.go:99` — `readChannelSet` reports success on a database
  error.** The `pgx.ErrNoRows` branch correctly returns a zero version with a
  nil error, and the branch immediately below it, catching *every other
  error*, does the same. A transient database fault therefore tells the
  caller the team's channel set is at version 0, so the client proposes v1 and
  races against a set that is really at v40.
- **`channels.go:537` — `updateChannelSet` reports success when its CAS
  `UPDATE` fails.** The `Exec` error path returns `nil` instead of the error,
  so the `RowsAffected() != 1` check below is never reached and a failed
  version bump looks committed.

Fix them as a standalone commit before anything else. They are also clean
upstream contributions on their own — upstream carries both, unchanged — and
landing them separately keeps them out of the feature diff.

## 4. Path inventory — the delta on `rt-private-channel-acl.md` §5

Same 15 rows; only the changed ones are repeated. "Archive gate" is the
addition. Every row must have a test (§7).

| # | Path | Archive gate | Mutation gate | Test |
|---|---|---|---|---|
| 1 | `rtGetThread @4` → `getThreadGeneric` | **none** — readable by id while archived (§3.4 Q1) | — | `TestArchivedThreadStillReadableById` |
| 2 | `rtGetThreadRecents @10` | same as 1 | — | covered by 1 |
| 3 | `rtSend @3` sender auth (`lockChannel` → chokepoint, `accessWrite`) | **refused**, `RTChannelArchivedError` | — | `TestArchivedChannelRejectsSend` |
| 4 | `rtSend` fan-out | unreachable (3 refuses first) | — | covered by 3 |
| 5 | `rtListAllChannelsForTeam @2` (`channels.go:133`) | **no filter** — the row stays, carrying `archived = true`. This is the name reservation (§3.3); the client hides it from the UI and keeps it in the collision map | — | `TestArchivedStaysInTheListing`, `TestArchivedNameStaysReserved` |
| 6 | `rtGetChannel @1` | none; returns `archived @22 = true` | — | `TestGetChannelReportsArchived` |
| 7 | `rtGetChangedThreads @6` (`inbox.go`) | **none — no server-side filter, deliberately.** The row is delivered once with `archived = true` and the client removes it; `notArchived` is *not* used here. **The app must handle this:** an archived channel arrives in the delta and has to be dropped, not rendered | — | `TestArchivedLeavesTheInbox` (asserts against the stored sync index, not the rendered view) |
| 9 | Late-join fan-in (`fanin.go:findMissingChannels`) | `AND ` + `notArchived("c")` — **without this, every sync re-fans members into archived channels and burns inbox versions forever** | — | `TestArchivedNotFannedInOnJoin`, and a second sync asserting the row count does not grow |
| 10 | `rtReadThrough @7` | **permitted** — it writes only the caller's own `user_channels` row and cannot produce activity for anyone else. Refusing it would make a client that marks read as a pane closes throw errors for no gain | — | `TestArchivedAllowsReadThrough` |
| 11 | `rtNewChannel @0` | an archived channel **does** block its name, because it is still in the listing the collision map is built from (client-side, as ever) | — | `TestArchivedNameStaysReserved`, `TestRenamedArchivedFreesTheName` |
| 12 | `rtChannelGrant/Revoke @200-201`, `rtChannelMembers @202` | grant/revoke **refused** on an archived channel; `Members` still readable | — | `TestArchivedRejectsGrant` |
| 18 | Push cleanup on archive (`channels.go:dropChannelPushes`) | held AND queued rows **deleted**, `'sending'` left alone (§3.4). Not *released*: queuing a held row would fire a notification for a room that has just closed | — | `TestArchiveDropsHeldPushes`, `TestArchiveDropsQueuedPushes` |
| 16 | **`rtUpdateChannel @207`** (new) | **permitted while archived** — renaming an archived channel is precisely how its reserved name is released (§3.3). Refusing it would strand the name forever | `accessMutate`: team admin-or-above; CAS on `seqno` | `TestRenameRequiresAdmin`, `TestRenameCasRace`, `TestRenameResealsAtTierRole`, `TestRenamedArchivedFreesTheName` |
| 17 | **`rtSetChannelArchived @208`** (new) | permitted (it is the gate's own operation) | `accessMutate`. The server does **not** protect the default channel — that is librt's, by name (§3.6) | `TestArchiveRequiresAdmin`, `TestArchivedLeavesTheInbox`, `TestCannotArchiveDefaultChannel` (client-side refusal) |

**Completeness.** The private-channel spec verified that exactly four non-test
Go files touch these tables — `server/realtime/{channels,messages,inbox,fanin}.go`
— and `acl.go`/`pushhold.go` have since joined them. Re-run that grep before
building and reconcile against this table; `TestRealtimeProtectedTableQueries`
will fail on any new or moved query until `queryAllowlist` is updated **with
its new count**, which is the mechanism that makes this list stay true.

## 5. Authority

`CHANNELS.md` §5.1 and the Member Directory capability matrix, translated:

| Action | Product rule | FOKS enforcement |
|---|---|---|
| Rename, edit description | Admins only | `role.IsAdminOrAbove()` |
| Archive an **open** channel | Leaders and Stewards | `role.IsAdminOrAbove()` |
| Archive a **private** channel | Leader only | `role.IsAdminOrAbove()` — see below |
| Archive `#general` | Never | **client only** — librt refuses the empty name; the server has no concept of a default channel (§3.6, §6 Q6) |

**Stewards and Leaders collapse to one role in FOKS.** There is no role
between member and admin (`REVIEW-CHANNELS.md`, 09-15), so "Leaders and
Stewards" and "Leader only" are the same check today. That means the
private-channel rule — archive reserved to Leaders — **cannot be enforced
server-side at MVP**, and a Steward could archive a private channel they can
see. Record it as a known gap against the blueprint rather than implementing a
rule the platform cannot express; it closes when the Steward tier does.

No channel-ownership concept is introduced. `channel_acl.acl_role = owner`
exists for private channels and could gate archive, but extending it to open
channels would invent a second authority model for one action. Carter's
creator-based instinct is recorded in `REVIEW-CHANNELS.md` as moot at MVP
(every creator is already an admin) and should stay moot here.

**Found while writing this: "members cannot create channels" is not enforced.**
`CHANNELS.md` says channel creation is Leaders and Stewards only, and the Hub
draws **+ New** for them alone. Server-side, `channelMaker.checkPerms`
(`channels.go:441`) gates only the *admin tier* and, via
`authorizeChannelCreate`, *private* creation. **An ordinary member can create
an open bottom-tier channel over the RPC today.** The private-channel spec's
own words apply: a UI-only rule is not a rule.

This is a pre-existing gap, not one this change introduces, and it is
deliberately **not** bundled here — the app's realtime transport creates the
default `#general` lazily on first send
(`foksRtChatTransport.ts`: `rtMakeChannel(team, "", "")`), by whoever sends
first, so adding the gate without first auditing that path would break
community bootstrap for a non-admin first sender. Worth its own issue and its
own release. Note the collision design in §3.3 does **not** depend on closing
it: archived names are reserved in every member's listing, not only in
admins'.

## 6. Decisions — RESOLVED 2026-09-22 (Stefan)

All seven are settled. The answer to Q2 settled Q1 and Q5 with it.

- **Q2. Archive or delete, and is there an undo? — ARCHIVE, no undo, no
  delete.** `CHANNELS.md` §5.1 offers **Archive** and nothing else: the word
  "delete" appears only for in-message actions and for `#general` ("cannot be
  deleted"), and "unarchive" appears nowhere in any blueprint. So the app
  archives, never claims to delete, and ships no undo. The server keeps
  `rtSetChannelArchived`'s target-state argument regardless — unarchive costs
  one `Bool` and having it means the app can add the button later without a
  wire change.
- **Q1. Is an archived channel still readable by id? — YES, unchanged.**
  Considered making messages inaccessible until revived. Not needed, and that
  is a consequence of Q2: the UI says *archived*, not *deleted*, so "the room
  is closed and hidden, the history is still there" is a true story. Hiding
  the history would only be required if we told members it was gone.
- **Q5. Does archiving close the members-only KV store? — NO, unchanged**,
  for the same reason. It also keeps this change free of any `kv-channel-acl`
  dependency, which matters for the upstream half: that store is fork-only and
  will never be upstream, so making archive depend on it would have made the
  feature unupstreamable.
- **Q3. Ship fork-only, or wait on #351? — SHIP.** Done, at `@207`/`@208`.
  #351 stays open; if upstream adopts a different shape we merge it into the
  fork later and migrate.
- **Q4. Do a channel's threads go with it? — YES, and it is the APP's
  cascade.** `CHANNELS.md` §5.1: *"Its threads go with it."* The realtime
  server has no concept of a thread, so it cannot and must not do this.
  Recorded in `REVIEW-CHANNELS.md` so the Threads work has an owner before the
  app work starts.
- **Q6. Should the server protect the default channel? — NO. Removed.**
  "`#general` is never archived" is a SECO product rule, not a FOKS one: the
  server has no concept of a default channel, cannot read names to recognise
  one, and another app on this server may want to archive its first channel.
  The structural approximation it used (oldest public bottom-tier channel)
  guessed wrong whenever anything public was created first — protecting the
  wrong channel while leaving the real `#general` archivable. A rule enforced
  approximately is worse than one not enforced at all. librt refuses by name,
  which it can do exactly, and the server has no opinion. This also keeps a
  SECO policy out of a general-purpose server, which the upstream half needs.
- **Q7. Should a channel box carry an authenticated purpose? — LEAVE IT.**
  `RTBoxRG` carries a role and a generation and nothing about purpose, so the
  server cannot tell a description box from a name box. Pre-existing: channel
  creation has always had it. This change shrank the blast radius from "one
  bad box makes every channel in the team unlistable" to "one channel is
  hidden". The fix is a protocol change and belongs upstream on its own.

## 7. Test plan

`integration-tests/lib/`, following `rt_private_channel_test.go`. Actors: A
(team admin), B (ordinary member), C (admin of another team). The harness
needs no work — `tew.NewTestUser`, `tew.makeTeamForOwner`, `tm.makeChanges`
cover it (`rt_test.go:20-32`).

**Per-path negatives:** the rightmost column of §4, one test each.

**Invariants:**
- `TestArchiveIsReversible` — create, send N messages, archive, unarchive;
  thread reads return the same N messages with the same seqs, and per-member
  `read_through` is unchanged.
- `TestArchiveDoesNotBurnInboxVersions` — archive, then sync twice; the
  member's `user_inbox` version advances at most once and no `user_channels`
  rows are created (this is the fan-in regression, row 9).
- `TestRenameCasRace` — two concurrent renames; one succeeds, the other gets
  `RTRaceError` and succeeds on retry, and `seqno` lands at exactly +2.
- `TestRenameResealsAtTierRole` — rename an admin-tier channel as an admin and
  assert the resulting `name_box` role is `AdminRole`, not the caller's; and
  the bottom-tier mirror. **This is the security test of this change.**
- `TestRenameVisibleToOtherMembers` — B lists, A renames, B lists again and
  sees the new name. Asserts the `touchChannelSet` bump actually reaches a
  second client through the incremental delta, which is the whole delivery
  mechanism and is otherwise untested by single-actor tests.
- `TestArchivedPrivateStorageStillReachable` — pins the Q5 behaviour that
  archive leaves the members-only KV store open, so that if the product later
  decides otherwise it is a deliberate change with a failing test, not a
  silent discovery.

**Guards:** `rpcAccessClass` gains `rtUpdateChannel` and
`rtSetChannelArchived` (both `channelWrite`); `queryAllowlist` counts change
for `readAllChannels`, `readChangedChannels`, `findMissingChannels` and
`authorizeChannel`, plus new entries for the two mutation handlers. Both guard
tests fail until this is done, which is the point of them.

## 8. Implementation plan (server)

Each step compiles and tests green on its own.

0. **The two-line bug fixes** (§3.7) and **the `touchChannelSet` move from
   `acl.go` to `channels.go`** — a pure refactor, no behaviour change, callers
   unchanged. Both on their own commits, both upstreamable as-is, and the move
   is what lets everything after it be written against `upstream/main` (§9.3).
   **As built:** the move needed *no* `queryAllowlist` change. This document
   previously said one key had to be renamed; that was wrong. The guard keys
   on receiver-plus-function name, not on the file, so moving a function
   within the package is invisible to it.
1. **p9 + the column.** `archived_at TIMESTAMPTZ` on `channels`. Registered in
   **three** files or it silently never applies:
   `server/sql/patches/foks_realtime/p9.sql`; the `//go:embed` line **and** the
   `"foks_realtime"` map entry in `server/sql/embed.go`; the column **and**
   `INSERT INTO schema_patches ... VALUES (9, NOW())` in
   `server/sql/foks_realtime.sql`.
2. **Chokepoint + predicate.** `archivedAt` on `channelAuth` and
   `channelAuthCols`, the archived gate, `accessMutate`, and `notArchived()`.
   No behaviour change yet — nothing is ever archived.
3. **Filters.** Apply `notArchived` at rows 5, 7 and 9. Update `queryAllowlist`
   counts.
4. **Wire + status code.** `@207`, `@208`, `archived @22`,
   `RT_CHANNEL_ARCHIVED_ERROR @12008`, `make proto`, classify both RPCs.
5. **Handlers.** `UpdateChannel` and `SetChannelArchived` in `channels.go`,
   both reusing `touchChannelSet` and `wakeInboxPollers`, both inside
   `RetryTx2`.
6. **librt.** Extract the tier→nameRole helper (step 0 of the client work, and
   the one to review hardest), then `UpdateChannel` / `SetChannelArchived` and
   their agent RPCs.
7. **Tests**, per §7.

Then: tag a fork release, and the app work (`rtUpdateChannel` /
`rtSetChannelArchived` through gomobile and the native module) follows the
private-channel deploy order — **fork server, then daemon, then app**.

## 9. Proposing this upstream when it sits on fork-only work

This is the part to get right before writing code, because it is cheap to
design for and expensive to retrofit.

### 9.1 The problem, stated exactly

Rename and archive are upstream-legal features — they touch only tables and
concepts upstream already has (`channels`, `channel_sets`, `seqno`,
`updated_at_set_vers`). But the *fork's* natural implementation of them sits
squarely on top of private channels, which is fork-only and not going
upstream ("not on the roadmap" — Max, 2026-08-28).

Measured against `upstream/main`, `server/realtime/` is **+2076 / −168**.
Specifically, upstream has **no `acl.go`, no `pushhold.go` and no
`rt_inventory_test.go`**, and its `messages.go` still uses the old
`loadChannelForRead` + `AuthorizeUserForTeam` pair rather than the
`authorizeChannel` chokepoint.

So a PR written against fork `main` would reference `authorizeChannel`,
`touchChannelSet`, `privateVisibleToCaller`, `channelForkCols`,
`rpcAccessClass` and `queryAllowlist` — six symbols upstream does not have. It
would not apply, would not compile, and would ask Max to review a design he
has never seen. There is also a quieter cost: such a PR discloses the fork's
private-channel architecture into an upstream thread, where it invites
questions we do not want to spend the review on.

### 9.2 The rule

> **The upstream change must be expressible against `upstream/main`, and must
> depend on nothing fork-only. Develop it on a branch of `upstream/main`, not
> of fork `main`. Then merge it into the fork and add the integration
> separately.**

The test is mechanical and worth running literally: *does this diff apply to
`upstream/main` and compile there?* If not, it is not yet an upstream change.

### 9.3 What that means, piece by piece

| Piece | Upstream-legal? | What to do |
|---|---|---|
| `archived_at` column + p9 | **Yes** | Goes upstream unchanged. Upstream's patch number will differ from our p9 — expect that and do not fight it. |
| `touchChannelSet` | **Yes, but it lives in the wrong file.** Its body touches only `channel_sets` and `channels.updated_at_set_vers`; it sits in `acl.go` purely because grant/revoke needed it first. | **Move it to `channels.go` in the fork first, as a pure no-behaviour-change refactor.** One commit, callers unchanged, and — **as built** — no `queryAllowlist` change at all: the guard keys on receiver-plus-function name rather than on the file, so a move within the package is invisible to it. After that it is an ordinary `channels.go` helper the upstream PR can introduce where it belongs, and our merge conflict on it disappears. This single move is the highest-leverage thing on this list. |
| The archived gate | **Content yes, placement no.** Upstream has no chokepoint to put it in. | Write it as a standalone helper — `checkNotArchived(archivedAt, want)` for row paths and `notArchived(alias)` for set paths — so the *predicate* is shared and only the *call site* differs. Upstream calls it from `loadChannelForRead` and `lockChannel`; the fork calls it from `authorizeChannel`. That reduces the permanent fork delta to a handful of call lines instead of a redesign. |
| Listing / inbox / fan-in filters | **Yes** | Upstream's diff is `AND c.archived_at IS NULL`. The fork's sits beside the private gate. Small, predictable, recurring conflict — the cost of having a fork. |
| `rtUpdateChannel`, `rtSetChannelArchived` | **Yes** | See §9.4 — the numbers differ between the two versions. |
| `archived` on `RTChannelMetadata` | **Yes** | Same. |
| `RT_CHANNEL_ARCHIVED_ERROR` | **Yes** | Status codes are a shared enum; propose the number, do not assume it. |
| Inventory guard entries | **No** | Fork-only. Keep `rpcAccessClass` and `queryAllowlist` entirely out of the upstream PR; add them when merging into the fork. |
| Anything naming `channel_acl`, `private`, `no_push`, `push_holds` | **No** | Must not appear in the upstream PR at all — not in code, not in comments, not in test names. |

### 9.4 Numbering: four separate sequences, none of them aligned

Four sequences run independently — RPC methods, struct fields, SQL patch ids
and status codes — and the fork and upstream disagree on all four. Verified
against `upstream/main` on 2026-09-22, because the earlier draft of this
section got two of them wrong from memory, and the fourth row was missing
entirely until the upstream PR was opened (see below).

| Sequence | Upstream today | Fork today | This change proposes |
|---|---|---|---|
| RealTime RPC methods | `@0`–`@11` (`rtSetPushToken @11`) | `@0`–`@11` shared, fork block `@200`–`@206` | upstream **`@16`/`@17`**, fork **`@207`/`@208`** |
| `RTChannelMetadata` fields | `@0`–`@13` (**`noPush @13` merged**, #365) | upstream's plus `private @20`, `noPush @21` | upstream **`@14`**, fork **`@22`** |
| `foks_realtime` patch ids | p1–**p5**, where **p5 is no-push** | p1–p8, where **p5 is private channels** | upstream **p8**, fork **p9** |
| `lib.status` codes, RT block | `@12001`–**`@12009`** (`RT_MSG_QUEUED @12008`, `RT_OUTBOX_FULL @12009`, both #359) | `@12001`–**`@12008`**, where **`@12008` is `RT_CHANNEL_ARCHIVED_ERROR`** | upstream **`@12010`**, fork `@12008` **and it is wrong** |

Four things in that table are easy to get wrong and were:

- **Upstream's next free field is `@14`, not `@13`.** `noPush @13` merged
  upstream on 2026-09-12 (#365) at a different number from ours (`@21`);
  `SECO-UPSTREAM.md` records that divergence.
- **Upstream RPC `@12`–`@15` are already claimed by our own open PR** — #370,
  push holds, proposed at `@12`..`@15` and still unmerged. Proposing channel
  mutation at `@12` would collide with our own proposal, not with upstream's
  work. Hence `@16`/`@17`, with the caveat that if #370 is declined or
  renumbered these move down.
- **The fork's `RT_CHANNEL_ARCHIVED_ERROR @12008` is already taken upstream**,
  by `RT_MSG_QUEUED` from #359, merged before the fork allocated it. Our last
  upstream merge is `d39371c` (09-01), so `@12008` looked free here and was
  not; the same blind spot would have hit any status code we added in this
  window. It shipped in `v0.1.9-seco.21`. It costs nothing while our clients
  only talk to our server, and the upstream PR uses the genuinely free
  `@12010` — but the next upstream merge must renumber the fork's constant by
  hand and re-run `make proto`. Git will not raise a conflict, because the two
  names sit on different lines of the enum.
- **Patch ids already collide, and the failure is silent.** `server/shared/patch.go`
  keys a patch on a bare integer with no content hash. Upstream p5 adds
  `no_push`; the fork's p5 creates `channel_acl`. A fresh fork database stamps
  ids 1–8 as applied, so an upstream patch arriving later at any of those ids
  is skipped and its DDL never runs — no error, no warning. `SECO-UPSTREAM.md`
  records the rule this produced: **reconcile patch files by CONTENT, not by
  id, renumbering whichever side diverges.** Upstream's archive patch will not
  be our p9; expect to renumber one of them at the merge, and check it
  explicitly rather than trusting the id.

So carry both numberings, deliberately:

- **The upstream PR uses upstream's numbers.** **The fork ships `@207`,
  `@208`, field `@22`, patch p9** and keeps doing so until upstream merges.
- **The delta between the two is constants only.** Keep it that way. Once the
  two versions differ in logic as well as numbers, the merge stops being
  mechanical.
- **Add the row to `SECO-UPSTREAM.md` in the same change**, per `AGENTS.md` —
  including the number divergence above, because that is exactly the detail
  the next session will need and cannot re-derive.

If upstream accepts, the fork renumbers once and deployed clients need a
coordinated upgrade — which is why this stays a written-down delta rather than
something discovered at merge time. If upstream declines, as with private
channels, the fork keeps `@207`/`@208` indefinitely.

### 9.5 Recommended sequence

1. **Land the `touchChannelSet` move** in the fork on its own (§9.3). Trivial,
   reviewable in a minute, and it unblocks a clean upstream diff.
2. **Build the upstream-legal core on a branch of `upstream/main`**: column,
   helpers, filters, two RPCs at upstream numbers, tests that do not reference
   the guard files. Open it as the PR that answers #351. Per `AGENTS.md`:
   branch from `upstream/main` and never from our `main`; verify with
   `git diff --name-only upstream/main...HEAD` and read the list, because our
   `main` runs ahead with unmerged work a careless cut will drag in;
   `git rebase --signoff` for the DCO trailer; and expect a supersede-PR if
   review finds follow-ups, since maintainer pushes to our org-owned branch
   403 even with `maintainerCanModify`. **The PR must reference no fork-only
   document** — not `rt-private-channel-acl.md`, not `kv-channel-acl.md`, not
   this file — since those are dangling pointers to the reviewer.
3. **Merge that core into the fork**, renumber to `@207`/`@208`/`@22`, re-home
   the gate into `authorizeChannel`, add the guard classifications, and add
   the archive-versus-private interactions (§4 rows 5, 7, 12).
4. **Ship the fork release**; do not wait on the upstream review. The app has
   been blocked since 09-15 and #351 has been unanswered for a week.

Doing (2) before (3) is what prevents the problem the whole section is about.
Doing them in the other order — building in the fork and then trying to
extract an upstream PR from it — is how the private-channel design ends up in
an upstream diff by accident.

### 9.6 The residual risk, named

Upstream may implement archive with a *different shape* — a boolean instead of
a timestamp, or an archived channel that leaves the listing (freeing its name)
rather than staying in it (reserving it). The second of those is the one that
would hurt: it is a behaviour difference our app depends on, not a column
type. Then our merge is a data migration and a product change, not a
renumber. The
mitigation is already in flight: #351 proposes the shape in prose and asks for
agreement before either implementation exists. If Max answers with a different
shape, take his — the cost of matching upstream early is much lower than the
cost of diverging on a schema.

---

## Appendix — code references

Fork `main` @ `a31da0d`; line numbers from the 2026-09-22 read.

| What | Where |
|---|---|
| `seqno`, documented CAS, never incremented | `server/sql/foks_realtime.sql:70`; written at `server/realtime/channels.go:insertChannel` |
| Channel-set bump + stamp, reusable as-is | `server/realtime/acl.go:touchChannelSet` |
| Client discards cache and re-lists in full | `client/librt/minder.go:689` |
| The chokepoint and its access kinds | `server/realtime/acl.go:authorizeChannel` |
| Set-based predicate pattern | `server/realtime/acl.go:privateVisibleToCaller` |
| "Bump, don't stamp" inbox trick | `server/realtime/acl.go:dropChannelMember` |
| Listing query (archive filter goes here) | `server/realtime/channels.go:139` |
| Tier → name-role derivation to extract | `client/librt/minder.go:308-327`, sealed at `:353` |
| Client `#general` guard (create only) | `client/librt/minder.go:209` |
| RTRaceError retry loop to reuse | `client/librt/minder.go:MakeChannelWithTestHooks` |
| Field numbering rationale (`@20`, not `@64`) | `proto-src/rem/realtime.snowp`, comment above `private @20` |
| Fork RPC block `@200`–`@206` | `proto-src/rem/realtime.snowp:285-332` |
| Inventory guards | `server/realtime/rt_inventory_test.go` |
| Patch registration, three files | `server/sql/embed.go:71-92`, `server/sql/foks_realtime.sql` tail |
| Private-channel precedent spec | `docs/rt-private-channel-acl.md` |
