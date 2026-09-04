package lib

import (
	"errors"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// TestKVOfflineReads pins kv_offline.md Phase 1 (D1, D2) end to end with no
// test hooks: real connect failures via the network conditioner.
//
//   - A read that completed against the local cache is served when only the
//     closing KvCacheCheck fails on transport, and the result says so (Stale).
//   - A read the cache cannot answer is an outage, not an empty result (D2).
//   - Listings have no local index yet, so offline they fail honestly rather
//     than synthesizing an empty directory (D2's whole-or-nothing rule).
//   - The write boundary holds: an offline put is a transport error, not a
//     silent queue. (The outbox is Phase 2; until it exists, a write that
//     cannot reach the server has not happened.)
func TestKVOfflineReads(t *testing.T) {
	tew := testEnvBeta(t)
	bluey := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, bluey))

	cs := libclient.CacheSettings{UseMem: true, UseDisk: true}
	kvm := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	dir := proto.KVPath("/" + nm + "/docs")
	file := proto.KVPath("/" + nm + "/docs/note.txt")
	missing := proto.KVPath("/" + nm + "/docs/never-written.txt")
	txt := []byte("offline changes the freshness claim, not the authenticity")

	_, err = kvm.Mkdir(mc, lcl.KVConfig{MkdirP: true}, dir)
	require.NoError(t, err)
	_, err = kvm.PutFileFirst(mc, lcl.KVConfig{}, file, txt, true)
	require.NoError(t, err)

	// --- online: warm the caches through validated reads ---
	st, err := kvm.Stat(mc, lcl.KVConfig{}, file)
	require.NoError(t, err)
	require.False(t, st.Stale)
	gfr, err := kvm.GetFile(mc, lcl.KVConfig{}, file)
	require.NoError(t, err)
	require.False(t, gfr.Stale)
	require.Equal(t, txt, []byte(gfr.Chunk.Chunk))

	// --- airplane mode: real connect failures, zero hooks ---
	// A fresh minder, as in TestRTOfflineNoHooks: the conditioner refuses new
	// dials but cannot kill kvm's already-open socket. (In production a dead
	// socket surfaces as RPC EOF/timeout, which the classifier also covers;
	// the fresh minder is how the harness reaches the dial path.) Its memory
	// cache starts empty, so everything below is served off the disk cache.
	offline := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)
	mc.G().SetNetworkConditioner(core.CatastrophicNetworkConditions{On: true})

	// A warm stat completes from cache and is flagged unvalidated.
	st, err = offline.Stat(mc, lcl.KVConfig{}, file)
	require.NoError(t, err)
	require.True(t, st.Stale)
	require.True(t, st.De != nil)

	// So does the file read, byte-identical to what was written.
	gfr, err = offline.GetFile(mc, lcl.KVConfig{}, file)
	require.NoError(t, err)
	require.True(t, gfr.Stale)
	require.Equal(t, txt, []byte(gfr.Chunk.Chunk))

	// A path the cache has never seen is an outage, not a "not found":
	// answering NONE would require asking the server (D2).
	_, err = offline.Stat(mc, lcl.KVConfig{}, missing)
	require.Error(t, err)
	require.True(t, core.IsTransportError(err), "expected transport-class, got %v", err)

	// Listings enumerate on the server; with no local listing index the
	// offline answer is the outage, never a short or empty directory (D2).
	_, err = offline.List(mc, lcl.KVConfig{}, dir, nil, rem.KVListOpts{})
	require.Error(t, err)
	require.True(t, core.IsTransportError(err), "expected transport-class, got %v", err)

	// The write boundary: no outbox yet, so an offline put fails plainly.
	_, err = offline.PutFileFirst(mc, lcl.KVConfig{},
		proto.KVPath("/"+nm+"/docs/offline.txt"), []byte("x"), true)
	require.Error(t, err)
	require.True(t, core.IsTransportError(err), "expected transport-class, got %v", err)

	// --- reconnect: the same minder validates again and drops the flag ---
	mc.G().SetNetworkConditioner(nil)
	st, err = offline.Stat(mc, lcl.KVConfig{}, file)
	require.NoError(t, err)
	require.False(t, st.Stale)
	gfr, err = offline.GetFile(mc, lcl.KVConfig{}, file)
	require.NoError(t, err)
	require.False(t, gfr.Stale)
}

