// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libkv

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/kv"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

// The client-side durable KV outbox (docs/kv_offline.md, D3-D6, D8, D10,
// D11). A write that fails on a transport-class error is recorded here as
// sealed *intent* -- path, node ID, sealed content, roles, and the dirent
// version observed at queue time -- never as a prepared dirent, which could
// not survive a directory rotation (D4). The drain re-runs the write through
// the normal online machinery, so name MAC, name box, dirVersion and binding
// MAC are all re-derived from whatever key material is current at drain time;
// the queue-time version remains the CAS predicate. Re-preparation is about
// which key is speaking, not about what the write thought it was replacing.
//
// Idempotency is by node ID (D5): client-chosen at queue time, stable across
// drains, and carried verbatim in the dirent's value. A drain that loses its
// CAS race asks whether the current value is its own node -- an earlier
// attempt that landed without an ack -- and retires or conflicts accordingly.
// Re-uploading the node itself is a server-side no-op on identical bytes.
//
// A conflicted entry stops and surfaces (D6); it is never re-applied over the
// winner. Ordering is per-parent-directory FIFO by ord; a conflicted or
// failed entry steps aside so it does not hold its directory hostage.
//
// Storage mirrors the librt outbox: one KVOutboxEntry row per queued write
// under the party scope, keyed by an opaque entry id, plus a KVOutboxIndex
// singleton (the DB layer cannot enumerate keys in a scope). Every mutation
// -- enqueue, state change, removal -- writes the row and the index in a
// single DbPutTx, so the two can never disagree. That matters beyond
// tidiness: a half-applied state change would leave the index claiming
// Queued for an entry the user is holding as Conflicted, and the next drain
// would re-attempt a write D6 promised not to auto-resolve. The index's
// State is a mirror for cheap filtering; the entry row is authoritative, and
// the drain gates on it.
//
// An index entry whose row is missing (from an older non-atomic write, or a
// half-written DB) is still tolerated: it is dropped, self-healing, on the
// next listing or drain.
//
// The payload is boxed under the party's KV-store key before it reaches the
// local DB, which stores values unencrypted: a queued path must not land on
// disk in the clear (D3).
var kvOutboxLocks sync.Map // party-scope key (string) -> *sync.Mutex

// kvPartyKey is a hashable identity for a party scope: FQParty holds
// slice-backed IDs, so it can't key a map directly.
func kvPartyKey(kvp *KVParty) string {
	fqp := kvp.Id()
	return string(fqp.Party) + "@" + string(fqp.Host[:])
}

