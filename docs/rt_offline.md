# RT Offline Mode

Design and implementation plan for offline support in the realtime (RT) chat
system: reading cached state with no connectivity, and queueing sends that
drain when connectivity returns.

This builds directly on the architecture in `chat-server-design.md`. The
inbox-versioning design there already anticipates the offline *read* case
("mainly used when the app is coming into the foreground, or when it's started
up after a period of being offline"), and much of it is built. This document
specifies the remaining read-side behavior and the entire write side, which is
currently a stub.

## Goals

- **Offline reads**: a client with no connectivity renders its inbox and any
  cached thread content, clearly distinguishable from fresh state. Partial
  cache coverage degrades to "what we have, flagged stale" rather than an
  error.
- **Offline writes**: a send attempted without connectivity is queued durably
  and drained on reconnect, exactly once, in per-channel order, surviving app
  restarts and crashes at any point in the pipeline.
- **Offline read-marks**: read-through positions advance locally offline and
  replay on reconnect.
- **Ad-hoc team compatibility**: the design accounts for ad-hoc teams' lazy
  PTK rotation (`chat-server-design.md`, Team Topology), even though Stage 1
  servers don't serve them yet.

## Non-Goals

- A general connectivity framework for the whole client (KV store, sigchain
  fetches, etc.). This is scoped to `client/librt` plus one server-side
  change. The connectivity probe below is deliberately minimal and RT-local.
- Conflict resolution beyond ordering. RT messages are append-only; there is
  nothing to merge.
- Offline channel *creation*. `MakeChannel` requires the server; queueing
  channel creation pulls in name-reservation races for marginal benefit.
- Attachment queueing (attachments are Stage 2b and live in the KV store).

## Current State

What exists today, verified against the code:

**Read side — largely built.**

- Inbox sync state (cursor + channel index) persists to local soft SQLite,
  pages applied atomically, crash-safe resume (`client/librt/inbox.go`).
  Ciphertext stays boxed at rest; decryption happens at render.
- Thread messages cache locally (`DataType_RTThreadMsgData`), and
  `GetThreadBookended` is cache-first: it computes holes and asks the server
  only for the bookends and gaps. A fully-cached range costs zero RPCs.
- `since=0` means full sync, inbox versions are monotonic, and the server
  never prunes `messages_enc`/`user_channels`/`user_inbox` — so a stored
  cursor cannot go stale today (see "Retention guard" below).

**Write side — a stub.**

- `Send` writes the sealed message to a local outbox row
  (`DataType_RTOutboxMsg`) *before* issuing `RtSend` — but nothing ever reads
  or deletes that row. There is no drain loop, no retry, and successful sends
  leave the row behind as an orphan. If `RtSend` fails, the error propagates
  to the caller and the queued row is dead weight.
- `ReadThrough` is a bare RPC with no local persistence.

**Server side — idempotency is half-built.**

- `messages_enc` has `UNIQUE(msg_id)` on the client-assigned 16-byte random
  message ID, so a retried send carrying the same `msgID` cannot double-insert.
  But `insertMessage` only special-cases the `messages_enc_pkey` conflict; a
  `msg_id` conflict surfaces as a generic database error the client cannot
  distinguish from failure — and the response does not carry the seq the
  message actually got.
- Prev-pointers are stored, never validated. The protocol comment on
  `RTMsgMetadata` already blesses `prevSeq`/`prevID` disagreeing ("the user is
  typing two messages rapidly"), which is exactly the queued-send situation.
  The only optimistic check is the client-opt-in `expectedPrevSeq`.

## Design

### D1 — Idempotent replay on the server: duplicate `msg_id` returns the original result

`RtSend` becomes idempotent on `msgID`. When the insert hits the
`UNIQUE(msg_id)` constraint, the server loads the existing row and — after
verifying it matches the caller (same channel, same sender) — returns a
normal `RTSendRes` carrying the original seq and insert time, plus a `replay`
flag so clients and metrics can tell.

```
struct RTSendRes {
    seq @0 : lib.RTMsgSeq;
    insertTime @1 : lib.Time;
    replay @2 : Bool;   // true if this msgID was already delivered
}
```

If the existing row does *not* match the caller (different channel or
sender — either a client bug or a stray collision in a 16-byte random space),
the server returns a new status code `RT_MSG_REPLAY_MISMATCH` rather than
silently acking, wired through `status.snowp` and `lib/core/errors.go` in the
usual pattern.

Rationale for returning success rather than an error on a clean replay: the
client's question during a drain is "did my earlier attempt land?"; making the
common answer an error forces every drain implementation to catch and rewrite
it. True idempotency — same call, same result — is the simpler contract.

Adding a trailing field to `RTSendRes` is wire-compatible for snowpack
struct decoding; old clients ignore it.

### D2 — Outbox lifecycle: queue → drain → ack → delete

The existing `dbPutMsgToOutbox` write becomes the head of a real pipeline:

1. **Queue.** `Send` seals the message, assigns `msgID`, and writes the
   outbox row (as today), keyed by channel with the send time as index.
   State advances to *queued*.
2. **Attempt.** The drain loop issues `RtSend`. Three outcomes:
   - **Ack** (including `replay=true`): write the message into the thread
     cache (as `Send` does today), then delete the outbox row. Delete strictly
     after the thread-cache put, so a crash between the two re-sends and hits
     the replay path — at-least-once attempts, exactly-once delivery.
   - **Transport error** (connection refused, timeout, TLS, DNS): leave the
     row queued; back off and retry. This is the offline case.
   - **Semantic rejection** (auth failure, channel gone, permanent server
     error): mark the row *failed* with the error, stop retrying it, surface
     to the caller/UI. Failed rows block nothing behind them (see ordering
     below) but are retained for the user to inspect/discard.
3. **Drain triggers.** On client start, on any successful RT RPC (proof of
   connectivity), on a send into any channel, and on a periodic timer with
   exponential backoff while rows are queued.

**Ordering.** Per-channel FIFO by queue time. The drain sends one message per
channel at a time and does not advance past a row still in *queued* state —
otherwise a flaky link reorders a conversation. Distinct channels drain
concurrently. A *failed* row is the exception: it steps aside so the rest of
the channel isn't held hostage by one rejected message.

**Restart recovery.** On start, every outbox row is either queued (drain will
retry; replay protection makes this safe regardless of whether the original
attempt landed) or failed (left for the user). The orphan rows today's code
leaves behind are cleaned by the same sweep: any row whose `msgID` already
appears in the thread cache is a completed send — delete it.

**`Send` return semantics.** Today `Send` is synchronous: RPC result or
error. It stays synchronous when connectivity is up. When the RPC fails with
a transport error, `Send` returns a new `RTMsgQueuedError` (status code
`RT_MSG_QUEUED`) carrying the `msgID` — not success, because the message has
no seq yet and callers must not assume ordering; not a plain failure, because
the message *will* be delivered. Callers that don't care can treat it as
soft-success; interactive clients use the `msgID` to render the pending
message and reconcile when the drain acks it.

### D3 — Prev-pointers on drained sends: already answered, one client fix

A message queued offline carries `prevSeq`/`prevID` frozen at queue time,
stale by the time it drains. No protocol change needed: the server stores
these unvalidated, and the `RTMsgMetadata` comment explicitly allows them to
disagree with reality. Queued sends never set `expectedPrevSeq` (it exists
for callers that *want* CAS semantics; a drain wants the opposite).

One client change: `SendWithTestHooks` currently errors (`RTMsgOrderError`)
if the acked seq is ≤ the last locally-seen seq. For a drained message that
check compares against a prev captured at queue time, which is exactly the
case the protocol permits — the drain path skips it. (The check remains for
the synchronous path, where it still catches genuine server misbehavior.)

### D4 — Read-side degradation: serve what we have, say that we did

`GetThreadBookended`, `GetThreadRecentMsgs`, and the inbox load each get a
uniform degraded mode: when the server RPC fails with a transport error *and*
the local cache has any relevant rows, return the cached subset with a
staleness indicator instead of the error.

Concretely, thread reads return a result carrying:

- the messages found in cache (holes and truncated bookends included),
- `stale: bool` — true when the server was needed and unreachable,
- the underlying transport error, for callers that want to distinguish
  "offline" from "healthy but empty".

An empty cache plus an unreachable server still returns the error — an empty
thread render must never masquerade as truth. Pending outbox messages for the
channel are appended to thread reads (flagged as unacked) so the sender sees
their own queued messages.

The inbox path needs no protocol work — a failed `RtGetChangedThreads` leaves
the persisted snapshot in place; the change is to return that snapshot with
`stale=true` rather than propagating the error.

### D5 — Offline read-marks: last-writer-wins replay

`ReadThrough` gets the same treatment as sends, but simpler because
read-marks are idempotent and monotonic server-side (a stale mark is ignored,
never an error):

- The client persists the highest locally-read seq per channel
  (`DataType_RTReadThroughPending`, soft state).
- On transport failure the mark is queued; the drain replays only the
  highest pending seq per channel (intermediate marks are subsumed).
- On ack the pending row is deleted.

No ordering interaction with the message outbox; the two queues are
independent.

### D6 — Sealing time and ad-hoc teams

A queued message is sealed at *queue* time with the then-current PTK (that is
what the outbox stores today — the boxed wrapper, consistent with the rule
that plaintext never hits the local DB). Two cases:

**Regular teams.** PTK rotation happens on membership change. A message
sealed to gen N and drained after a rotation to N+1 is accepted (the server
stores `ptk_gen` per row and never validates it) and remains readable by
current members, who hold all prior gens. The one wrinkle: a member removed
during the offline window can still read a message sealed before their
removal. This matches the security semantics of a message *sent* before the
removal — which, from the sender's point of view, it was. Documented as
accepted behavior.

**Ad-hoc teams.** These rotate lazily *before message send* precisely so that
removals take effect at the next send. A drained message sealed pre-rotation
would silently skip that guarantee. So the drain performs the
rotate-if-needed check at *drain* time, exactly as a live send would, and if
the PTK has advanced past the sealed gen, the client **re-seals** before
sending: unbox with the stored gen (the sender holds it), re-box with the
current gen, regenerate the box under the same `msgID` and metadata. `msgID`
is assigned at queue time and never changes across re-seals, so replay
protection is unaffected.

Re-sealing is the only part of this design that touches message crypto, and
it is confined to the ad-hoc drain path. Stage 1 servers don't serve ad-hoc
teams, so it can land as a later phase — but the outbox row format carries the
sealed gen from day one so no migration is needed.

### D7 — Connectivity classification, not a connectivity service

The design deliberately avoids a reachability oracle. "Offline" is defined
operationally: an RPC failed with a transport-class error. A helper
(`core.IsTransportError` or similar, shared with the drain loop) classifies
errors into transport vs. semantic; everything else in this document keys off
that classification plus the drain triggers in D2. If the platform later
grows a real connectivity signal, it becomes one more drain trigger — nothing
else changes.

### Retention guard (forward-compatibility note)

The offline read design assumes inbox cursors never go stale, which holds
because nothing prunes RT tables. If message retention/expiry is ever added,
the server must also add a "cursor too old" signal on
`RtGetChangedThreads` (e.g. a minimum-supported inbox version in the
response) so clients fall back to a full resync instead of silently missing
deletions. Recording the requirement here so retention work inherits it.

## Implementation Plan

Phased so each lands independently, in the staged-build spirit of
`chat-server-design.md` (this is effectively Stage 1f, before push work in
Stage 2a — the drain triggers are also where background-push wakeups will
eventually hook in).

### Phase 1 — Server: idempotent replay (small, unblocks everything)

1. Name the unique constraint (`messages_enc_msg_id_key`) explicitly in a
   schema patch so the error check isn't tied to an auto-generated name.
2. In `insertMessage`, catch the `msg_id` duplicate, load the existing row,
   verify channel + sender, return `RTSendRes{.., replay=true}` or
   `RT_MSG_REPLAY_MISMATCH`.
3. New status codes `RT_MSG_REPLAY_MISMATCH` and `RT_MSG_QUEUED` (the latter
   is client-generated but lives in the shared status space) via
   `status.snowp` + `lib/core/errors.go`; regenerate proto.
4. Tests: same `msgID` twice → same seq, `replay=true`, no second row; same
   `msgID` on a different channel → mismatch error; concurrent duplicate
   sends → one row, both callers get the same seq.

### Phase 2 — Client: outbox drain (the core)

1. Outbox row gains a small state envelope (queued/failed + attempt count +
   last error) — new `lcl` struct wrapping today's `RTMsgCached`.
2. Drain loop in `Minder`: per-channel FIFO, transport/semantic
   classification, backoff, triggers per D2.
3. `Send`: on transport error, leave the row queued and return
   `RTMsgQueuedError{msgID}`; on ack (fresh or replay), thread-cache put then
   outbox delete — fixing the orphan-row leak as a side effect.
4. Startup sweep: reconcile outbox against thread cache, resume queued rows.
5. Tests (`minder_test.go` + test hooks): kill the transport mid-send and
   verify replay convergence; crash between ack and delete; ordering under a
   flaky link; failed row doesn't block its channel.

### Phase 3 — Client: read-side degradation + read-marks

1. Stale-tolerant returns for thread reads and inbox load (D4), including
   pending-outbox overlay on thread views.
2. Pending read-through persistence and replay (D5).
3. `rt` CLI: surface staleness and queued counts (e.g. `rt inbox` marks
   stale, `rt outbox ls` lists queued/failed).

### Phase 4 — Ad-hoc drain path (when ad-hoc channels exist end-to-end)

1. Rotate-if-needed at drain time; re-seal on gen advance (D6).
2. Tests: queue → rotate (member removal) → drain re-seals with new gen;
   `msgID` stable across re-seal.

## Open Questions

- Should the server *validate* `ptk_gen` against the team's current
  generation window (rejecting gens that predate a removal by more than the
  lazy-rotation contract allows), or is storing it opaquely — the current
  behavior — the intended trust model? The design above assumes the latter.
- Backoff parameters and outbox caps (max queued messages per channel, max
  age before a queued row is declared failed) — proposed defaults: cap 256
  rows/channel, no age limit, backoff 1s→2m with jitter. Worth maintainer
  input before hard-coding.
- Whether `RTSendRes.replay` should also be surfaced in `rt send` CLI output
  or kept internal.