// TestKVOfflineOutbox pins kv_offline.md Phase 2 (D3-D6, D8-D11): an offline
// write queues as sealed intent and returns KVWriteQueuedError; the drain
// re-runs it through the normal online machinery with the queue-time version
// as its CAS predicate; a write that lost its race to another writer stops
// and surfaces as conflicted, never clobbering the winner; and a write whose
// parent state isn't cached refuses (plain transport error) rather than
// queueing on a guess (D9).
func TestKVOfflineOutbox(t *testing.T) {
	tew := testEnvBeta(t)
	bluey := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, bluey))

	cs := libclient.CacheSettings{UseMem: true, UseDisk: true}
	online := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	cfg := lcl.KVConfig{}
	owCfg := lcl.KVConfig{OverwriteOk: true}
	dir := proto.KVPath("/" + nm + "/docs")
	fileA := proto.KVPath("/" + nm + "/docs/a.txt")
	fileB := proto.KVPath("/" + nm + "/docs/b.txt")
	fileC := proto.KVPath("/" + nm + "/docs/c.txt")
	v1 := []byte("first version")

	_, err = online.Mkdir(mc, lcl.KVConfig{MkdirP: true}, dir)
	require.NoError(t, err)
	for _, f := range []proto.KVPath{fileA, fileB, fileC} {
		_, err = online.PutFileFirst(mc, cfg, f, v1, true)
		require.NoError(t, err)
	}

	// Warm the shared disk cache through validated reads, then go offline
	// with a fresh minder -- the conditioner refuses new dials but cannot
	// kill an already-open socket (same as TestKVOfflineReads).
	for _, f := range []proto.KVPath{fileA, fileB, fileC} {
		_, err = online.Stat(mc, cfg, f)
		require.NoError(t, err)
	}
	mc.G().SetNetworkConditioner(core.CatastrophicNetworkConditions{On: true})
	offline := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)

	// An offline overwrite of a cached dirent queues and says so.
	v2 := []byte("second version, written offline")
	_, err = offline.PutFileFirst(mc, owCfg, fileA, v2, true)
	var qerr core.KVWriteQueuedError
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)

	// So does an unlink of a cached dirent.
	err = offline.Unlink(mc, cfg, fileB)
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)

	// A put under a NAME the cache has never resolved refuses rather than
	// queueing on a guess (D9): the dirent lookup needs the server.
	_, err = offline.PutFileFirst(mc, cfg, proto.KVPath("/"+nm+"/docs/new.txt"), v2, true)
	require.Error(t, err)
	require.False(t, errors.As(err, &qerr))
	require.True(t, core.IsTransportError(err), "expected transport-class, got %v", err)

	rows, err := offline.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, lcl.KVOutboxState_Queued, rows[0].State)
	require.Equal(t, lcl.KVOutboxOp_PutSmallFile, rows[0].Op)
	require.Equal(t, fileA, rows[0].Path)
	require.Equal(t, lcl.KVOutboxOp_Unlink, rows[1].Op)
	require.Equal(t, fileB, rows[1].Path)

	// While the offline minder holds its queue, another writer overwrites
	// fileC -- the conflict the drain must detect. Queue an offline
	// overwrite of fileC too.
	v3 := []byte("offline edit that will lose its race")
	_, err = offline.PutFileFirst(mc, owCfg, fileC, v3, true)
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)

	mc.G().SetNetworkConditioner(nil)
	winner := []byte("the other writer's content")
	_, err = online.PutFileFirst(mc, owCfg, fileC, winner, true)
	require.NoError(t, err)

	// The winner write's own successful validation already fired the drain
	// (triggers are per party, not per minder): fileA landed, fileB's unlink
	// landed, fileC conflicted -- and it conflicted against the winner's
	// committed write, since the trigger runs at validation time, after the
	// winner's dirent put. An explicit drain now finds nothing to do.
	res, err := offline.DrainOutbox(mc, cfg)
	require.NoError(t, err)
	require.Equal(t, 0, res.Retired)
	require.Equal(t, 0, res.Conflicted)
	require.Equal(t, 0, res.Failed)
	require.Equal(t, 0, res.StillQueued)

	// The landed write reads back; the unlink took; the conflicted entry
	// held its content and the winner was not clobbered.
	gfr, err := online.GetFile(mc, cfg, fileA)
	require.NoError(t, err)
	require.Equal(t, v2, []byte(gfr.Chunk.Chunk))
	_, err = online.Stat(mc, cfg, fileB)
	require.Error(t, err) // tombstoned
	gfr, err = online.GetFile(mc, cfg, fileC)
	require.NoError(t, err)
	require.Equal(t, winner, []byte(gfr.Chunk.Chunk))

	rows, err = offline.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, lcl.KVOutboxState_Conflicted, rows[0].State)
	require.Equal(t, fileC, rows[0].Path)
	require.NotEmpty(t, rows[0].LastErr)

	// A second drain does not touch the conflicted entry (D6: never
	// auto-resolved).
	res, err = offline.DrainOutbox(mc, cfg)
	require.NoError(t, err)
	require.Equal(t, 0, res.Retired+res.Conflicted+res.Failed+res.StillQueued)
}

