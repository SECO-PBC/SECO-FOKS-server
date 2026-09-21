# Private-channel storage via a KV-store ACL (fork-only)

**Status:** DESIGN — not built. Drafted 2026-09-21.
**Scope:** fork-only, same as `rt-private-channel-acl.md`. Not proposed upstream.
**Base:** fork `main` @ `640c806`. Line numbers from the 2026-09-21 read.
**Problem it solves:** a private channel today owns nothing but its message
stream. There is no place for it to keep settings, state, pinned documents or
attachments that other members of the same community cannot read.
**Relationship to `rt-private-channel-acl.md`:** that document gates *messages*
by `channel_acl`. This one extends the same ACL, the same chokepoint pattern and
the same guarantee statement to the *community team's KV store*. Read it first;
this document assumes its vocabulary (`channel_acl`, `accessKind`, the path
inventory discipline) and does not repeat it.

---

## 1. The guarantee, in plain words

Put this in the product copy, not just here.

Every private channel in a community stores its files in the **community team's**
KV store. They are locked with the **community's** key — the same key that every
member at that read role already holds. There is no separate lock per channel.

So what stops a member of private channel A from reading private channel B's
files?

**A tag on every stored item, and a bouncer at the door.**

Every directory and every file carries a tag naming the channel it belongs to.
Before the server hands over anything, it checks the tag. No tag: behave as
today. Tagged: is the caller on that channel's member list (`channel_acl`)? If
not, the server answers **exactly as if the item did not exist**.

The analogy: everyone in the building holds the same master key, but the guard
only lets you into the rooms on your list.

What that buys, and what it does not:

| Against | Protection |
|---|---|
| Outsiders (not in the community) | **Cryptographic**, unchanged. A leaked database is ciphertext they cannot open. |
| Community members not in the channel | **The server refusing to serve them.** Policy, not mathematics. |
| A revoked channel member who kept the ciphertext | Nothing. They hold the key forever; there is no rekey. |
| SECO, running the server with the daemon in the team | Nothing — by construction, same as §1 of the messages doc. |

**The consequence, stated once and honestly:** a bug in this policy exposes a
channel's entire storage, silently and retroactively, to any current or former
member of the *community* who kept their keys. The policy is the whole defence,
forever. That is why §5 is one chokepoint, §6 is an exhaustive path inventory,
and §9 makes an unclassified new path a build failure.

This is the same guarantee the shipped message ACL gives. It is not weaker. It
is also not stronger, and nobody should be told otherwise.

## 2. What the code actually allows (verified, not assumed)

Four findings from reading `server/kv-store/` shaped every decision below.

### 2.1 The server never sees paths. There is no subtree to gate.

Almost every KV RPC addresses a node **directly by ID**
(`proto-src/rem/kv.snowp:95-180`):

| RPC | Addressed by |
|---|---|
| `kvGetNode @10` | `KVNodeID` |
| `kvGetDir @12` | `DirID` |
| `kvGetEncryptedChunk @11` | `FileID` |
| `kvList @14` | `DirID` |
| `kvPutSmallFileOrSymlink @7` | `KVNodeID` |
| `kvFileUploadInit @3` / `kvFileUploadChunk @4` | `FileID` |

Only `kvGet @9` takes anything path-shaped, and it is `(parentDir, nameMAC)` —
one hop, not a path from the root.

The schema agrees (`server/sql/foks_kv_store.sql`). `dirent` maps **parent to
child** and there is no reverse pointer or index; `dir` and `large_file` carry
no parent column at all; and `dir_refcount` exists precisely because a
directory can have **several parents** (hardlinks), so "which subtree is this
in" is not a well-formed question.

**Therefore:** "gate everything under `/_chan/<id>/`" is not implementable. The
gate is **per node**, and every node carries a tag. §3 and §7.3 are consequences
of this paragraph.

### 2.2 There is already a per-node permission field of the right shape

`dir.read_role_type/_viz_level`, `large_file_key.read_role_*`,
`small_file_or_symlink.read_role_*`, `dirent.write_role_*` — and the server
already loads and checks them through `assertAtOrAbove`
(`server/kv-store/server.go:196`). A nullable `channel_id` beside those columns,
checked at the same call sites, is an addition to an existing mechanism rather
than a parallel one.

### 2.3 Per-channel VizLevels do NOT work (the shortcut that fails)

The tempting design is to give each private channel its own `VizLevel`: FOKS
teams already derive a PTK per role, so a channel would get key separation and
server gating for free, with no new team and no new ACL.

It fails. `Role` is `(RoleType, VizLevel)` and the comparison is a **total
order** (`proto/lib/extras.go:2738`):

