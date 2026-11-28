# Round-trip budget

Fork-local. Goal: every user-facing agent operation finishes in at most two
sequential server round trips. On a 150 ms mobile link each sequential round
trip is visible; a restart that walks the team graph one load at a time took
about 15 of them.

## Measuring

`core.RpcStats.Depth()` is the longest chain of an operation's calls in which
each started after the previous one finished: what a slow link multiplies by
its round-trip time. Calls that overlap count once. Tests that pin a budget
add `core.WithSimulatedRpcLatency` so calls issued together overlap on
loopback too (`integration-tests/lib/rtt_budget_test.go`).

## Batching

Most sequential calls exist because each load learns what to load next from
the previous reply. After a restart over warm local state, local state almost
always already knows the answer. So:

1. **Plan** from local state the exact calls the operation will make. Each
   loader's `serverArg()` (and `TMLTeam.fetchArg`) is the one place its call
   is built, used by both the loader and the planner. `core.PlanCall` runs
   the generated client against a client that sends nothing and records the
   call -- the method and argument `RpcClient.Call2` will see -- so the key a
   reply is kept under is the loader's call by construction.
2. **Batch** them in `User.sync`, at most `rem.MaxSyncCalls` (128) per
   batch, several batches at once when there are more. The server keeps one
   answer's encoded replies within `rem.MaxSyncReplyBytes` (4 MiB, a quarter
   of the transport's frame limit) as the calls complete: a reply past what
   is left comes back as a too-big error, and once one has (or the budget is
   used up) the calls not yet started are not run and come back too big as
   well, so a batch of large chains never runs or holds much more than the
   budget (`runSyncBatch`, `TestSyncBudgetStopsStartingCalls`). Each refused
   call goes out on its own. The server runs each call
   through the handler the client would have called itself, so authorization
   and reply shapes are unchanged, and observes it under that method's name
   (`Telemetry.ObserveCall`). Each result is the call's reply (of the call's
   type, checked) or its error.
3. **Prefetch scope.** Replies go into the operation's `core.PrefetchScope`.
   `RpcClient.Call2` answers a matching call from there, once; a reply that
   won't decode answers nothing.
4. The operation runs unchanged and **verifies every reply** as it would a
   network reply. A reply fetched ahead can predate state stored since (a
   newer merkle root from a load that went out on its own): a chain load that
   used one and failed runs once more, and the network answers
   (`BaseChainLoader.runMany`). Every retry there -- this one and the merkle
   race's -- starts from a clean slate: the loader's own state and the link
   hashes, merkle leaves and last hash the failed run accumulated
   (`BaseChainLoader.resetRun`).

A call planned wrong, refused, failing its checks, or not planned at all (a
team joined since the last run, one with no reusable view token) goes to the
network: one more round trip, never a different result. Scopes never outlive
their operation.

A newer method with a fallback is a `core.OptionalMethod`, and its call is
made under `core.WithOptionalMethod`. A server without it answers "method not
found" once; the `RpcClient` remembers that for the connection it came over
and answers later calls to it locally, so callers fall back to the older
calls at no extra cost. The planner checks the memo for the connection it
is about to send over (`RpcClient.RefusedOver`, which never dials). No time
window: a re-dial (which may reach an upgraded server) asks again. A method
nobody declared optional is never answered locally. `User.sync` is the only
optional method today.

The batch's connect is the operation's first (`syncConnection`; at once over
a live connection). Offline its failure is the operation's: the exploration
returns it, as its own first load would have, rather than dialing again for
that load -- one connect loop, not two or three
(`TestOfflineListMembershipsDialsOnce`).

## Reads that don't need what came before

Some calls wait on an earlier one only because the code ran them in order.
A head-page thread read needs only the channel id and the newest cached seq
(stored in the clear), not the team, so `getThreadRecentMsgs` loads the team
alongside the read and opens the messages once both are back.

## Writes on the read path

Viewing the head page marks it read, through the newest message shown. The
mark goes out only once the page has been built and verified, and after the
view has returned (`Minder.markLater`), so it costs the view no round trip
and a page that failed is never marked (`TestThreadViewFailedPageNotMarked`).
**Contract:** when `GetThreadView` returns a head page, its read pointer may
not have moved yet; `Minder.WaitLaterMarks` waits until every such mark has
been sent or queued (tests use it before reading the pointer or changing
hooks). The mark runs on a fresh context (`libclient.DetachFromOperation`)
carrying only the view's log tags: not cancelled with the view, not in its
stats, none of its values (load-me and prefetch scopes included). Its send
is bounded by `laterMarkTimeout` (20 s), not the call default; a mark the
server didn't take in time, or at all while offline, is queued for the
outbox drain, as any mark is. History pages mark inside the view, as before.