// TestKVOfflineDrainTrigger pins the Phase 2 drain trigger: a successful
// validation on any KV operation is proof of connectivity, and the queued
// outbox drains itself -- no explicit DrainOutbox call.
func TestKVOfflineDrainTrigger(t *testing.T) {
	tew := testEnvBeta(t)
	bluey := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, bluey))

	cs := libclient.CacheSettings{UseMem: true, UseDisk: true}
	online := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	cfg := lcl.KVConfig{}
	owCfg := lcl.KVConfig{OverwriteOk: true}
	file := proto.KVPath("/" + nm + "/docs/t.txt")
	v1 := []byte("v1")

	_, err = online.Mkdir(mc, lcl.KVConfig{MkdirP: true}, proto.KVPath("/"+nm+"/docs"))
	require.NoError(t, err)
	_, err = online.PutFileFirst(mc, cfg, file, v1, true)
	require.NoError(t, err)
	_, err = online.Stat(mc, cfg, file)
	require.NoError(t, err)

	mc.G().SetNetworkConditioner(core.CatastrophicNetworkConditions{On: true})
	offline := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)
	v2 := []byte("v2, queued offline")
	_, err = offline.PutFileFirst(mc, owCfg, file, v2, true)
	var qerr core.KVWriteQueuedError
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)

	// Reconnect and perform an ordinary read. Its successful validation must
	// fire the drain; nobody calls DrainOutbox.
	mc.G().SetNetworkConditioner(nil)
	st, err := offline.Stat(mc, cfg, file)
	require.NoError(t, err)
	require.False(t, st.Stale)

	rows, err := offline.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 0, "outbox should have drained itself")

	gfr, err := online.GetFile(mc, cfg, file)
	require.NoError(t, err)
	require.Equal(t, v2, []byte(gfr.Chunk.Chunk))
}