func kvOutboxLock(kvp *KVParty) *sync.Mutex {
	v, _ := kvOutboxLocks.LoadOrStore(kvPartyKey(kvp), &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Drain triggering. The drain fires opportunistically after any KV operation
// whose closing validation succeeded -- proof the server is reachable again
// -- so queued writes deliver on the next warm read or write, not on an
// explicit command. Two guards keep this cheap and re-entrant-safe:
//
//   - a per-party hint (empty / maybe-work / unknown) so the common case --
//     nothing queued -- costs one atomic load per operation, with the local
//     DB consulted once per process and after that only when an enqueue or a
//     drain moves the answer. Every transition that could *clear* the hint
//     happens under the outbox lock, so it cannot go stale-empty over a
//     concurrent enqueue; setting it to "work" is conservative and needs no
//     lock.
//   - a per-party in-flight flag, because the drain itself runs operations
//     through the same cache-race loop that hosts the trigger: without the
//     flag, a drain's own successful validation would re-enter the drain on
//     the same goroutine and deadlock on the outbox lock.
const (
	kvOutboxHintUnknown int32 = 0
	kvOutboxHintEmpty   int32 = 1
	kvOutboxHintWork    int32 = 2
)

var (
	kvOutboxHints  sync.Map // party key -> *atomic.Int32 (hint states above)
	kvOutboxDrains sync.Map // party key -> *atomic.Bool (drain in flight)
)

func kvOutboxHint(kvp *KVParty) *atomic.Int32 {
	v, _ := kvOutboxHints.LoadOrStore(kvPartyKey(kvp), &atomic.Int32{})
	return v.(*atomic.Int32)
}

func kvOutboxDrainFlag(kvp *KVParty) *atomic.Bool {
	v, _ := kvOutboxDrains.LoadOrStore(kvPartyKey(kvp), &atomic.Bool{})
	return v.(*atomic.Bool)
}

// hasOutboxWork reports whether a drain has anything to do: queued rows.
// Conflicted and failed rows are not work; they are held for the user.
//
// The error is returned rather than folded into the bool because callers use
// this to *clear* the trigger hint: reading "could not tell" as "nothing
// queued" would pin the hint to Empty on a transient DB error and disable the
// trigger for the rest of the process, stranding every queued write.
func (k *Minder) hasOutboxWork(m MetaContext, kvp *KVParty) (bool, error) {
	idx, err := k.dbGetOutboxIndex(m, kvp)
	if err != nil {
		return false, err
	}
	for _, e := range idx.Entries {
		if e.State == lcl.KVOutboxState_Queued {
			return true, nil
		}
	}
	return false, nil
}

// maybeDrainOutbox is the trigger: called from the cache-race loop after a
// successful validation. Synchronous, like the chat inbox drain -- the
// operation that discovers connectivity pays for the delivery it proves
// possible.
func (k *Minder) maybeDrainOutbox(m MetaContext, kvp *KVParty) {
	flag := kvOutboxDrainFlag(kvp)
	if flag.Load() {
		return
	}
	hint := kvOutboxHint(kvp)
	switch hint.Load() {
	case kvOutboxHintEmpty:
		return
	case kvOutboxHintUnknown:
		// Probe and publish under the outbox lock. Unlocked, an enqueue
		// landing between the read and the store would have its Work
		// overwritten by our Empty, and the trigger would stay off for the
		// life of the process -- the invariant this file claims for every
		// hint-clearing transition. The lock is released before the drain
		// itself, which must not hold it.
		lk := kvOutboxLock(kvp)
		lk.Lock()
		work, err := k.hasOutboxWork(m, kvp)
		if err == nil && !work {
			hint.Store(kvOutboxHintEmpty)
		} else if err == nil {
			hint.Store(kvOutboxHintWork)
		}
		lk.Unlock()
		if err != nil {
			// Leave the hint Unknown so the next operation re-checks.
			m.Warnw("kvOutbox", "stage", "hint-probe", "party", kvp.Id(), "err", err)
			return
		}
		if !work {
			return
		}
	}
	if !flag.CompareAndSwap(false, true) {
		return
	}
	defer flag.Store(false)
	res, err := k.drainOutboxWithParty(m, kvp)
	if err != nil {
		m.Warnw("kvOutbox", "stage", "triggered-drain", "err", err)
		return
	}
	m.Infow("kvOutbox", "stage", "triggered-drain",
		"retired", res.Retired, "stillQueued", res.StillQueued,
		"conflicted", res.Conflicted, "failed", res.Failed)
}

// maxKVDrainRetries bounds how many times an entry may come back from a drain
// as "contended" (drainRetryLater) before it is held as Failed. Without a cap
// an entry that never converges is re-attempted on every drain trigger, at
// cacheRaceLoop's full budget, for the life of the process -- slowing every
// read on the party with nothing in the outbox to explain why.
const maxKVDrainRetries = 8

// maxKVOutboxEntries bounds outbox rows per party -- queued, conflicted and
// failed alike, so a stream of rejections cannot grow state unboundedly. At
// capacity, writes fail fast with KVOutboxFullError rather than silently
// shedding an entry.
const maxKVOutboxEntries = 256

func (k *Minder) dbGetOutboxIndex(
	m MetaContext,
	kvp *KVParty,
) (
	lcl.KVOutboxIndex,
	error,
) {
	if err := k.preDBHook("dbGetOutboxIndex"); err != nil {
		return lcl.KVOutboxIndex{}, err
	}
	var ret lcl.KVOutboxIndex
	scope := kvp.Id()
	_, err := m.DbGet(&ret, libclient.DbTypeSoft, &scope,
		lcl.DataType_KVOutboxIndex, core.EmptyKey{})
	if errors.Is(err, core.RowNotFoundError{}) {
		return lcl.KVOutboxIndex{}, nil
	}
	if err != nil {
		return lcl.KVOutboxIndex{}, err
	}
	return ret, nil
}

func (k *Minder) dbGetOutboxEntry(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
) (
	*lcl.KVOutboxEntry,
	error,
) {
	var ret lcl.KVOutboxEntry
	scope := kvp.Id()
	_, err := m.DbGet(&ret, libclient.DbTypeSoft, &scope,
		lcl.DataType_KVOutboxEntry, eid)
	if errors.Is(err, core.RowNotFoundError{}) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

func (k *Minder) dbPutOutboxEntry(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
	entry *lcl.KVOutboxEntry,
) error {
	scope := kvp.Id()
	return m.DbPut(libclient.DbTypeSoft, libclient.PutArg{
		Scope: &scope,
		Typ:   lcl.DataType_KVOutboxEntry,
		Key:   eid,
		Val:   entry,
	})
}

// removeOutbox retires an entry: the row delete and the index rewrite go in
// one transaction, so the two cannot disagree. Callers hold the outbox lock.
func (k *Minder) removeOutbox(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
) error {
	if err := k.preDBHook("removeOutbox"); err != nil {
		return err
	}
	idx, err := k.dbGetOutboxIndex(m, kvp)
	if err != nil {
		return err
	}
	idx.Entries = slices.DeleteFunc(idx.Entries, func(e lcl.KVOutboxIndexEntry) bool {
		return e.Eid == eid
	})
	scope := kvp.Id()
	return m.DbPutTx(libclient.DbTypeSoft, []libclient.PutArg{
		{Scope: &scope, Typ: lcl.DataType_KVOutboxEntry, Key: eid, Del: true},
		{Scope: &scope, Typ: lcl.DataType_KVOutboxIndex, Key: core.EmptyKey{}, Val: &idx},
	})
}

// errToOutboxCode reduces a drain rejection to the status code that will be
// persisted for it. Only the code is stored: error *messages* carry KV paths
// (KVNoentError prints one), and the outbox row lives in a local DB that
// stores values unencrypted -- which is exactly why the payload beside it is
// sealed. The full message still reaches the logs at the point of failure.
func errToOutboxCode(e error) proto.StatusCode {
	if e == nil {
		return proto.StatusCode_OK
	}
	// An error ErrorToStatus does not recognize falls into the default arm,
	// whose code is OK(0) -- which here would read as "never rejected". Map
	// it to GENERIC_ERROR so a recorded rejection is always distinguishable
	// from the absence of one.
	code := core.ErrorToStatus(e).Sc
	if code == proto.StatusCode_OK {
		return proto.StatusCode_GENERIC_ERROR
	}
	return code
}

// noteOutboxAttempt records a non-terminal drain rejection on an entry --
// bumping Attempts and LastError while leaving it Queued -- and returns the
// new attempt count. Callers hold the outbox lock.
func (k *Minder) noteOutboxAttempt(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
	lastErr error,
) (
	uint64,
	error,
) {
	if err := k.preDBHook("noteOutboxAttempt"); err != nil {
		return 0, err
	}
	ent, err := k.dbGetOutboxEntry(m, kvp, eid)
	if err != nil {
		return 0, err
	}
	if ent == nil {
		return 0, nil
	}
	ent.Attempts++
	ent.LastErrCode = errToOutboxCode(lastErr)
	err = k.dbPutOutboxEntry(m, kvp, eid, ent)
	if err != nil {
		return 0, err
	}
	return ent.Attempts, nil
}

// setOutboxState flips an entry (and its index mirror) to conflicted or
// failed, recording the rejection. Callers hold the outbox lock.
func (k *Minder) setOutboxState(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
	state lcl.KVOutboxState,
	lastErr error,
) error {
	return k.setOutboxStateInner(m, kvp, eid, state, lastErr, true)
}

// setOutboxStateNoCount is setOutboxState for a rejection whose attempt was
// already recorded (see noteOutboxAttempt), so the count stays honest.
func (k *Minder) setOutboxStateNoCount(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
	state lcl.KVOutboxState,
	lastErr error,
) error {
	return k.setOutboxStateInner(m, kvp, eid, state, lastErr, false)
}

func (k *Minder) setOutboxStateInner(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
	state lcl.KVOutboxState,
	lastErr error,
	countAttempt bool,
) error {
	if err := k.preDBHook("setOutboxState"); err != nil {
		return err
	}
	ent, err := k.dbGetOutboxEntry(m, kvp, eid)
	if err != nil {
		return err
	}
	if ent == nil {
		return nil
	}
	ent.State = state
	ent.LastErrCode = errToOutboxCode(lastErr)
	if countAttempt {
		ent.Attempts++
	}
	idx, err := k.dbGetOutboxIndex(m, kvp)
	if err != nil {
		return err
	}
	for i := range idx.Entries {
		if idx.Entries[i].Eid == eid {
			idx.Entries[i].State = state
		}
	}
	// Row and index mirror move together: a half-applied state change would
	// let the drain re-attempt an entry the user is holding (D6).
	scope := kvp.Id()
	return m.DbPutTx(libclient.DbTypeSoft, []libclient.PutArg{
		{Scope: &scope, Typ: lcl.DataType_KVOutboxEntry, Key: eid, Val: ent},
		{Scope: &scope, Typ: lcl.DataType_KVOutboxIndex, Key: core.EmptyKey{}, Val: &idx},
	})
}

// enqueueOutbox durably queues one sealed write intent. key/rg are the party
// KV-store key bundle (and its role+generation) that seals the payload
// envelope.
func (k *Minder) enqueueOutbox(
	m MetaContext,
	kvp *KVParty,
	eid proto.KVNodeID,
	key *kv.KeyBundle,
	rg proto.RoleAndGen,
	payload *lcl.KVOutboxPayload,
) error {
	if err := k.preDBHook("enqueueOutbox"); err != nil {
		return err
	}
	// Padded, like every other seal over a name in this package: the payload
	// carries the path, and an unpadded ciphertext leaks its length to
	// anyone reading the (unencrypted) local DB.
	box, err := key.BoxPadded(payload)
	if err != nil {
		return err
	}

	lock := kvOutboxLock(kvp)
	lock.Lock()
	defer lock.Unlock()

	idx, err := k.dbGetOutboxIndex(m, kvp)
	if err != nil {
		return err
	}
	if len(idx.Entries) >= maxKVOutboxEntries {
		return core.KVOutboxFullError{}
	}
	ord := idx.NextOrd
	idx.NextOrd++
	idx.Entries = append(idx.Entries, lcl.KVOutboxIndexEntry{
		Ord:   ord,
		Eid:   eid,
		State: lcl.KVOutboxState_Queued,
	})
	entry := lcl.KVOutboxEntry{
		State: lcl.KVOutboxState_Queued,
		Rg:    rg,
		Box:   *box,
		Ord:   ord,
		Ctime: proto.ExportTimeMicro(time.Now()),
	}
	scope := kvp.Id()
	err = m.DbPutTx(libclient.DbTypeSoft, []libclient.PutArg{
		{Scope: &scope, Typ: lcl.DataType_KVOutboxEntry, Key: eid, Val: &entry},
		{Scope: &scope, Typ: lcl.DataType_KVOutboxIndex, Key: core.EmptyKey{}, Val: &idx},
	})
	if err != nil {
		return err
	}
	// Only once the row is durable: a hint claiming work that no transaction
	// committed would cost the next operation a pointless drain pass.
	kvOutboxHint(kvp).Store(kvOutboxHintWork)
	return nil
}

func (k *Minder) openOutboxPayload(
	m MetaContext,
	kvp *KVParty,
	ent *lcl.KVOutboxEntry,
) (
	*lcl.KVOutboxPayload,
	error,
) {
	if err := k.preDBHook("openOutboxPayload"); err != nil {
		return nil, err
	}
	kb, err := kvp.kvStoreKeyAtRoleGen(m, ent.Rg.Role, ent.Rg.Gen)
	if err != nil {
		return nil, err
	}
	var ret lcl.KVOutboxPayload
	err = kb.Unbox(&ret, ent.Box)
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

// maybeQueueWrite is the enqueue decision at a write's failure point: a
// transport-class error queues the intent and converts to KVWriteQueuedError;
// anything else declines (returns nil) and the caller propagates the original
// error. Writes queue only on failure of a live attempt whose local half
// completed -- exactly the state D3 records.
func (k *Minder) maybeQueueWrite(
	m MetaContext,
	kvp *KVParty,
	sendErr error,
	eid proto.KVNodeID,
	key *kv.KeyBundle,
	rg proto.RoleAndGen,
	payload *lcl.KVOutboxPayload,
) error {
	if !core.IsTransportError(sendErr) {
		return nil
	}
	err := k.enqueueOutbox(m, kvp, eid, key, rg, payload)
	if err != nil {
		return err
	}
	m.Infow("kvOutbox", "stage", "queued", "party", kvp.Id(),
		"op", payload.Op, "err", sendErr)
	return core.KVWriteQueuedError{NodeID: payload.Nid}
}

// uploadNode puts one sealed small-file or symlink node. Identical replays
// are a server-side no-op, so the drain may re-issue this blindly (D5).
func (k *Minder) uploadNode(
	m MetaContext,
	kvp *KVParty,
	nid proto.KVNodeID,
	sfb proto.SmallFileBox,
) error {
	if err := k.preRPCHook("uploadNode"); err != nil {
		return err
	}
	auth, cli, err := k.client(m, kvp)
	if err != nil {
		return err
	}
	return cli.KvPutSmallFileOrSymlink(m.Ctx(), rem.KvPutSmallFileOrSymlinkArg{
		Auth: *auth,
		Id:   nid,
		Sfb:  sfb,
	})
}

// KVOutboxRow is one outbox entry, opened for listing.
type KVOutboxRow struct {
	Ord      uint64
	Eid      proto.KVNodeID
	State    lcl.KVOutboxState
	Attempts uint64
	// LastErr names the status code of the last drain rejection. It is a
	// code, not a message: see errToOutboxCode.
	LastErr string
	Op      lcl.KVOutboxOp
	Path    proto.KVPath
	Nid     proto.KVNodeID

	// Unreadable marks a row whose sealed payload would not open, so Op,
	// Path and Nid are unset. Listed anyway: it holds a slot against the
	// per-party cap and the user needs to be able to see it.
	Unreadable bool

	// IndexState is the state the index mirrors for this entry. It should
	// always equal State -- the two are written in one transaction -- and is
	// surfaced so a divergence is observable rather than silent.
	IndexState lcl.KVOutboxState
}

// outboxCodeString renders a persisted rejection code for display; an entry
// that has never been rejected has none.
func outboxCodeString(c proto.StatusCode) string {
	if c == proto.StatusCode_OK {
		return ""
	}
	if s, ok := proto.StatusCodeRevMap[c]; ok {
		return s
	}
	return fmt.Sprintf("status %d", int(c))
}

// ListOutbox opens and returns the party's outbox entries in ord order.
// Index entries whose row is missing (a crash mid-removal) are dropped here.
func (k *Minder) ListOutbox(
	m MetaContext,
	cfg lcl.KVConfig,
) (
	[]KVOutboxRow,
	error,
) {
	kvp, err := k.initReq(m, cfg)
	if err != nil {
		return nil, err
	}
	lock := kvOutboxLock(kvp)
	lock.Lock()
	defer lock.Unlock()

	idx, err := k.dbGetOutboxIndex(m, kvp)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(idx.Entries, func(a, b lcl.KVOutboxIndexEntry) int {
		return cmp.Compare(a.Ord, b.Ord)
	})
	var ret []KVOutboxRow
	var drop []proto.KVNodeID
	for _, ie := range idx.Entries {
		// A row that will not load or open must not hide the rest of the
		// outbox from the user (the librt outbox applies the same policy):
		// warn and skip, rather than failing the whole listing.
		ent, err := k.dbGetOutboxEntry(m, kvp, ie.Eid)
		if err != nil {
			m.Warnw("kvOutbox", "stage", "list-entry", "eid", ie.Eid, "err", err)
			continue
		}
		if ent == nil {
			drop = append(drop, ie.Eid)
			continue
		}
		// A row whose payload will not open still has to be listed: it
		// occupies a slot against maxKVOutboxEntries, so hiding it would
		// leave the user facing an "outbox full" they cannot account for or
		// clear. Report it with Unreadable set and the reason in LastError;
		// Op/Path/Nid stay zero because they live in the payload.
		payload, err := k.openOutboxPayload(m, kvp, ent)
		if err != nil {
			m.Warnw("kvOutbox", "stage", "list-open", "eid", ie.Eid, "err", err)
			ret = append(ret, KVOutboxRow{
				Ord:        ent.Ord,
				Eid:        ie.Eid,
				State:      ent.State,
				IndexState: ie.State,
				Attempts:   ent.Attempts,
				LastErr:    outboxCodeString(errToOutboxCode(err)),
				Unreadable: true,
			})
			continue
		}
		ret = append(ret, KVOutboxRow{
			Ord:        ent.Ord,
			Eid:        ie.Eid,
			State:      ent.State,
			IndexState: ie.State,
			Attempts:   ent.Attempts,
			LastErr:    outboxCodeString(ent.LastErrCode),
			Op:         payload.Op,
			Path:       payload.Path,
			Nid:        payload.Nid,
		})
	}
	for _, eid := range drop {
		err = k.removeOutbox(m, kvp, eid)
		if err != nil {
			return nil, err
		}
	}
	return ret, nil
}

// KVDrainResult summarizes one drain pass.
type KVDrainResult struct {
	Retired     int // landed (fresh or discovered already-landed) and removed
	StillQueued int // left queued: transport failure or contention
	Conflicted  int // CAS lost to another writer; held for the user (D6)
	Failed      int // semantic rejection (e.g. quota); held, not retried

	// Skipped is set when another drain for this party was already in
	// flight and this pass did nothing. Without it an all-zero result is
	// indistinguishable from "the outbox was empty".
	Skipped bool
}

// DrainOutbox attempts every queued entry, per-parent-directory FIFO. A
// transport failure stops the drain (the network is gone again); a conflicted
// or failed entry steps aside and the rest of its directory proceeds.
func (k *Minder) DrainOutbox(
	m MetaContext,
	cfg lcl.KVConfig,
) (
	*KVDrainResult,
	error,
) {
	kvp, err := k.initReq(m, cfg)
	if err != nil {
		return nil, err
	}
	flag := kvOutboxDrainFlag(kvp)
	if !flag.CompareAndSwap(false, true) {
		// A drain is already running for this party (most likely the
		// trigger); this pass has nothing separate to do. Report the skip
		// rather than zeros a caller would read as "outbox empty".
		return &KVDrainResult{Skipped: true}, nil
	}
	defer flag.Store(false)
	return k.drainOutboxWithParty(m, kvp)
}

// drainOutboxWithParty is the drain body. Callers hold the party's in-flight
// flag (never the outbox lock).
func (k *Minder) drainOutboxWithParty(
	m MetaContext,
	kvp *KVParty,
) (
	*KVDrainResult,
	error,
) {
	// The lock covers the index/entry read-modify-write cycles only, never the
	// drain's RPCs: an enqueue (a user write failing on transport) and a
	// ListOutbox must not block for the length of a whole drain. Concurrent
	// drains are already excluded by the per-party in-flight flag the caller
	// holds, so dropping the lock between phases is safe.
	lock := kvOutboxLock(kvp)

	// Phase 1, under the lock: snapshot the queued rows and drop orphans.
	type drainItem struct {
		eid     proto.KVNodeID
		ord     uint64
		payload *lcl.KVOutboxPayload
	}
	var items []drainItem
	snapshot := func() error {
		lock.Lock()
		defer lock.Unlock()

		idx, err := k.dbGetOutboxIndex(m, kvp)
		if err != nil {
			return err
		}
		var drop []proto.KVNodeID
		for _, ie := range idx.Entries {
			if ie.State != lcl.KVOutboxState_Queued {
				continue
			}
			// One row that will not load or open must not block delivery of
			// every other queued write: warn and skip it, leaving it queued
			// for a later pass, rather than aborting the drain. (Same policy
			// as the librt outbox's row enumeration.)
			ent, err := k.dbGetOutboxEntry(m, kvp, ie.Eid)
			if err != nil {
				m.Warnw("kvOutbox", "stage", "drain-entry", "eid", ie.Eid, "err", err)
				continue
			}
			if ent == nil {
				drop = append(drop, ie.Eid)
				continue
			}
			// The index's State is a mirror kept for cheap filtering; the
			// entry row is authoritative. setOutboxState writes the row
			// before the index, so a failure between the two can leave the
			// index claiming Queued for an entry the user is holding as
			// Conflicted -- and re-attempting that would apply a write D6
			// promised not to auto-resolve.
			if ent.State != lcl.KVOutboxState_Queued {
				continue
			}
			payload, err := k.openOutboxPayload(m, kvp, ent)
			if err != nil {
				m.Warnw("kvOutbox", "stage", "drain-open", "eid", ie.Eid, "err", err)
				continue
			}
			items = append(items, drainItem{eid: ie.Eid, ord: ent.Ord, payload: payload})
		}
		for _, eid := range drop {
			err = k.removeOutbox(m, kvp, eid)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := snapshot(); err != nil {
		return nil, err
	}

	// Each mutation below re-takes the lock for its own read-modify-write.
	withLock := func(f func() error) error {
		lock.Lock()
		defer lock.Unlock()
		return f()
	}

	// Per-parent-directory FIFO (docs/kv_offline.md D8 ordering note): group
	// by parent path, ascending ord within a group.
	groups := make(map[string][]drainItem)
	var order []string
	for _, it := range items {
		pap, err := kv.ParseAbsPath(it.payload.Path)
		if err != nil {
			return nil, err
		}
		parent, _, err := pap.Split()
		if err != nil {
			return nil, err
		}
		key := string(parent.Export())
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], it)
	}
	slices.Sort(order)

	// settleHint re-derives the trigger hint from durable state, under the
	// lock so a concurrent enqueue cannot be shadowed by a stale "empty".
	settleHint := func() {
		lock.Lock()
		defer lock.Unlock()
		work, err := k.hasOutboxWork(m, kvp)
		if err != nil {
			// Don't clear the hint on a read we could not make: leave it
			// Unknown so the next operation probes again.
			m.Warnw("kvOutbox", "stage", "settle-hint", "party", kvp.Id(), "err", err)
			kvOutboxHint(kvp).Store(kvOutboxHintUnknown)
			return
		}
		if work {
			kvOutboxHint(kvp).Store(kvOutboxHintWork)
		} else {
			kvOutboxHint(kvp).Store(kvOutboxHintEmpty)
		}
	}

	var res KVDrainResult
	for _, gk := range order {
		g := groups[gk]
		slices.SortFunc(g, func(a, b drainItem) int {
			return cmp.Compare(a.ord, b.ord)
		})
		for _, it := range g {
			outcome, derr := k.drainOne(m, kvp, it.payload)
			switch outcome {
			case drainRetired:
				err := withLock(func() error {
					return k.removeOutbox(m, kvp, it.eid)
				})
				if err != nil {
					return nil, err
				}
				res.Retired++
			case drainTransport:
				// The network is gone (again); everything still queued
				// stays queued, and this pass is over.
				res.StillQueued = len(items) - res.Retired - res.Conflicted - res.Failed
				settleHint()
				return &res, nil
			case drainRetryLater:
				// Contention, not a conflict: leave the row Queued so a
				// later pass retries it (see drainOne) -- but record the
				// attempt, so the reason is visible in ListOutbox and so an
				// entry that never converges eventually stops costing every
				// read a full cache-race budget.
				var attempts uint64
				err := withLock(func() error {
					var e error
					attempts, e = k.noteOutboxAttempt(m, kvp, it.eid, derr)
					return e
				})
				if err != nil {
					return nil, err
				}
				if attempts >= maxKVDrainRetries {
					// noteOutboxAttempt already counted this attempt;
					// setOutboxState must not count it a second time.
					err = withLock(func() error {
						return k.setOutboxStateNoCount(m, kvp, it.eid,
							lcl.KVOutboxState_Failed, derr)
					})
					if err != nil {
						return nil, err
					}
					m.Warnw("kvOutbox", "stage", "retry-exhausted",
						"party", kvp.Id(), "attempts", attempts, "err", derr)
					res.Failed++
					break
				}
				m.Infow("kvOutbox", "stage", "retry-later",
					"party", kvp.Id(), "attempts", attempts, "err", derr)
				res.StillQueued++
			case drainConflicted:
				err := withLock(func() error {
					return k.setOutboxState(m, kvp, it.eid,
						lcl.KVOutboxState_Conflicted, derr)
				})
				if err != nil {
					return nil, err
				}
				res.Conflicted++
			case drainFailed:
				err := withLock(func() error {
					return k.setOutboxState(m, kvp, it.eid,
						lcl.KVOutboxState_Failed, derr)
				})
				if err != nil {
					return nil, err
				}
				res.Failed++
			}
		}
	}
	settleHint()
	return &res, nil
}

type drainOutcome int

const (
	drainRetired    drainOutcome = 0
	drainTransport  drainOutcome = 1
	drainConflicted drainOutcome = 2
	drainFailed     drainOutcome = 3
	// drainRetryLater: the attempt lost to cache-coherence churn rather than
	// to another writer's content. The entry stays Queued for a later pass;
	// parking it in the terminal Conflicted state would strand a valid write
	// on transient contention.
	drainRetryLater drainOutcome = 4
)

// drainOne runs a single queued write through the normal online machinery.
func (k *Minder) drainOne(
	m MetaContext,
	kvp *KVParty,
	payload *lcl.KVOutboxPayload,
) (
	drainOutcome,
	error,
) {
	var err error
	switch payload.Op {
	case lcl.KVOutboxOp_PutSmallFile, lcl.KVOutboxOp_PutSymlink:
		err = k.drainPut(m, kvp, payload)
	case lcl.KVOutboxOp_Mkdir:
		err = k.drainMkdir(m, kvp, payload)
	case lcl.KVOutboxOp_Unlink:
		err = k.drainUnlink(m, kvp, payload)
	default:
		return drainFailed, core.InternalError("unknown outbox op")
	}
	if err == nil {
		return drainRetired, nil
	}
	if core.IsTransportError(err) {
		return drainTransport, err
	}

	// Exhausted cache-race retries are not a conflict. cacheRaceLoop returns
	// a bare KVStaleCacheError after clearing and retrying its budget, which
	// means the directory kept moving under us -- another device writing
	// steadily, say -- not that someone else holds this name. Leave the entry
	// queued for the next pass rather than parking it in Conflicted, which no
	// drain ever revisits (D6).
	var staleCache core.KVStaleCacheError
	if errors.As(err, &staleCache) {
		return drainRetryLater, err
	}

	// A CAS-class rejection: ask whether our own write already landed (D5).
	// For a put, the current value equal to our node ID is an earlier attempt
	// whose ack was lost; for an unlink, drainUnlink already treats a
	// tombstoned dirent as done, so any rejection reaching here is a real
	// conflict.
	if isDrainRaceError(err) && payload.Op != lcl.KVOutboxOp_Unlink {
		landed, lerr := k.outboxWriteLanded(m, kvp, payload)
		if lerr != nil {
			if core.IsTransportError(lerr) {
				return drainTransport, lerr
			}
			return drainFailed, lerr
		}
		if landed {
			return drainRetired, nil
		}
		return drainConflicted, err
	}
	if isDrainRaceError(err) {
		return drainConflicted, err
	}

	// A queued unlink whose target is not there at all has its desired end
	// state already: the walk reports KVNoentError when no dirent exists,
	// which is the same "nothing left to write" that drainUnlink recognizes
	// one step later for a tombstone. Retire it rather than parking a
	// not-found in the terminal Failed state.
	var noent core.KVNoentError
	if payload.Op == lcl.KVOutboxOp_Unlink && errors.As(err, &noent) {
		return drainRetired, nil
	}
	return drainFailed, err
}

// isDrainRaceError classifies the rejections that mean "another writer holds
// this name": the client-side CAS (KVRaceError) and an exists-rejection on an
// expect-absent put. KVStaleCacheError is deliberately NOT here -- the
// server's version-vector rejection is handled inside cacheRaceLoop, which
// clears and retries, and only surfaces once its budget is spent. That is
// contention, not a conflict, and drainOne routes it to drainRetryLater.
func isDrainRaceError(err error) bool {
	var race core.KVRaceError
	if errors.As(err, &race) {
		return true
	}
	var exists core.KVExistsError
	return errors.As(err, &exists)
}

// drainPut re-runs a queued put: node upload (an identical replay is a
// server-side no-op), then the shared link path.
func (k *Minder) drainPut(
	m MetaContext,
	kvp *KVParty,
	payload *lcl.KVOutboxPayload,
) error {
	err := k.uploadNode(m, kvp, payload.Nid, payload.Sfb)
	if err != nil {
		return err
	}
	return k.drainLink(m, kvp, payload)
}

// drainMkdir re-runs a queued directory creation: the sealed dir row (an
// identical replay is a server-side no-op), then the link, exactly as a put
// links its node.
func (k *Minder) drainMkdir(
	m MetaContext,
	kvp *KVParty,
	payload *lcl.KVOutboxPayload,
) error {
	if payload.Dir == nil {
		return core.InternalError("mkdir outbox entry with no dir")
	}
	err := k.uploadDir(m, kvp, payload.Dir)
	if err != nil {
		return err
	}
	return k.drainLink(m, kvp, payload)
}

// drainLink links a queued node or dir into its parent through the normal
// online path with the queue-time version as the CAS assertion. prepareDirent
// re-derives name MAC, name box, dirVersion and binding MAC from the
// directory as loaded now -- D4's re-preparation, falling out of the
// ordinary machinery.
func (k *Minder) drainLink(
	m MetaContext,
	kvp *KVParty,
	payload *lcl.KVOutboxPayload,
) error {
	pap, err := kv.ParseAbsPath(payload.Path)
	if err != nil {
		return err
	}
	parent, file, err := pap.Split()
	if err != nil {
		return err
	}
	rp := proto.RolePair{Read: payload.ReadRole, Write: payload.WriteRole}
	dv := payload.DirentVers
	return k.retryCacheLoop(m, kvp, func(m MetaContext) error {
		dp, err := k.walkFromRoot(m, kvp, parent, walkOpts{})
		if err != nil {
			return err
		}
		if dp.dir == nil {
			return core.InternalError("unexpected nil directory")
		}
		lno := linkNodeOpts{
			perms:       rp,
			overwriteOk: payload.OverwriteOk,
			direntVers:  &dv,
		}
		_, err = k.linkNode(m, kvp, dp.dir, file, payload.Nid, lno)
		return err
	})
}

// drainUnlink re-runs a queued unlink with the queue-time version as the CAS
// assertion. A dirent already tombstoned is done -- ours landed earlier, or
// someone else produced the state we wanted; either way there is nothing left
// to write.
func (k *Minder) drainUnlink(
	m MetaContext,
	kvp *KVParty,
	payload *lcl.KVOutboxPayload,
) error {
	pap, err := kv.ParseAbsPath(payload.Path)
	if err != nil {
		return err
	}
	rp := proto.RolePair{Read: payload.ReadRole, Write: payload.WriteRole}
	return k.retryCacheLoop(m, kvp, func(m MetaContext) error {
		wr, err := k.walkFromRoot(m, kvp, pap, walkOpts{unlink: true})
		if err != nil {
			return err
		}
		lde := wr.lde
		if lde == nil || lde.found == nil {
			return core.InternalError("expected lde.found")
		}
		if lde.found.Value.IsTombstone() {
			return nil
		}
		if lde.found.Version != payload.DirentVers {
			return core.KVRaceError("dirent")
		}
		tmp, err := lde.found.edit(m, Tombstone(), linkNodeOpts{perms: rp}, lde.newKb)
		if err != nil {
			return err
		}
		return k.putDirent(m, kvp, []*Dirent{tmp})
	})
}

// outboxWriteLanded reports whether the dirent at the payload's path already
// carries the payload's node ID -- the signature of our own earlier attempt
// having landed without its ack (D5).
func (k *Minder) outboxWriteLanded(
	m MetaContext,
	kvp *KVParty,
	payload *lcl.KVOutboxPayload,
) (
	bool,
	error,
) {
	pap, err := kv.ParseAbsPath(payload.Path)
	if err != nil {
		return false, err
	}
	parent, file, err := pap.Split()
	if err != nil {
		return false, err
	}
	var landed bool
	err = k.retryCacheLoop(m, kvp, func(m MetaContext) error {
		dp, err := k.walkFromRoot(m, kvp, parent, walkOpts{})
		if err != nil {
			return err
		}
		// walkRes.dir is nil whenever the walk ended at a leaf rather than a
		// directory -- reachable here if the parent path stopped being a
		// directory during the offline window. lookupDirent dereferences it
		// unconditionally, so guard as every other call site does.
		if dp.dir == nil {
			return core.InternalError("unexpected nil directory")
		}
		lde, err := k.lookupDirent(m, kvp, dp.dir, file, lookupDirentOpts{forPut: true})
		if err != nil {
			return err
		}
		landed = lde.found != nil && lde.found.Value == payload.Nid
		return nil
	})
	if err != nil {
		return false, err
	}
	return landed, nil
}