```go
case tmp == 0 && typ == RoleType_MEMBER:
    return r.Member() >= r2.Member(), nil
```

VizLevels are a ladder, not compartments. A channel at viz 10 is readable by
everyone at viz 20. Mutually-isolated siblings are not expressible. Privacy must
therefore be a **second, orthogonal axis** — exactly the conclusion
`rt-private-channel-acl.md` §4.1 reached for `tier`, for a different reason.

### 2.4 The read gate was NOT uniform: large files had no server check

**Status (2026-09-21): fixed upstream in [#371](https://github.com/foks-proj/go-foks/pull/371), NOT yet in this fork.** The
table below is what the code looked like when this was written and is still
what `main` of this fork does — see §4, which is now a dependency rather than a
plan.

| Path | Server-side role check |
|---|---|
| `getDir` (`dir.go:257`) | yes |
| `listDir` (`dirent.go:466`) | yes |
| `loadDirentByID` (`dirent.go:155`, gates on the parent dir) | yes |
| small files / symlinks (`file.go:748`) | yes |
| `getRoot` (`root.go:58`) | yes |
| `getCurrentDirVersion` (`vv.go:44`) | yes |
| **`loadLargeFileMetadata` (`file.go:548-614`)** | **none** |
| **`getChunk` (`file.go:635`) → `BlobSQLStorage.Get` (`blob_sql_storage.go:21`)** | **none** |

`loadLargeFileMetadata` reads `read_role_type, read_role_viz_level` out of the
DB, copies them into the response, and never calls `assertAtOrAbove`. `getChunk`
accepts a `role` parameter and **never uses it**; the storage engine's `Get`
selects a chunk by file id with no permission predicate at all.

**Is that a live vulnerability today? No — and the doc should not claim it is.**
The key box is sealed to the PTK at the file's read role, so a caller below that
role gets ciphertext they cannot open, and file ids are 16 random bytes learned
only from a dirent they could already read. It is missing defence in depth, and
it leaks existence, size and chunk count to anyone who knows an id.

**Under this design it becomes a real hole**, because the key is the *team's*
PTK at a role every community member holds. Ciphertext served to a
non-channel-member is plaintext to them. §4 is the fix, and §4.3 is what
stands between this fork and having it.

**There are two routes in, not one.** `kvGetNode @10` reaches
`loadLargeFileMetadata` directly, and `kvGet @9` reaches it through
`getDirent` → `loadNode` when `follow` is set (`dirent.go:445`). Both land on
the same unchecked loader, which is why §4.1 fixes the loader rather than its
callers.

### 2.5 Two things that are not problems

- **Merkle.** KV merkle sub-trees are design only (`docs/kv_store.md` §Merkle
  Sub-trees); there is not one merkle reference in `server/kv-store/*.go`.
  Nothing to do now. When it lands it needs the same treatment — noted in §10.
- **Activity side channel.** `root_node_version` bumps only in `putRoot`
  (`root.go:144`), not on ordinary writes, so writes inside a channel's storage
  do not tick a party-wide counter that non-members can watch.

### 2.6 Cross-database access is already precedent

`ClientConn.auth` (`server/kv-store/server.go:125`) already opens
`DbTypeUsers` for `CheckTeamVOBearerToken` and then switches to
`m.KVShard(pid)`. Reading `channel_acl` from `DbTypeRealTime` is a third
connection in an existing pattern, not a new architecture.

**Verified, because the build depends on it.** `GlobalContext.dbPool`
(`server/shared/db.go:30`) creates pools lazily from `cfg.DbConfig(which)`, and
the config is one `db` block shared by every server process —
`conf/srv/foks.jsonnet:103` lists `foks_realtime` alongside the rest. So the
kv-store process can open it with no config change. Note the asymmetry: the KV
store is **sharded** (`db_kv_shards`, two shards in our config) while realtime
is a single database, so every tagged-node check crosses from a sharded pool to
an unsharded one. That is fine, and it is the reason the next paragraph's
caveat is not avoidable by tidier plumbing.

The real cost is the one `rt-private-channel-acl.md` §6.3 already names: **there
is no cross-database transaction.** A revoke committing between the ACL read and
the response lets that one in-flight request through. Every subsequent request
re-authorizes. Accepted, and stated here rather than discovered later.

## 3. Design: the tag is on the node, and it never changes

```sql
-- server/sql/patches/foks_kv_store/p1.sql   (first KV patch; see §8)
ALTER TABLE dir                   ADD COLUMN channel_id BIGINT NULL;
ALTER TABLE large_file            ADD COLUMN channel_id BIGINT NULL;
ALTER TABLE small_file_or_symlink ADD COLUMN channel_id BIGINT NULL;

CREATE INDEX dir_channel_idx
    ON dir(short_host_id, short_party_id, channel_id)
    WHERE channel_id IS NOT NULL;

-- One registered storage root per channel. This is the only place a tagged
-- node may hang beneath an untagged parent (see H3).
CREATE TABLE channel_kv_root (
    short_host_id  SMALLINT NOT NULL,
    short_party_id BYTEA    NOT NULL,
    channel_id     BIGINT   NOT NULL,
    dir_id         BYTEA    NOT NULL,
    ctime          TIMESTAMP NOT NULL,
    PRIMARY KEY (short_host_id, short_party_id, channel_id)
);
```

`NULL` means "not channel storage" — every existing row is correct with no
backfill, exactly as `channels.private DEFAULT false` was.

**On the type.** `BIGINT` here is `RTChannelIDShort`
(`proto-src/lib/realtime.snowp:27`), which is what `channel_acl.channel_id`
already stores (`server/sql/foks_realtime.sql:153`), so the ACL join needs no
conversion. The *wire* field in §8.2 is the full `RTChannelID`, a `Blob(16)`
(`realtime.snowp:28`); the server shortens it on the way in. Do not conflate
them — they are different widths with the same name in prose.

**Why `large_file` and not `large_file_key`:** `large_file` is the identity
table, one row per file, and `large_file_chunk` already carries a foreign key to
it. `large_file_key` is per *version*, so a rotation would have to carry the tag
forward — a second place to lose it. **Tag identity, not versions.**

## 4. The large-file read checks — done upstream, NOT yet in this fork

**This is a dependency, not a plan.** It shipped as
[foks-proj/go-foks#371](https://github.com/foks-proj/go-foks/pull/371), opened
2026-09-21 from `upstream-pr/kv-large-file-read-checks`, cut from
`upstream/main`. That branch is **not** this fork's `main`: `getChunk` on our
`main` is still the unchecked version. See §4.3 — nothing in §§5–9 is sound
until the fork carries these checks.

**4.1 `loadLargeFileMetadata`.** It reads `read_role_type` /
`read_role_viz_level` out of `large_file_key`, copies them into the response
and returns the key box; the check every sibling loader makes was missing.

**The check must run BEFORE the status switch, and that is not cosmetic.** A
review pass found the obvious placement — at the end, where the role has been
imported — still leaks: `KVUploadInProgressError` and `KVNoentError` are
returned from the switch above it, so a caller under the read role learns
whether a file it may not read is mid-upload, deleted or live, and can tell an
existing file from a missing one. As shipped, the role is imported into a local
immediately after the row loads and checked there; the key box is not decoded
for a caller that is about to be refused.

**4.2 `getChunk`.** It took a `role` and discarded it, and
`BlobSQLStorage.Get` selects a chunk by file ID with no permission predicate.
As shipped it resolves the file's read role through a new
`loadLargeFileReadRole` helper — one indexed read of the current
`large_file_key` row, without loading the key box — and checks it before
serving bytes. **This design adds the ACL check at the same point**, reading
`large_file.channel_id` (§3).

**Test both RPCs separately, and drive `kvGetEncryptedChunk` directly.**
`libkv`'s `GetFileChunk` loads the file's metadata before asking for a chunk,
so once 4.1 is in place the chunk RPC is unreachable through the client API: a
test written that way passes with 4.2's check removed and pins nothing. The
server must not depend on a client asking for metadata first. The same applies
to the ACL checks this design adds on top.

**Cost.** `MaxInputFileChunkSize` is 4 MB (`lib/kv/constants.go:11`), so a 1 GB
file is ~256 chunks and ~256 extra lookups across a full download, each a
single-row indexed read on an already-open pooled connection. The ACL check
adds a second, to a different database (§2.6).

### 4.3 The fork dependency — settle this before K1

`main` of this fork does not have #371. Building §§5–9 on it ships an ACL with
a hole under it: a community member who knows a file ID reads any tagged large
file's bytes, and the team PTK opens them. Pick one, and say which in the
`SECO-UPSTREAM.md` row:

- **Wait for #371 to merge** and arrive on the next upstream merge. Zero
  divergence, but the ACL work is blocked on maxtaco's queue.
- **Cherry-pick #371's commit onto the fork now** and let the merge drop it
  later, which is how `Upstreamed` rows already behave. Unblocks immediately;
  costs one temporary duplicate row in the tracker.

**DECIDED 2026-09-21 (Stefan): cherry-pick.** Done — `git cherry-pick -x
9ae9458` on `feat/kv-channel-acl`, all four large-file tests green against the
fork tree, and the #371 tracker row says the fork now carries the commit.

## 5. One chokepoint

```go
// server/kv-store/acl.go
//
// EVERY path that returns a dir row, a dirent row, file metadata, a key box,
// file bytes, or a version number for a node calls this. No path may answer
// from `dir`, `dirent`, `large_file*` or `small_file_or_symlink` for a caller
// without going through here.
//
//  1. if node.channel_id IS NULL -> return nil (unchanged behaviour)
//  2. require a channel_acl row for (channel_id, m.UID()) in the realtime DB,
//     OR team admin standing for the management kinds
//  3. on failure return the SAME error as a missing node (NotFoundError) --
//     existence is never disclosed
//
// The role gates in §2.2 still run, before and independently of this. The ACL
// narrows; it never widens.
func authorizeKVNode(m shared.MetaContext, rtdb *pgxpool.Conn,
    channelID *proto.RTChannelID, want accessKind) error
```

Mirrors `server/realtime/acl.go` deliberately: same name shape, same access
kinds, same "private check returns not-found" rule. One pattern, reviewed once.

## 6. Path inventory — the actual deliverable

Same discipline as `rt-private-channel-acl.md` §5. Anything that reads these
tables for a caller and is not in this table is a bug.

| # | Path | Returns | Gate today | ACL gate |
|---|---|---|---|---|
| 1 | `getDir` (`dir.go:246`) | dir row + seed box | read role | + ACL on `dir.channel_id` |
| 2 | `listDir` (`dirent.go:452`) | dirents | read role on dir | + ACL on the dir |
| 3 | `loadDirentByID` (`dirent.go:155`) | one dirent | read role on parent dir | + ACL on the parent dir |
| 4 | small files / symlinks (`file.go:748`) | key box + data box | read role | + ACL on `small_file_or_symlink.channel_id` |
| 5 | `loadLargeFileMetadata` (`file.go:548`) | key box | read role, **ahead of the status switch** (#371; not yet in this fork, §4.3) | + ACL on `large_file.channel_id` |
| 6 | `getChunk` (`file.go:635`) | file bytes | read role via `loadLargeFileReadRole` (#371; not yet in this fork, §4.3) | + ACL on `large_file.channel_id` |
| 7 | `putDirent` (`dirent.go:197`) | writes | write + read role on parent | + ACL + containment (H3) |
| 8 | `mkdir` (`dir.go:91`) | writes | write role | + ACL at creation (H3) |
| 9 | `putSmallFileOrSymlink` (`file.go:96`) | writes | read role on the box | + ACL at creation (H3) |
| 10 | `kvFileUploadInit` / `Chunk` (`file.go:325`) | writes | read role on md | + ACL at creation (H3) |
| 11 | `getCurrentDirVersion` (`vv.go:44`) | a version number | read role | + ACL — otherwise a non-member probes for a channel dir's existence and activity |
| 12 | `getCurrentDirentVersion` (`vv.go:50`) | a version number | **none of its own** | **none needed.** Reached only from `checkVersionVector`, which calls `getCurrentDirVersion` on the enclosing dir first (`vv.go:112`). It inherits row 11's gate, so fixing 11 covers it. |
| 13 | `getRoot` / `putRoot` (`root.go:58`, `:72`) | party root | read role / admin | none — the party root is never channel storage |
| 14 | `lockCheckPerms` (`lock.go:16`), for `kvLockAcquire @15` / `kvLockRelease @16` | lock state | loads the parent dir, requires `role >= dir.WriteRole` | + ACL on the parent dir — one site |
| 15 | `kvUsage @17` → `getUsage` (`usage.go:277`) | party-wide aggregate counts | **none** (no role check) | **none.** It returns one row of totals for the whole party with no per-node breakdown, so it discloses nothing about *which* channel holds what. See Q2. |
| 16 | **`kvGet @9` → `getDirent` (`dirent.go:396`)** | a dirent, and on `follow` the node itself | read role on the parent dir, via `loadDirent` | covered by rows 3 and 1/4/5 — **but it must be listed**, because `getDirent` calls `loadNode` on follow (`dirent.go:445`), which is a **second route into the §2.4 large-file hole** |
| 17 | `kvCacheCheck @13` (`server.go:386`) | nothing; validates a precondition | `checkVersionVector` | covered by rows 11–12 |
| 18 | `selectVHost @18` | — | n/a | n/a — no node data; inventory guard only |

**Every RPC in `proto-src/rem/kv.snowp` maps to a row above.** `kvMkdir @0`→8,
`kvPut @1`→7, `kvPutRoot @2`→13, `kvFileUploadInit @3`/`Chunk @4`→10,
`kvPutSmallFileOrSymlink @7`→9, `kvGetRoot @8`→13, `kvGet @9`→16,
`kvGetNode @10`→1/4/5, `kvGetEncryptedChunk @11`→6, `kvGetDir @12`→1,
`kvCacheCheck @13`→17, `kvList @14`→2, `kvLockAcquire @15`/`Release @16`→14,
`kvUsage @17`→15, `selectVHost @18`→18. Anything added to that file without a
row here is a bug, which §9.4 makes a build failure.

## 7. The four hard problems, solved

### H1 — Re-tagging

**As built, 2026-09-21.** The plan below assumed rotation existed and would
need to copy the tag forward. It does not: `server/kv-store` has no code that
writes a second `dir` version — `putDir` rejects any version but 1 — and
`channel_id` is written by exactly three INSERTs and never by an UPDATE. So
immutability is a property of the code's shape, not of a rule it follows.
`TestChannelTagIsWriteOnce` pins that shape, and is the thing that will tell
whoever implements rotation that it must carry the tag from the superseded
row rather than from the request. The replay comparisons in `putDir` and
`putSmallFileOrSymlink` weigh the tag alongside every other persisted field,
so resending a creation with a different tag is a conflict, not a replay.

**Threat.** A community member with write access relabels a node's `channel_id`,
moving data into a channel they are in, or out of one they are not.

**Solution: the tag is immutable and server-assigned.**

- Set exactly once, at node creation, from the creation RPC's argument.
- At creation the server requires the caller to hold a `channel_acl` row for
  that channel. An untagged node never becomes tagged and a tagged node never
  changes tag.
- **No RPC accepts a tag change.** The mutation does not exist to be authorized.
- On directory rotation, which writes a new `dir` row version, the server
  **copies `channel_id` from the previous version** and ignores any value in the
  request.

Moving a file between channels is copy-then-delete, performed by somebody who
is a member of both. That is explicit and auditable rather than a silent
relabel.

**Rejected alternative:** "only ACL owners and team admins may re-tag." It keeps
the tag mutable, which means every mutation path becomes another place to get
authorization wrong — the exact failure mode §1 says is unrecoverable.
Immutability deletes the class.

### H2 — `kvGetEncryptedChunk` has no directory context

**Threat.** The RPC receives a bare `FileID`. There is no parent, no path, and
(today) no check of any kind, so the subtree the file belongs to is unknowable
at the point of service.

**Solution: put the tag on the file's own identity row and look it up.**

`large_file` is one row per file, primary key
`(short_host_id, short_party_id, file_id)`, and `large_file_chunk` already
references it. `getChunk` resolves the tag with a single primary-key read before
serving any bytes — §4.2. No path reconstruction, no ancestry walk, no reliance
on the caller telling the truth about where the file lives.

### H3 — Orphan tagging (creation and linking are separate calls)

**Threat.** `kvMkdir` creates an unparented directory; it is linked into a
parent later by `kvPut`. Between the two there is no parent to inherit a tag
from, so a naive design leaves a window in which channel data sits untagged and
readable.

**Solution: tag at creation, verify containment at link. Both server-enforced.**

1. **At creation** (`kvMkdir`, `kvFileUploadInit`, `kvPutSmallFileOrSymlink`)
   the RPC carries the channel id and the server requires ACL membership *then*.
   The node is protected from the instant it exists. **There is no untagged
   window.**
2. **At link** (`kvPut`) the server enforces

   ```
   child.channel_id == parent.channel_id
   ```

   with exactly one exception: the child is the channel's registered root in
   `channel_kv_root`. `KVDirent` carries both `parentDir` and `value`
   (`proto-src/lib/kv.snowp:66`), so the server holds both sides of the edge and
   needs no ancestry index.
3. **The registered root** is created once per channel by an ACL owner or a team
   admin, through a new RPC (§8.2), and is the only tagged node permitted
   beneath an untagged parent.

This is stronger than trusting the client to keep a subtree consistent: the
containment invariant is checked on **every** link, by the server, with both
endpoints in hand.

**Hardlinks (§2.1) fall out correctly.** A directory with two parents is legal
only if both parents carry the same tag, because each link is checked
independently. A hardlink that would straddle two channels is refused at the
second link, not discovered later as a leak.

### H4 — It is policy, not mathematics

**Threat.** There is no technical fix. Everything above is a server that chooses
not to answer, protecting data sealed with a key the attacker already holds.

**Mitigation is honesty plus defence in depth:**

- **One chokepoint** (§5), and an inventory that enumerates every path (§6).
- **A build-breaking guard** (§9) so a path added in three years by somebody who
  never read this document fails CI until it is classified.
- **Product copy that says it** — §1's table, not buried in a design doc. A
  member must never believe their channel storage is hidden from community
  leaders when it is not.
- **Scope discipline:** never put anything in channel KV storage whose exposure
  to the whole community would be worse than exposure of the channel's messages.
  The two now carry identical guarantees, so that is a simple rule to hold.

**Forward compatibility is the real answer, and this design preserves it.**
Because the tag lives on the node rather than in the key hierarchy, a future
per-channel key changes only what seals `dir.seed_box` and the file key boxes.
The ACL, the tags, the containment rule and the chokepoint all stay as they are.
That upgrade turns §1's second row from policy into mathematics without a
redesign — which is exactly why VizLevels (§2.3) had to be rejected rather than
bent into service.

## 8. Wire and schema changes

### 8.1 Reads need no wire change

The server derives the tag from the node it was already loading. `kvGet`,
`kvGetNode`, `kvGetDir`, `kvList`, `kvGetEncryptedChunk` are **untouched on the
wire**. Only creation calls carry a channel id.

### 8.2 Creation calls and one new RPC

```
kvMkdir @0                 + channelID : Option(lib.RTChannelID)
kvPutSmallFileOrSymlink @7 + channelID : Option(lib.RTChannelID)
kvFileUploadInit @3        + channelID : Option(lib.RTChannelID)

kvChannelMkRoot @200 (auth, channelID, dirID)  -- registers channel_kv_root
```

**Field numbering:** append at the next free number in each struct — `@2` for
`kvMkdir`, `@3` for `kvPutSmallFileOrSymlink`, `@4` for `kvFileUploadInit`.
Sparse *field* numbers are not free: the codegen emits `codec:",toarray"`, so
every skipped number is a nil placeholder on every encoded record. That is the
lesson `rt-private-channel-acl.md` §4.4 paid for with a measurement (53 bytes of
padding from `private @64`); do not re-learn it. These are upstream structs, so
upstream may append at the same numbers — a collision fails loudly at merge,
because snowpc rejects duplicate field numbers.

**RPC numbering: `@200`, not `@19`.** Sparse RPC numbers cost nothing, and the
fork's convention is that fork-only methods start at `@200` so upstream can keep
appending after its own last number without colliding — see
`proto-src/rem/realtime.snowp:278` and `rtChannelGrant @200`. Upstream's KVStore
protocol currently ends at `selectVHost @18`, so `@19` is exactly the number
upstream takes next. Use `@200`.

### 8.3 The patch is the first one this database has ever had

`foks_kv_store` has **no** `patches/` directory and **no entry in the `Patches`
map** (`server/sql/embed.go:95-119`). Registration is three files, and missing
any one makes the patch silently never apply:

1. `server/sql/patches/foks_kv_store/p1.sql`
2. a `//go:embed` directive and a `var kvStorePatch1 string` in
   `server/sql/embed.go`
3. a **new top-level key** `"foks_kv_store": {1: kvStorePatch1}` in the
   `Patches` map

`server/sql/schema_patches_test.go` guards the base-schema/patch-id
correspondence; run it.

**Operational note — KV patches run per shard.** `PatchDBEng.loadShards` and
`getDBs` (`server/shared/patch.go:44-85`) fan out over `AllShards`, and each
shard keeps its own `schema_patches` table. A patch that applies to one shard
and fails on another leaves the deployment half-migrated with a green-looking
run. Confirm every shard before declaring the rollout done.

## 9. Test plan

Pattern: `integration-tests/lib/rt_private_channel_test.go`. Actors as in that
suite — A (ACL owner), B (granted member), C (community member, not in the
channel), D (team admin, not in the channel), E (removed from the team).

### 9.1 One negative test per path
Every row of §6: A and B succeed; **C gets the same not-found error as a
nonexistent node**; D gets not-found until an explicit self-grant; E gets
nothing.

### 9.2 The question §1 promises an answer to
`TestTwoPrivateChannelsCannotReadEachOther` — two private channels in one
community, each with storage. A member of A is refused every node of B across
all of §6, and vice versa. **This is the test that encodes the product claim**,
so it asserts the full path list, not a sample.

### 9.3 Invariant tests
- `TestKvTagIsImmutable` — every RPC that writes a node is tried against an
  existing tagged node with a different tag, and refused (H1).
- `TestKvTagSurvivesRotation` — a dir rotation preserves `channel_id` even when
  the client's request omits or contradicts it (H1).
- `TestKvContainmentEnforced` — linking a tagged child under a differently
  tagged or untagged parent fails, except the registered root (H3).
- `TestKvHardlinkAcrossChannelsRefused` — the second link is refused (H3).
- `TestKvOrphanNodeIsProtected` — a node created but never linked is already
  refused to non-members (H3).
- `TestKvLargeFileChunkDeniedToNonMember` — the §2.4 hole, asserted against
  **both** `kvGetNode` and `kvGetEncryptedChunk` (§4).

### 9.4 The inventory guard
`TestKvStoreRpcInventory`, modelled on `server/realtime/rt_inventory_test.go`
and living in `server/kv-store/` **for the same reason**: CI runs `./server/...`
but does not provision the postgres the integration suite needs, and a guard CI
does not run is not a guard. It parses `proto-src/rem/kv.snowp` for every
`name @N`, requires each to appear in an explicit access-class map, and
source-greps `server/kv-store/*.go` for queries against `dir`, `dirent`,
`large_file`, `large_file_chunk`, `small_file_or_symlink` outside an allowlist
of classified call sites.

## 10. Decisions — Q1 and Q4 RESOLVED 2026-09-21 (Stefan)

Q2, Q3 and Q5 do not block anything and can be settled while K2 is written.

| # | Question | Recommendation |
|---|---|---|
| Q1 | May team admins read a private channel's storage without joining? | **DECIDED: No.** An admin self-grants, visibly via `granted_by`, exactly as for messages (§6.1 of the messages doc). Storage is not a quieter back door. Consequence for §5: `authorizeKVNode` has no admin bypass for reads — admin standing matters only to the management kind (`kvChannelMkRoot`, and revocation via the existing realtime RPCs). |
| Q2 | Does `kvUsage @17` leak a channel's aggregate size to non-members? (§6 row 15) | **Answered by code: no new leak. Accept, change nothing.** `getUsage` (`usage.go:277`) returns five party-wide totals with no per-node breakdown, so it cannot attribute bytes to a channel. A member polling it can infer *that* the community stored something — already true today for admin-tier content they cannot read. Subtracting tagged nodes would cost a scan and buy nothing. |
| Q3 | Is the daemon granted channel storage when it is granted the channel? | Follows Q3 of the messages doc — not by default; surface it in the grant UI, because storage widens what its corpus ingests. |
| Q4 | Quota and garbage collection | **Mostly answered by code; one real gap.** GC is refcount-driven and channel-agnostic (`dir_refcount`, `large_file.refcount`, `small_file_or_symlink.refcount`, with the `*_mtime_gc_idx` partial indexes on `refcount = 0`), so a tagged node that is unlinked is collected exactly as an untagged one is — tagging does not interfere. The gap is **who unlinks it**: storage is charged to the community, and if a channel's last ACL member is revoked, nobody holds access to unlink the subtree and it lingers against the community's quota forever. The path already exists — a team admin self-grants (§6.1 of the messages doc, visibly via `granted_by`) and then deletes — but it must be an explicit step in channel deletion, not an assumption. **DECIDED: yes, deletion cascades to storage** — with the caveat, verified 2026-09-21, that FOKS has no channel deletion today (no delete/archive RPC in `proto-src/rem/realtime.snowp`, no `DELETE FROM channels` anywhere in `server/`). So this is a standing rule binding whichever change introduces deletion, not work in this plan: v1 ships no storage-delete path and `channel_kv_root` rows are written once and never removed. Whoever builds channel deletion owns the cascade, and this row is the reminder. |
| Q5 | Merkle sub-trees (§2.5) | When they land they must cover tagged nodes without disclosing their existence to non-members. Raise it with Max as a *design* input now, while it is still on paper and cheap to shape. |

## 11. Implementation plan

| Phase | Work | Est. |
|---|---|---|
| ~~K0~~ | ~~§4 — the two missing large-file read checks~~ **Done**, as [#371](https://github.com/foks-proj/go-foks/pull/371), from `upstream/main`. | — |
| ~~K0b~~ | ~~Carry #371 into this fork~~ **Done** — cherry-picked (§4.3) on `feat/kv-channel-acl`, tests green. | — |
| K1 | ~~Decide Q1 and Q4~~ (decided, §10); patch `p1.sql` with its three-file registration; proto + `kvChannelMkRoot @200`; codegen. `KvChannelMkRoot` lands as a `NotImplementedError` stub so the tree compiles; its authorization is K2's chokepoint. | 1 day |
| ~~K2~~ | **Done** (`server/kv-store/acl.go`). Read gate = `authorizeKVNodeRead` inside the five loaders every read funnels through (`loadDir`, `loadDirent`, `mLoadSmallFilesOrSymlinks`, `loadLargeFileMetadata`/`loadLargeFileReadRole`, `getCurrentDirVersion`), each masking a denial as its own missing-row answer, ahead of its role gate. Denials return `errKVNodeMasked`, a package sentinel, so infrastructure errors are never swallowed into "not found". The actor is the bearer token's signed member, carried by context from `auth()` — connections can be anonymous — and a team-as-member or remote member fails closed. `kvChannelMkRoot` landed here too (pulled from K3: its authorization IS the chokepoint's manage kind — ACL owner or team admin, channel private and of this team, dir already tagged; replays succeed, second roots refused). Read-gate tests exist now (`kv_channel_acl_test.go`), with tagged rows planted by SQL so the gate is pinned independently of K3; an 8-mutation matrix (each loader, the actor plumbing, the manage gate) fails exactly one probe each. Found and fixed in passing: `KvGetNode` on any absent small-file/symlink ID was a remotely triggered server panic (`loadSmallFileOrSymlink` returned a nil entry that `loadNode` dereferences) — upstream bug, see the tracker. | — |
| ~~K3~~ | **Done.** Creation tagging on all three creation RPCs, gated by `authorizeKVNodeCreate` (membership, so H3 holds: a node is tagged at the instant it exists and a non-member cannot mint one). Containment enforced in `direntUpdater.checkContainment` on every link, both directions, with the registered root as the single crossing. `channel_kv_root` written by `kvChannelMkRoot` (shipped in K2). **H1 came out simpler than planned**: there is no rotation path in `server/kv-store` at all — `putDir` rejects any version but 1, and `channel_id` is written by exactly three INSERTs and never by an UPDATE — so the tag is immutable by construction rather than by copy-forward logic. `TestChannelTagIsWriteOnce` (no DB, runs in CI) fails the build if that stops being true, which is the message to whoever implements rotation. Both replay comparisons weigh the tag, so resending a creation cannot relabel a node. Eight-mutation matrix, each caught. | — |
| ~~K4~~ | **Done**, alongside K2/K3 rather than after. `kv_channel_acl_test.go` (read gate, mkroot standing) and `kv_channel_create_test.go` (creation membership, orphan protection, containment both ways + root crossing, hardlink refusal, tag immutability on replay for both small files and directories). The K3 tests drive the raw RPCs: libkv does not carry a channel tag yet, and the wire is what the guarantee actually rests on. | — |
| K5 | `TestKvStoreRpcInventory` (§9.4) | ½ day |
| K6 | libkv/librt wrappers; fork PR; tag; app pins | 1 day |

**`SECO-UPSTREAM.md` is not optional (AGENTS.md, "the one rule").** Two rows,
each written in the change that makes it true, not afterwards:

- **K0** → done: the `Proposed` row for [#371](https://github.com/foks-proj/go-foks/pull/371) is in the tracker (on
  `docs/kv-channel-acl`). If K0b is taken by cherry-pick rather than by waiting,
  the row must say so, because the fork will then carry a commit that is also
  in flight upstream.
- **This design** → a `Local` row **with the reason**: it depends on
  `channel_acl`, which is fork-only and which Max has said is not on the
  roadmap. That keeps it decided instead of re-litigated.

Client work — the bridge surface, how a channel's storage appears in the UI, and
what it is actually used for — is a follow-up spec and not on this critical path.

## 12. What needs Max, and what does not

**Answered here; do not spend his time:**
- *Can VizLevels express this?* — no, settled by `IsAtOrAbove` (§2.3).
- *Is a path-based ACL possible?* — no, settled by the RPC surface and schema
  (§2.1).
- *Is a fork-only column on a shared table safe?* — our call; nullable with no
  backfill, same shape as `channels.private`.

**Sent, 2026-09-21:** §2.4 went upstream as
[#371](https://github.com/foks-proj/go-foks/pull/371) rather than as a question
— a fix with tests reads better than a bug report, and it stands on its own
merits independently of this fork-only design. Framed there as a missing check
rather than a data leak, since confidentiality does hold while the key is
per-role. Open; §4.3 says what to do while it is.

**Genuinely only-Max:** Q5 (merkle sub-trees over tagged nodes), and the
standing per-channel-key question that would turn §1's policy row into a
cryptographic one.