// TestKVOfflineMkdirQueue pins mkdir queueing (kv_offline.md Phase 2): a
// leaf directory creation interrupted mid-flight -- absence confirmed live,
// transport dead before the dir row or its link landed -- queues as intent
// and drains. The interruption is driven by the PreRPC test hook, since the
// network conditioner refuses dials but cannot kill an established
// connection; production sees these states as RPC EOF/timeouts.
func TestKVOfflineMkdirQueue(t *testing.T) {
	tew := testEnvBeta(t)
	bluey := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, bluey))

	cs := libclient.CacheSettings{UseMem: true, UseDisk: true}
	kvm := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	cfg := lcl.KVConfig{}
	_, err = kvm.Mkdir(mc, lcl.KVConfig{MkdirP: true}, proto.KVPath("/"+nm))
	require.NoError(t, err)

	failOnce := func(op string) {
		fired := false
		kvm.SetTestHooks(&libkv.MinderTestHooks{PreRPC: func(got string) error {
			if got == op && !fired {
				fired = true
				return core.NewConnectError("test-induced mid-flight failure",
					errors.New("connection torn"))
			}
			return nil
		}})
	}

	// (a) The dir-row write dies: nothing landed server-side.
	subA := proto.KVPath("/" + nm + "/qa")
	failOnce("uploadDir")
	_, err = kvm.Mkdir(mc, cfg, subA)
	var qerr core.KVWriteQueuedError
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)
	kvm.SetTestHooks(nil)

	// (b) The link dies after the dir row landed: the drain's dir-row replay
	// must be a server-side no-op.
	subB := proto.KVPath("/" + nm + "/qb")
	failOnce("putDirent")
	_, err = kvm.Mkdir(mc, cfg, subB)
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)
	kvm.SetTestHooks(nil)

	rows, err := kvm.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, lcl.KVOutboxOp_Mkdir, rows[0].Op)
	require.Equal(t, subA, rows[0].Path)
	require.Equal(t, lcl.KVOutboxOp_Mkdir, rows[1].Op)
	require.Equal(t, subB, rows[1].Path)

	res, err := kvm.DrainOutbox(mc, cfg)
	require.NoError(t, err)
	require.Equal(t, 2, res.Retired)
	require.Equal(t, 0, res.Conflicted+res.Failed+res.StillQueued)

	// Both directories are real: files land inside them and read back.
	for _, d := range []proto.KVPath{subA, subB} {
		f := proto.KVPath(string(d) + "/f.txt")
		_, err = kvm.PutFileFirst(mc, cfg, f, []byte("in a drained dir"), true)
		require.NoError(t, err)
		gfr, err := kvm.GetFile(mc, cfg, f)
		require.NoError(t, err)
		require.Equal(t, []byte("in a drained dir"), []byte(gfr.Chunk.Chunk))
	}
}

// The outbox's partial-failure branches are unreachable without injection:
// SQLite will not fail on demand, and these are the branches that decide
// whether a queued write is stranded, re-attempted, or silently hidden.
//
// dbFailerAfter lets the first `skip` calls to the named step through and
// fails every one after that. The count is how a test reaches a *particular*
// call site of a step that runs more than once in an operation -- a drain
// reads the index once for its snapshot and again inside setOutboxState, so
// skip=1 lands in the second.
func dbFailerAfter(op string, skip int) func(string) error {
	seen := 0
	return func(got string) error {
		if got != op {
			return nil
		}
		seen++
		if seen <= skip {
			return nil
		}
		return core.InternalError("test-induced local DB failure")
	}
}

// dbFailer fails every call to the named step for as long as it stays armed.
// The returned pointer disarms it, for the cases that need the fault to stop
// without swapping the whole hook out.
func dbFailer(op string) (func(string) error, *bool) {
	armed := true
	return func(got string) error {
		if got != op || !armed {
			return nil
		}
		return core.InternalError("test-induced local DB failure")
	}, &armed
}