Trade-off, chosen over a combined read-and-mark RPC (tried and dropped): the
server would mark before the client had opened and checked the messages, so
a page failing verification (or a decrypt error, or a failed team load) left
unseen messages marked read on every device. The cost of marking afterwards
is a second RPC per head-page view, off the critical path; and a mark is
lost if the process dies in the moment between the view returning and the
mark reaching the server (the next view of the chat marks it again).

## One load per team per operation

A restart resolving a team explores (which loads it) and then refreshed the
same team again. The exploration now records the teams it loaded in the
operation's load-me scope, and the party cache's ordinary refresh in the same
operation is skipped (`LoadTeamOpts.UnlessExplored`); a forced refresh (a
stale token, new keys) always reloads. Per operation, never per time window.

## Not queuing behind another exploration

Explorations run one at a time (`TeamMinder.exploreMu`). "One is running" is
a count of the loads holding `exploreMu` for round trips (`lockForLoad`: an
exploration, `DumpMembershipChain`, `refreshUserTML`), not whether the lock
is taken, which an instant getter (`UserTMW`) or another caller's probe also
does. On a cold start the
app lists its teams (an exploration) while the tapped chat reads its thread,
whose party-cache load of the team found no record and explored too: it
waited for the whole listing, then explored again. Now the party cache's
first load of a team, while an exploration is running, loads just that team
(`loadOnlyThisTeam`: `directLoadTeam`, its two loads -- my chain and the
team's -- asked for in one batch by `prefetchForTeam`), for that operation
only; the running exploration indexes it as usual. A team I'm only in
through another team fails the direct load and explores as before. An
ad-hoc (DM) team is loaded with member names, as an exploration loads it
(`minMemberLoad`; it is named by its member list), and the same operation's
skipped refresh never hands back an ad-hoc record without them; the skip
fills the teamname cache as a load would (`TestAdHocLoadedAloneCarriesNames`).
The app names teams by name (`name@host`), and a cold process has no name
index in memory, so resolving the name explored too -- before the read even
started. While an exploration runs, a name now resolves from the index the
last exploration persisted (`resolveAndReindex`, `teamResolution`), unchecked,
and only where the caller checks it: `LoadTeam` checks the loaded team still
goes by that name (`teamClaimsName`); if it doesn't, or the load fails for
any reason (without `Refresh`, a team the running exploration has not
indexed yet is simply not found), it resolves the long way -- waiting for
the exploration, as without the shortcut
(`TestUnloadedIndexNameWaitsForExploration`);
librt's and libkv's per-team minder keys (`ResolveForKey`) are a distinct
type, `libclient.TeamKey`, that nothing which loads, seals or shows a team
accepts. Offline resolution keeps its check against the cached snapshot.
`TestPushOpenDirectWithListing` (team by name): the read takes one round trip
while the listing runs, where it took the listing's plus five.
`TestUncheckedNameIsChecked`: a row naming another team is not acted on.

## Covered so far

| Operation (restart, warm local state) | Before | After |
| --- | --- | --- |
| `TeamMinder.ListMemberships`, 3 teams | depth 6, 10 RPCs | depth 2, 2 RPCs |
| `Minder.GetThreadView`, head page by id | depth 9, 10 RPCs | depth 2, 3 RPCs |
| `libkv.Minder.GetFile`, team file | depth 8, 9 RPCs | depth 3, 3 RPCs |

Each includes the client certificate fetch, which a saved certificate
(`perf/cold-start-connections`) removes. The KV read's cache check stays
after the team: it is built by walking the cache, and naming each path
segment needs the team's keys.

Older servers: a "method not found" for `User.sync` is remembered for the
connection and the old calls are used; a new connection asks again
(`TestListMembershipsOlderServer`, using `core.WithSimulatedMissingMethods`).

Follow-ups, kept out of this change on purpose:

- **Cold open pages the whole history.** The SECO app's first read of a chat
  (`foksRtChatTransport.list` with an empty cursor) pages back to the start of
  the thread, 200 messages a round trip, before it shows anything: a channel
  of N messages costs ceil(N/200) sequential round trips on a cold start.
  Showing the newest page first and paging history in behind it is an app
  transport change, to do as its own optimization.
- Teams new since the last run: a view token minted without a challenge
  round trip (an open design decision).