# A team system store Leaders cannot change (fork-only)

**Status:** built on `feat/daemon-system-store`, based on fork `main` at
`v0.1.9-seco.45`. Merged and tagged separately.
**Scope:** fork-only, like `kv-channel-acl.md`, which is the pattern this
mirrors. Not proposed upstream.
**Problem it solves:** FOKS checks KV writes by role, and roles are a total
order. A Sage (the SECO daemon's account) is an ADMIN and the team's creator is
OWNER, so anything a Sage can write, a Leader can write. The per-community
usage counters need a place a Leader can read but cannot edit.

## 1. The guarantee, in plain words

Each team can have one **system store**: a KV tree only Sage accounts can
change. Any member at the read role can read it.

It is a write lock and not a read restriction: the store is sealed with the
team's key at its read role, so every Leader holds the key. This is server
policy, not cryptography, the same level as every other FOKS write check. A
Leader could compute any ciphertext; only the server's refusal stops it.

A Leader can still remove Sage from the community or re-mint it. A newly minted
Sage continues the store its predecessor made, with no hand-over.

## 2. Why it is a second root, and not a folder

The server sees node IDs, never paths, and `loadExistingDirent` finds only the
client-supplied (ID, version-1) pair. Readers take the highest version per name
MAC across IDs, and `name_mac` has no unique key. So a Leader can write a
gapped version or a new dirent ID for any name in a directory they may write,
and a symlink at the name defeats a check on the entry too. A lock on one entry
in an ordinary tree cannot be enforced by inspecting that entry.

`system_root` holds one row per team: the store's root directory, with the same
key and binding fields as `root`, in a separate table so no upstream query of
`root` has to learn about it. It is insert-only. Because the store hangs from no
dirent, there is no name to shadow and no entry to remove: whatever a Leader
does in the ordinary tree, including creating a folder called `system`, cannot
touch it.

## 3. The lock, and its one chokepoint

`dir.daemon_locked` is one immutable boolean, and `lib.KVDir` carries it as the
trailing `daemonLock` so the same value travels on create and read. An
idempotent replay compares it like every other persisted field.

`authorizeLockedWrite` (`server/kv-store/system_store.go`) answers one
question: is the verified actor on the Sage list, in a team store? With no
actor it refuses. A remote member or a team acting as a member gives none. The
role checks still run beside it, so a Sage must also hold the write role in that
team.

| Path | Rule |
|---|---|
| `putDir` with `daemonLock` | caller must be a Sage; both locked and channel-tagged is refused |
| `putDirent`, per dirent, when the parent is locked | caller must be a Sage |
| `putDirent` whose value is a directory across the lock from its parent | refused for everyone |
| `putRoot` naming a locked directory | refused for everyone |
| `kvLockAcquire`, `kvLockRelease` on a locked directory | caller must be a Sage |
| `kvPutSystemRoot` | team party, ADMIN or above, caller must be a Sage |

Gating on the parent directory, by ID, makes forged versions, new IDs and
symlinks all the same refused write.

Links into the store are refused for everyone because Sage would otherwise be a
confused deputy: a Leader could point an ordinary dirent at the store's
directory, and Sage's next ordinary write would land in the store and pass the
gate. Only a locked parent, and `system_root`, can refer to a locked directory.
The rule runs in both directions: a locked directory may not refer to an
unlocked one, which would be a hole any Leader could write.

`kv_inventory_test.go` classifies every `nodeWrite` RPC by how it treats the
lock (`rpcLockClass`), so a new write path fails the build until someone states
it.

### Why no second users-database connection

`auth` already holds a `DbTypeUsers` connection for the length of a team
request. Acquiring a second one from the same pool while holding it is the
nested acquire that wedges a loaded server, so `authorizeLockedWrite` queries
through the connection `withSageList` left on the context. A request with no
verified actor never sets it.

## 4. Who is a Sage

`daemon_accounts(short_host_id, uid, ctime)` in `foks_users`, and
`multiuse_invite_codes.daemon_signup`. A signup with a flagged code inserts the
row in the signup transaction (`signupInviteCode`). An explicit list, rather
than deriving it from `multiuse_invite_code_users`, lets an operator add the
existing bots exactly, and revoke one.

The trust this rests on: the flagged code is known only to the daemon's
deployment secrets and the operator, and every bot's seed is sealed in the TEE.

## 5. The client

`lcl.KVConfig.systemStore`, honoured by get file and put file. Every other
operation refuses it in `initReq`, one place, so a new operation cannot quietly
ignore the flag and read a Leader's ordinary file of the same name. Those two
also refuse `mkdirP` and channel tags, and put file refuses large files: a large
file links its dirent after the first chunk, and a chunk is not gated by the
directory it ends up in.

A walk with the flag starts at the system root and creates it on first write: a
locked directory, then `kvPutSystemRoot`. A rival Sage can win that race; the
loser re-reads and carries on in the winner's store. The binding MAC is
verified as for an ordinary root.

The system root is immutable, so it has its own in-memory slot, separate from
`KVParty.root` and the on-disk root cache, and never goes through `VisitRoot`:
the request's version vector describes the ordinary root, and reporting this one
there would read as a stale cache on every retry. `checkVersionVector` skips
its root query when the vector has no root version, so a team with no ordinary
root still works.

Writes with the flag are not queued by the outbox: a counter write replayed
later would overwrite newer counts. The write fails and the caller knows it did
not land.

## 6. Tests

`integration-tests/lib/kv_system_store_test.go`, against a real server and
postgres. The raw-RPC half pins what the server accepts, since a Leader holds
the keys and need not go through libkv; the libkv half pins the client. Run it
with:

```bash
go test ./integration-tests/lib/ -run 'TestKvSystem|TestSignupWithDaemonCode' -count=1
```

`server/kv-store/system_store_test.go` pins the no-actor refusal without a
database, so `./server/...` carries it too. `TestRpcConnectGiveUpIsTransportError`
in `lib/core` covers the connect failure mapping below.

## 7. A dependency: failed connects are transport errors

`connectLoop` returned a raw `io.EOF` when it ran out of attempts. It comes back
from `Connect`, before `Call2`'s EOF mapping, and crosses the agent RPC as a
plain error, so anything classifying failures by `IsTransportError` read a
server that was down as a refusal. It is now a `ConnectError`. The daemon relies
on this: a metered read that fails on transport must fail open, and any other
failure must stop metered work.