// TestKVOutboxDBFailures covers the outbox's behaviour when the *local*
// database misbehaves -- the branches every review pass found bugs in, none
// of which the happy-path tests reach.
//
//   - A failed hint probe must not clear the trigger hint. Reading "could not
//     tell" as "nothing queued" would disable the drain for the life of the
//     process and strand every queued write.
//   - A state change is atomic: if it fails, the entry row and the index
//     mirror stay in agreement, so no later drain re-attempts an entry the
//     user is holding.
//   - An entry whose payload will not open is still listed. It occupies a
//     slot against the per-party cap, so hiding it would leave the user
//     facing an "outbox full" they cannot account for.
func TestKVOutboxDBFailures(t *testing.T) {
	tew := testEnvBeta(t)
	bluey := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, bluey))

	cs := libclient.CacheSettings{UseMem: true, UseDisk: true}
	online := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	cfg := lcl.KVConfig{}
	owCfg := lcl.KVConfig{OverwriteOk: true}
	file := proto.KVPath("/" + nm + "/docs/a.txt")

	_, err = online.Mkdir(mc, lcl.KVConfig{MkdirP: true}, proto.KVPath("/"+nm+"/docs"))
	require.NoError(t, err)
	_, err = online.PutFileFirst(mc, cfg, file, []byte("v1"), true)
	require.NoError(t, err)
	_, err = online.Stat(mc, cfg, file)
	require.NoError(t, err)

	// Queue a write offline.
	mc.G().SetNetworkConditioner(core.CatastrophicNetworkConditions{On: true})
	offline := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)
	_, err = offline.PutFileFirst(mc, owCfg, file, []byte("v2 queued"), true)
	var qerr core.KVWriteQueuedError
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)
	mc.G().SetNetworkConditioner(nil)

	// --- a failed hint probe must not strand the queue ---
	// A fresh minder starts with the hint Unknown for this party, so its
	// first operation probes the DB. Fail that probe: the operation must
	// still succeed, and the queued write must still be deliverable
	// afterwards rather than being written off as "no work".
	probe := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)
	failProbe, armed := dbFailer("dbGetOutboxIndex")
	probe.SetTestHooks(&libkv.MinderTestHooks{PreDB: failProbe})
	_, err = probe.Stat(mc, cfg, file)
	require.NoError(t, err, "a failed hint probe must not fail the operation")
	*armed = false
	probe.SetTestHooks(nil)

	rows, err := probe.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the queued write must survive a failed probe")
	require.Equal(t, lcl.KVOutboxState_Queued, rows[0].State)

	// --- a failed state change leaves row and index in agreement ---
	// Force a conflict for the queued entry, then fail the write that would
	// record it. The winner's own write fires the drain trigger, so the hook
	// goes on that minder too: its triggered drain fails harmlessly (the
	// trigger logs and moves on) and leaves the entry Queued for the
	// explicit drain below.
	failState, _ := dbFailer("setOutboxState")
	online.SetTestHooks(&libkv.MinderTestHooks{PreDB: failState})
	offline.SetTestHooks(&libkv.MinderTestHooks{PreDB: failState})
	_, err = online.PutFileFirst(mc, owCfg, file, []byte("winner"), true)
	require.NoError(t, err, "a failed outbox write must not fail an unrelated put")

	_, err = offline.DrainOutbox(mc, cfg)
	require.Error(t, err, "the drain surfaces the DB failure")
	online.SetTestHooks(nil)
	offline.SetTestHooks(nil)

	rows, err = offline.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, rows[0].State, rows[0].IndexState,
		"entry row and index mirror must not diverge")
	require.Equal(t, lcl.KVOutboxState_Queued, rows[0].State,
		"a state change that failed must not half-apply")

	// Same again, but failing the index *read* that setOutboxState makes
	// rather than the step as a whole. A drain reads the index once to build
	// its snapshot, so letting the first read through and failing the second
	// lands inside setOutboxState -- between the entry-row write and the
	// index write, as they used to be ordered. That is the window where a
	// half-applied change left the index claiming Queued for an entry the
	// row had marked Conflicted; the assertion below is that no such
	// divergence is reachable now that both go in one transaction.
	offline.SetTestHooks(&libkv.MinderTestHooks{PreDB: dbFailerAfter("dbGetOutboxIndex", 1)})
	_, err = offline.DrainOutbox(mc, cfg)
	require.Error(t, err)
	offline.SetTestHooks(nil)

	rows, err = offline.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, rows[0].State, rows[0].IndexState,
		"a failure inside setOutboxState must not split row from index")
	require.Equal(t, lcl.KVOutboxState_Queued, rows[0].State)

	// Retried without the fault, the same drain records the conflict, and
	// row and index still agree.
	res, err := offline.DrainOutbox(mc, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, res.Conflicted)
	rows, err = offline.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, lcl.KVOutboxState_Conflicted, rows[0].State)
	require.Equal(t, rows[0].State, rows[0].IndexState)

	// The winner was never clobbered by any of this.
	gfr, err := online.GetFile(mc, cfg, file)
	require.NoError(t, err)
	require.Equal(t, []byte("winner"), []byte(gfr.Chunk.Chunk))

	// --- an unreadable entry stays visible, and does not block the rest ---
	// Queue a second write, then make every payload open fail. Both rows --
	// the Conflicted one and the freshly Queued one -- occupy slots against
	// maxKVOutboxEntries, so listing must show them (Unreadable, with the
	// reason) rather than skipping them; and the drain must skip the queued
	// one rather than aborting.
	fileB := proto.KVPath("/" + nm + "/docs/b.txt")
	_, err = online.PutFileFirst(mc, cfg, fileB, []byte("b1"), true)
	require.NoError(t, err)
	_, err = online.Stat(mc, cfg, fileB)
	require.NoError(t, err)
	mc.G().SetNetworkConditioner(core.CatastrophicNetworkConditions{On: true})
	offline2 := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)
	_, err = offline2.PutFileFirst(mc, owCfg, fileB, []byte("b2 queued"), true)
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)
	mc.G().SetNetworkConditioner(nil)

	failOpen, _ := dbFailer("openOutboxPayload")
	offline2.SetTestHooks(&libkv.MinderTestHooks{PreDB: failOpen})

	rows, err = offline2.ListOutbox(mc, cfg)
	require.NoError(t, err, "unreadable rows must not fail the listing")
	require.Len(t, rows, 2, "unreadable rows must still be listed")
	for _, r := range rows {
		require.True(t, r.Unreadable)
		require.NotEmpty(t, r.LastErr)
		require.Equal(t, proto.KVPath(""), r.Path, "payload fields are unset")
	}

	// The drain skips what it cannot open instead of aborting, and leaves it
	// queued for a later pass.
	res, err = offline2.DrainOutbox(mc, cfg)
	require.NoError(t, err, "an unreadable row must not abort the drain")
	require.Equal(t, 0, res.Retired)
	offline2.SetTestHooks(nil)

	// With the fault cleared the skipped entry drains normally.
	res, err = offline2.DrainOutbox(mc, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, res.Retired)
	gfr, err = online.GetFile(mc, cfg, fileB)
	require.NoError(t, err)
	require.Equal(t, []byte("b2 queued"), []byte(gfr.Chunk.Chunk))
}

