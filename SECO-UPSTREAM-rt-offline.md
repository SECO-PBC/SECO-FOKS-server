# Upstream proposal: RT offline mode

Fork-local. This is the text we take upstream, not something upstream ever
sees as a file. Tracker rows live in `SECO-UPSTREAM.md`; the design doc
(`docs/rt_offline.md`) **does** go upstream, with PR 2.

Work sits on `docs/rt-offline-mode` (14 commits off our `main`). Before any
PR: rebase onto `upstream/main`, `git rebase --signoff`, split by the landing
order below.

---

## Landing order and why

Three PRs, sequential. Each is reviewable alone; each later one depends on the
behaviour the earlier one defines.

1. **`rtSend` idempotent on `msg_id`** — server, ~300 lines with tests. A
   defect fix in its own right, independent of anything offline.
2. **RT offline mode** — client `librt` + design doc. Depends on 1.
3. **Offline cold-start bootstrap** — client `libclient`/`lib/chains`. Depends
   on 2, and touches shared loaders, so it goes last and invites the most
   discussion. Also carries `verifiedAt` and the offline-reads design doc.

Since amended: a fourth PR, [#347](https://github.com/foks-proj/go-foks/pull/347)
(transport classification), was split out of 2 and opened first. It is
upstream of the series rather than a new step in it.

Opening 2 before 1 lands would ask a reviewer to judge a drain loop against
replay semantics that aren't merged yet.

---

## PR 1 — `rtSend` idempotent on `msg_id`

### Problem

`messages_enc` already carries `UNIQUE(msg_id)` on the client-assigned 16-byte
random ID. But `insertMessage` only special-cases the `messages_enc_pkey`
conflict; a `msg_id` conflict falls through to the generic error branch and
reaches the client as a raw Postgres error. So a client whose ack was lost has
no way to ask "did my earlier send land?" — the one question a retry needs
answered. The constraint exists; the behaviour on hitting it doesn't.

### Change

On a `msg_id` unique violation, resolve the duplicate and return the original
result:

- The violation aborts the enclosing transaction (`pgconn.SafeToRetry` is
  false for constraint violations, so `RetryTx2` correctly doesn't retry), so
  the lookup runs after rollback as a fresh read. Safe without a lock —
  `messages_enc` rows are immutable once committed.
- The existing row is verified against the caller on **host, channel, and
  sender** before anything is acked. `UNIQUE(msg_id)` is global, spanning
  channels and vhosts; without the host/channel check an ack could be minted
  against an unrelated row, and without the sender check one channel member
  could replay another's message ID and treat the ack as their own delivery.
- A match returns a normal `RTSendRes` with the original seq and insert time.
  A mismatch returns a new payload-free `RT_MSG_REPLAY_MISMATCH` — bare on
  purpose, so the response leaks nothing about the row it didn't match.
- `expectedPrevSeq` callers get the same contract: a failed CAS checks for an
  already-landed `msg_id` first, so a retry of a landed send resolves as a
  replay instead of looping on `RTRaceError` forever.

### Two deliberate non-changes

**No wire change.** We first drafted a `replay bool` on `RTSendRes` and then
dropped it. Snowpack structs encode as positional msgpack arrays
(`codec:",toarray"` with per-field pointers), which makes a *new* decoder
reading an *old* shorter array safe, but leaves an *old* client decoding a
server's *grown* array unproven. `rtSend` responses flow server→client — the
risky direction — and a chat server shouldn't need flag-day client upgrades.
The only caller that wants the distinction is a drain loop, which already
knows it's retrying. If a replay indicator ever earns its place (metrics,
CLI), `rtSendV2` is the honest way to add it.

**The constraint is named, not added.** The base schema now spells
`CONSTRAINT messages_enc_msg_id_key UNIQUE(msg_id)` because the name is
load-bearing (`IsDuplicateKeyError` matches on it). It is byte-identical to
the name Postgres already auto-generates, so existing deployments need no
patch and fresh installs land in the same place — we checked that p1–p4 never
touch `messages_enc`.

### Why a replay returns success, not an error

The client's question during a retry is "did my earlier attempt land?".
Answering the common case with an error forces every drain implementation to
catch and rewrite it. Same call, same result is the simpler contract.

### Tests

`TestRTSendIdempotentReplay`: same `msgID` twice → same seq and insert time,
one row; same `msgID` on a different channel or from a different sender →
mismatch error, nothing inserted.

---

## PR 2 — RT offline mode (client)

### Framing

This is a continuation of `chat-server-design.md`, not a departure from it.
That document already anticipates the offline read case — `get_changed_threads`
is "mainly used when the app is coming into the foreground, or when it's
started up after a period of being offline" — and client retries are
anticipated in the schema notes (`insertTime` "might be later than sentAtTime
if retries were needed"). The prev-pointer comment on `RTMsgMetadata` already
blesses `prevSeq`/`prevID` disagreeing "if the user is typing two messages
rapidly", which is exactly a queued send.

Two things here are genuinely new rather than derived, and we call them out
rather than smuggle them in:

1. **A client-side durable outbox.** The only outbox in the original design is
   the server's `push_outbox` for notifications. Nothing there argues against
   a client one; it simply isn't described.
2. **Defined duplicate-send semantics** — that's PR 1.

### What it fixes on the way

The current client outbox is a stub with two live defects: `Send` writes a row
to `DataType_RTOutboxMsg` that nothing ever reads or deletes (so every
successful send leaks an orphan), and the row is keyed by channel with a
millisecond-resolution index, so two sends into one channel in the same
millisecond silently overwrite each other. Separately, `GetThreadView`'s
read-mark advance is dropped with a warning on failure, so offline reading
loses read state entirely.

### What it adds

- **Durable outbox**: queue → drain → ack → delete. Rows are keyed by `msgID`
  under a new DataType, sealed before the first network attempt, deleted only
  after the ack reaches the thread cache. Crash between those two → the retry
  converges via PR 1's replay. Per-channel FIFO; distinct channels drain
  concurrently; a semantically rejected row steps aside as Failed so one
  poisoned message can't hold a channel hostage; per-channel cap refuses
  rather than sheds.
- **No queue-jumping**: a fresh send into a channel with queued rows flushes
  the channel in order rather than racing past it.
- **Degraded reads**: thread reads return a result struct carrying `stale` and
  the transport error; a fully-cached range still costs zero RPCs, a partial
  one serves what's cached flagged stale, and an *empty* cache still errors —
  an empty render must never masquerade as truth.
- **Offline read-marks**: persisted per channel, max-merged, replayed on the
  drain; local unread counts consult them.
- **CLI**: `rt outbox {ls,drain,retry,discard}`, offline banners on
  `rt read`/`rt inbox`, and `rt send` reporting a queued message rather than
  failing.
- ~~**Transport-vs-semantic classification** (`core.IsTransportError`)~~ —
  **split out and opened separately as
  [#347](https://github.com/foks-proj/go-foks/pull/347)** on 2026-09-02. It
  fixes a live inconsistency of its own (a timeout and a refused dial were
  treated differently in the same outage), so it did not need to wait behind
  this PR, and removing it makes this one smaller. This PR now *depends* on
  #347 rather than containing it.

### Ad-hoc teams

Handled, though Stage 1 servers don't serve them yet. Ad-hoc teams rotate
lazily *before send* precisely so removals take effect at the next send. A
message queued offline and drained later would silently skip that guarantee,
so the drain performs the rotate-if-needed check at drain time and re-seals
under the current generation if it advanced. `msgID` is assigned at queue time
and survives the re-seal, so replay protection is unaffected. The outbox row
carries the sealed generation from day one, so no migration is needed when
ad-hoc channels arrive.

### Trigger coverage — stated plainly

Implemented drain triggers: a send into a channel with queued rows, a
successful inbox sync (which covers app start and foreground cadence, since
apps sync on both), and the explicit CLI drain/retry. **Not** implemented: a
periodic backoff timer and a real connectivity signal — those belong at the
app layer, and there's no standing poll loop in `librt` today to hang them
off. `PollInbox` is a single long-poll call; when a standing loop lands, its
first success after a gap is another natural trigger.

### Tests

`TestRTOfflineNoHooks` drives the whole arc — queue, stale read with pending
overlay, offline read-mark, reconnect, drain, server-confirmed delivery —
through the **real network layer** using the repo's own
`CatastrophicNetworkConditions`, which fails at connect, upstream of every
RPC. No test hooks. We verified it has teeth: reverting the channel-resolution
cache fallback makes it fail with the exact raw connect error. Plus targeted
tests for lost-ack replay convergence, no-queue-jumping, failed-row
step-aside, discard, and degraded reads.

---

## PR 3 — Offline cold-start bootstrap

### Problem

Everything in PR 2 assumes the client can load its active user. Cold, with no
network, it can't: `user load-me` — and even `rt inbox --local-only` — fails
with a raw connect error before any offline code runs. So a *warm* process
that loses connectivity works, and an app *launched* offline gets nothing.

### Change

Extend the same verified-on-write doctrine already used for sigchain and KV
caching to the layers RT sits on. Nothing is served that wasn't fully verified
when it was written; nothing new enters without verification:

- the host's public zone is cached after signature verification, and a probe
  rehydrates chain + zone when it fails on transport;
- the reg-issued client-cert chain (public material only) is cached per key,
  so mTLS clients construct offline;
- `LoadMeFromCache` rebuilds the active user from the merkle-verified sigchain
  snapshot via the loader's existing hydration helpers;
- `LoadTeamFromCache` rebuilds a team wrapper from the verified
  `TeamChainState` — keyring, roster, historical senders — and unboxes cached
  PTK parcels against the snapshot's post-roster;
- a persisted name→team index keeps names resolving with no in-memory index,
  consulted **only** when exploration fails on transport;
- transport failures no longer ride the merkle-race retry schedule (a dead
  network is not a race, and the retries stalled agent startup past its socket
  bind).

### The two questions you already answered

Recording them so the PR doesn't rest on an out-of-band conversation:

1. **Cached PTKs may be used offline without replaying links.** The snapshot
   was fully verified before it was written, and it already persists the
   historical-sender records the unboxing checks need, so rehydrating carries
   that verification forward; the next online load re-verifies. Worst case
   it's stale — a rotation or removal during the offline window — which
   matches the semantics of a message sent just before that change.
2. **The view bearer token isn't needed offline.** It gates server resources;
   a token-less wrapper is correct for local-only work. Your framing, and it
   simplified the design — no grace-period machinery, and any server operation
   attempted with one meets a connect-class error at its own boundary.

### Known limits, by design

- Ad-hoc team names aren't in the persisted index, so an ad-hoc team can't be
  resolved from a cold offline start (RT ad-hoc channels are gated on Stage 1c
  anyway).
- Persisted name rows are written through but never deleted, and are consulted
  only when exploration is impossible — so a stale row can only ever be served
  offline, with the same verified-but-possibly-behind semantics as any
  snapshot.
- Team *mutations* (membership edits, rotations) always require a fresh online
  load and fail connect-class offline.

### Tests

`TestRTOfflineCLIWalkthrough` runs the whole arc from a **cold offline agent
start** through the real CLI and agent: resolve by name, seal with cached
PTKs, queue durably, stale reads with the queued-message overlay, stale inbox
with its badge, a second offline restart (persistence), then reconnect, drain
on inbox sync, server-confirmed delivery — plus a post-reconnect `team inbox`
that pins the online path against the name-index short-circuit.

---

## What we'd genuinely like your read on

Not blockers — places where we made a call and would rather hear yours.

1. **Altitude of the offline fallback.** It currently sits as ~7
   `IsTransportError` special cases across the user loader, team loader, PUK
   minder, and `Reconnect`. Both loaders share `BaseChainLoader.runMany` and
   both already have a `loadExisting*` step; a single "serve the verified
   snapshot on transport failure" policy there would replace the copies. We
   didn't do that surgery unasked in shared code.

2. **`PLCNode.isFresh` requires a non-nil view token.** With token-less
   offline wrappers now legitimate, an offline node is never fresh, so every
   RT operation re-runs a full connect attempt before falling back. We
   mitigated with a cache window rather than change the predicate, since
   freshness semantics are yours.

3. **Connect backoff offline.** `connectLoop` defaults (20 attempts, 1s→30s)
   mean the first attempt after an outage is expensive per operation. A
   probe-level circuit breaker feels right but belongs in shared RPC plumbing.

4. **Retention vs inbox cursors.** Nothing prunes `messages_enc` /
   `user_channels` / `user_inbox` today, so a stored cursor can't go stale —
   which the offline read design depends on. If retention is ever added, it
   needs a "cursor too old" signal on `rtGetChangedThreads` so clients fall
   back to a full resync instead of silently missing deletions. Flagged so the
   requirement travels with the feature rather than being discovered later.

---

## Evidence summary

Full `integration-tests/lib` and `integration-tests/cli` suites pass, plus all
unit tests across `client/`, `lib/`, `server/`. Two adversarial review passes
(one max-effort, one medium) ran over the branch; the second caught a
security-relevant bug in our own bootstrap code — probe hydration accepting
in-memory state from the *current failed run*, which would have let an
attacker on a taken-over hostname skip the hostname-pin check by dropping the
merkle query. Fixed before proposing: hydration now loads only rows a fully
verified run persisted.