// TestKVOutboxNoPlaintextPaths pins the confidentiality rule the sealed
// payload exists for (kv_offline.md D3): the local database stores values
// unencrypted, so nothing an outbox row persists may carry a KV path. The
// easy way to break this is an error message -- KVNoentError prints the path
// it could not find -- so the rejection is recorded as a status code, never
// as text.
func TestKVOutboxNoPlaintextPaths(t *testing.T) {
	tew := testEnvBeta(t)
	bluey := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, bluey))

	cs := libclient.CacheSettings{UseMem: true, UseDisk: true}
	online := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	cfg := lcl.KVConfig{}
	owCfg := lcl.KVConfig{OverwriteOk: true}
	// A path component distinctive enough that finding it anywhere in the
	// DB file proves it leaked.
	secret := "TOPSECRETDIRNAME"
	dir := proto.KVPath("/" + nm + "/" + secret)
	file := proto.KVPath("/" + nm + "/" + secret + "/notes.txt")

	_, err = online.Mkdir(mc, lcl.KVConfig{MkdirP: true}, dir)
	require.NoError(t, err)
	_, err = online.PutFileFirst(mc, cfg, file, []byte("v1"), true)
	require.NoError(t, err)
	_, err = online.Stat(mc, cfg, file)
	require.NoError(t, err)

	// Queue a write offline, then force it to a terminal rejection so an
	// error is recorded against the row.
	mc.G().SetNetworkConditioner(core.CatastrophicNetworkConditions{On: true})
	offline := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), cs)
	_, err = offline.PutFileFirst(mc, owCfg, file, []byte("v2 queued"), true)
	var qerr core.KVWriteQueuedError
	require.True(t, errors.As(err, &qerr), "expected queued, got %v", err)

	// Another writer takes the name, so the drain conflicts and records it.
	failState, _ := dbFailer("setOutboxState")
	online.SetTestHooks(&libkv.MinderTestHooks{PreDB: failState})
	mc.G().SetNetworkConditioner(nil)
	_, err = online.PutFileFirst(mc, owCfg, file, []byte("winner"), true)
	require.NoError(t, err)
	online.SetTestHooks(nil)

	res, err := offline.DrainOutbox(mc, cfg)
	require.NoError(t, err)
	require.Equal(t, 1, res.Conflicted)

	rows, err := offline.ListOutbox(mc, cfg)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, lcl.KVOutboxState_Conflicted, rows[0].State)
	// A reason is still reported -- as a status code, which cannot carry a
	// path -- so the fix did not cost the diagnostic.
	require.NotEmpty(t, rows[0].LastErr)
	require.NotContains(t, rows[0].LastErr, secret)
	require.Equal(t, dir+"/notes.txt", rows[0].Path, "the path is still available in memory")
}
