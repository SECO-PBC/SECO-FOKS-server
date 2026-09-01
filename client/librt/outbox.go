// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"errors"
	"sync"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

// The client-side durable outbox (docs/rt_offline.md, D2). A send is written
// here, sealed, before its first network attempt, and deleted only after the
// server's ack is reflected into the thread cache. Between those two points
// the message survives transport failures, process restarts, and crashes at
// any step: re-attempting is always safe because rtSend is idempotent on
// msgID (D1), so a retry of a send that actually landed converges to the
// original seq.
//
// Storage layout, all soft state under the party scope (the same scope as the
// thread cache):
//   - DataType_RTOutboxEntry, key = msgID: one lcl.RTOutboxEntry per queued or
//     failed message, holding the sealed proto.RTMsgCached exactly as it will
//     be sent. Plaintext never hits the local DB.
//   - DataType_RTOutboxIndex, key = EmptyKey: the index of entries plus the
//     monotonic queue-order allocator, since the KV layer can't enumerate keys
//     in a scope (the RTInboxSyncState pattern).
//
// Enqueue writes the entry row and the index in one DbPutTx. Removal deletes
// the row first and rewrites the index second, so a crash between the two
// leaves an index entry whose row is missing -- dropped, self-healing, on the
// next drain. The reverse order would leave an unreachable orphan row.
//
// Ordering is per-channel FIFO by ord. The drain sends one message per
// channel at a time and does not advance past a still-queued row; a failed
// row (semantic rejection) steps aside so it doesn't hold the channel
// hostage, and is retried only explicitly. Distinct channels drain
// concurrently.

// maxOutboxPerChannel bounds queued rows per channel. At capacity, sends fail
// fast with RTOutboxFullError rather than silently shedding a queued message.
const maxOutboxPerChannel = 256

type outboxScopeT = proto.FQParty

func (d *Minder) outboxScope() outboxScopeT {
	return d.au.FQParty()
}

// dbGetOutboxIndex loads the outbox index; a missing row is an empty outbox.
// Callers mutating outbox state must hold d.outboxMu.
func (d *Minder) dbGetOutboxIndex(m MetaContext) (lcl.RTOutboxIndex, error) {
	var ret lcl.RTOutboxIndex
	scope := d.outboxScope()
	_, err := m.DbGet(&ret, libclient.DbTypeSoft, &scope,
		lcl.DataType_RTOutboxIndex, core.EmptyKey{})
	if errors.Is(err, core.RowNotFoundError{}) {
		return lcl.RTOutboxIndex{}, nil
	}
	if err != nil {
		return ret, err
	}
	return ret, nil
}

func (d *Minder) dbGetOutboxEntry(
	m MetaContext,
	msgID proto.RTMsgID,
) (
	*lcl.RTOutboxEntry,
	error,
) {
	var ret lcl.RTOutboxEntry
	scope := d.outboxScope()
	_, err := m.DbGet(&ret, libclient.DbTypeSoft, &scope,
		lcl.DataType_RTOutboxEntry, msgID)
	if errors.Is(err, core.RowNotFoundError{}) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

// dbPutOutbox writes an entry row and the index atomically.
func (d *Minder) dbPutOutbox(
	m MetaContext,
	entry *lcl.RTOutboxEntry,
	idx *lcl.RTOutboxIndex,
) error {
	scope := d.outboxScope()
	args := []libclient.PutArg{
		{
			Scope: &scope,
			Typ:   lcl.DataType_RTOutboxEntry,
			Key:   entry.Msg.Md.Md.MsgID,
			Val:   entry,
		},
		{
			Scope: &scope,
			Typ:   lcl.DataType_RTOutboxIndex,
			Key:   core.EmptyKey{},
			Val:   idx,
		},
	}
	return m.DbPutTx(libclient.DbTypeSoft, args)
}

func (d *Minder) dbPutOutboxIndex(m MetaContext, idx *lcl.RTOutboxIndex) error {
	scope := d.outboxScope()
	return m.DbPut(libclient.DbTypeSoft, libclient.PutArg{
		Scope: &scope,
		Typ:   lcl.DataType_RTOutboxIndex,
		Key:   core.EmptyKey{},
		Val:   idx,
	})
}

// enqueueOutbox durably queues a sealed message. It returns the entry's queue
// ord and whether earlier rows are already queued for the same channel (in
// which case the caller must not race past them to the server -- no
// queue-jumping, D2). Fails fast with RTOutboxFullError at the per-channel
// cap.
func (d *Minder) enqueueOutbox(
	m MetaContext,
	msgCached proto.RTMsgCached,
) (
	hasEarlier bool,
	err error,
) {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()

	chid := msgCached.Md.Chid
	idx, err := d.dbGetOutboxIndex(m)
	if err != nil {
		return false, err
	}
	nQueued := 0
	for _, e := range idx.Entries {
		if e.Chid == chid {
			if e.State == lcl.RTOutboxState_Queued {
				nQueued++
				hasEarlier = true
			}
		}
	}
	if nQueued >= maxOutboxPerChannel {
		return false, core.RTOutboxFullError{}
	}

	entry := lcl.RTOutboxEntry{
		Msg:   msgCached,
		State: lcl.RTOutboxState_Queued,
		Ord:   idx.NextOrd,
	}
	idx.Entries = append(idx.Entries, lcl.RTOutboxIndexEntry{
		MsgID: msgCached.Md.Md.MsgID,
		Chid:  chid,
		Ord:   idx.NextOrd,
		State: lcl.RTOutboxState_Queued,
	})
	idx.NextOrd++

	err = d.dbPutOutbox(m, &entry, &idx)
	if err != nil {
		return false, err
	}

	// Drop any legacy pre-drain outbox row for this channel (the old format
	// keyed rows by chid; nothing ever drained them, so each is either the
	// orphan of an acked send or a failure its caller already observed).
	scope := d.outboxScope()
	_ = m.G().DbDelete(m.Ctx(), libclient.DbTypeSoft, &scope,
		lcl.DataType_RTOutboxMsg, chid)

	return hasEarlier, nil
}

// removeOutbox deletes a delivered or discarded entry: row first, index
// second, so a crash in between self-heals (see the layout comment above).
func (d *Minder) removeOutbox(m MetaContext, msgID proto.RTMsgID) error {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()
	return d.removeOutboxLocked(m, msgID)
}

func (d *Minder) removeOutboxLocked(m MetaContext, msgID proto.RTMsgID) error {
	scope := d.outboxScope()
	err := m.G().DbDelete(m.Ctx(), libclient.DbTypeSoft, &scope,
		lcl.DataType_RTOutboxEntry, msgID)
	if err != nil && !errors.Is(err, core.RowNotFoundError{}) {
		return err
	}
	idx, err := d.dbGetOutboxIndex(m)
	if err != nil {
		return err
	}
	kept := idx.Entries[:0]
	for _, e := range idx.Entries {
		if e.MsgID != msgID {
			kept = append(kept, e)
		}
	}
	idx.Entries = kept
	return d.dbPutOutboxIndex(m, &idx)
}

// setOutboxState updates an entry after an attempt: attempts++, the error
// recorded, and (for a semantic rejection) state flipped to Failed, mirrored
// into the index.
func (d *Minder) setOutboxState(
	m MetaContext,
	msgID proto.RTMsgID,
	state lcl.RTOutboxState,
	attemptErr error,
) error {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()

	entry, err := d.dbGetOutboxEntry(m, msgID)
	if err != nil {
		return err
	}
	if entry == nil {
		return core.RowNotFoundError{}
	}
	entry.State = state
	entry.Attempts++
	if attemptErr != nil {
		entry.LastError = attemptErr.Error()
	} else {
		entry.LastError = ""
	}
	idx, err := d.dbGetOutboxIndex(m)
	if err != nil {
		return err
	}
	for i := range idx.Entries {
		if idx.Entries[i].MsgID == msgID {
			idx.Entries[i].State = state
		}
	}
	return d.dbPutOutbox(m, entry, &idx)
}

// attemptSend issues the rtSend RPC for a sealed message. Everything the wire
// call needs travels inside the stored RTMsgCached, so the drain never has to
// re-resolve teams or channels.
func (d *Minder) attemptSend(
	m MetaContext,
	msgCached *proto.RTMsgCached,
) (
	*rem.RTSendRes,
	error,
) {
	arg := rem.RTSendArg{
		Md:   msgCached.Md.Md,
		Mw:   msgCached.Mw,
		Chid: msgCached.Md.Chid.Short(),
	}
	if d.testHooks != nil && d.testHooks.SendRPC != nil {
		return d.testHooks.SendRPC(m, arg)
	}
	_, cli, err := d.clientLocal(m.Base(), d.au)
	if err != nil {
		return nil, err
	}
	res, err := cli.RtSend(m.Ctx(), arg)
	if d.testHooks != nil && d.testHooks.MutateSendOutcome != nil {
		resp, err := d.testHooks.MutateSendOutcome(&res, err)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// finalizeAck reflects a server ack into the thread cache and removes the
// outbox row -- strictly in that order, so a crash in between re-sends and
// converges via the replay path rather than losing the message.
//
// No prev-seq order check here: a drained message's prev pointers were frozen
// at queue time and the protocol explicitly permits them to be stale (D3).
// Seq/msgID consistency is still enforced: if the cache already holds this
// seq, it must carry this msgID (then the validated original is kept), and
// anything else is server misbehavior.
func (d *Minder) finalizeAck(
	m MetaContext,
	msgCached proto.RTMsgCached,
	res *rem.RTSendRes,
) error {
	msgID := msgCached.Md.Md.MsgID
	chid := msgCached.Md.Chid

	err := d.cacheMsgID(res.Seq, msgID)
	if err != nil {
		return err
	}

	cached, err := dbGetMsgs(m, d.au, chid, res.Seq, res.Seq)
	if err != nil {
		return err
	}
	switch {
	case len(cached) == 1 && cached[0].Cm.Md.Md.MsgID == msgID:
		// A replay of a delivery the cache already ingested (and validated);
		// keep the original copy.
	case len(cached) == 1:
		return core.RTMsgOrderError(
			"server acked a sequence already held by a different message",
		)
	default:
		msgCached.Sit = res.InsertTime
		err = d.dbPutMsgs(m, []proto.RTMsgCachedWithSeq{{
			Cm:  msgCached,
			Seq: res.Seq,
		}})
		if err != nil {
			return err
		}
	}

	return d.removeOutbox(m, msgID)
}

// DrainResult reports what a drain pass did.
type DrainResult struct {
	// Acked maps each delivered msgID to its server result (fresh or replay).
	Acked map[proto.RTMsgID]*rem.RTSendRes
	// Failed lists msgIDs marked Failed by a semantic rejection this pass.
	Failed []proto.RTMsgID
	// TransportErr is the first transport-level failure, if the pass ended
	// early because the server was unreachable; the remaining rows stay
	// queued.
	TransportErr error
}

// snapshotQueue groups the current index by channel, each channel's rows in
// ord (FIFO) order, dropping index entries whose row is missing (the
// self-heal for a crash between an ack's row delete and index rewrite).
func (d *Minder) snapshotQueue(
	m MetaContext,
	only *proto.RTChannelID,
) (
	map[proto.RTChannelID][]lcl.RTOutboxEntry,
	error,
) {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()

	idx, err := d.dbGetOutboxIndex(m)
	if err != nil {
		return nil, err
	}
	ret := make(map[proto.RTChannelID][]lcl.RTOutboxEntry)
	var dropped bool
	for _, e := range idx.Entries {
		if only != nil && e.Chid != *only {
			continue
		}
		if e.State != lcl.RTOutboxState_Queued {
			continue
		}
		entry, err := d.dbGetOutboxEntry(m, e.MsgID)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			err = d.removeOutboxLocked(m, e.MsgID)
			if err != nil {
				return nil, err
			}
			dropped = true
			continue
		}
		ret[e.Chid] = append(ret[e.Chid], *entry)
	}
	_ = dropped
	for _, lst := range ret {
		entries := lst
		for i := 1; i < len(entries); i++ {
			for j := i; j > 0 && entries[j].Ord < entries[j-1].Ord; j-- {
				entries[j], entries[j-1] = entries[j-1], entries[j]
			}
		}
	}
	return ret, nil
}

// drainChannelRows walks one channel's queued rows in FIFO order. A transport
// error stops the channel (everything behind stays queued for the next
// trigger); a semantic rejection marks the row Failed and steps past it, so
// one rejected message doesn't hold the channel hostage.
func (d *Minder) drainChannelRows(
	m MetaContext,
	rows []lcl.RTOutboxEntry,
	res *DrainResult,
	mu *sync.Mutex,
) {
	for _, row := range rows {
		ack, err := d.attemptSend(m, &row.Msg)
		msgID := row.Msg.Md.Md.MsgID
		if err != nil {
			if core.IsTransportError(err) {
				_ = d.setOutboxState(m, msgID, lcl.RTOutboxState_Queued, err)
				mu.Lock()
				if res.TransportErr == nil {
					res.TransportErr = err
				}
				mu.Unlock()
				return
			}
			_ = d.setOutboxState(m, msgID, lcl.RTOutboxState_Failed, err)
			mu.Lock()
			res.Failed = append(res.Failed, msgID)
			mu.Unlock()
			continue
		}
		err = d.finalizeAck(m, row.Msg, ack)
		if err != nil {
			// The ack landed server-side; local bookkeeping failed. Leave the
			// row queued -- the next drain replays and converges.
			mu.Lock()
			if res.TransportErr == nil {
				res.TransportErr = err
			}
			mu.Unlock()
			return
		}
		mu.Lock()
		res.Acked[msgID] = ack
		mu.Unlock()
	}
}

// Drain attempts every queued outbox row, distinct channels concurrently,
// each channel strictly FIFO. Safe to call at any time and from any trigger
// (reconnect, client start, an explicit CLI drain): a row whose earlier
// attempt actually landed resolves as a replay. Concurrent drains of the same
// row are likewise convergent, if wasteful.
func (d *Minder) Drain(m MetaContext) (*DrainResult, error) {
	return d.drain(m, nil)
}

// DrainChannel is Drain for a single channel.
func (d *Minder) DrainChannel(
	m MetaContext,
	chid proto.RTChannelID,
) (
	*DrainResult,
	error,
) {
	return d.drain(m, &chid)
}

func (d *Minder) drain(
	m MetaContext,
	only *proto.RTChannelID,
) (
	*DrainResult,
	error,
) {
	snapshot, err := d.snapshotQueue(m, only)
	if err != nil {
		return nil, err
	}
	res := &DrainResult{Acked: make(map[proto.RTMsgID]*rem.RTSendRes)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, rows := range snapshot {
		wg.Add(1)
		go func(rows []lcl.RTOutboxEntry) {
			defer wg.Done()
			d.drainChannelRows(m, rows, res, &mu)
		}(rows)
	}
	wg.Wait()

	// Read-marks queued while offline replay on the same triggers (D5).
	err = d.drainPendingMarks(m, only)
	if err != nil && res.TransportErr == nil {
		res.TransportErr = err
	}
	return res, nil
}

// OutboxRow is one outbox entry as presented to the CLI / app layer.
type OutboxRow struct {
	MsgID     proto.RTMsgID
	Chid      proto.RTChannelID
	Ord       uint64
	State     lcl.RTOutboxState
	Attempts  uint64
	LastError string
	SendTime  proto.Time
}

// ListOutbox returns every outbox entry, queued and failed, in queue order.
func (d *Minder) ListOutbox(m MetaContext) ([]OutboxRow, error) {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()

	idx, err := d.dbGetOutboxIndex(m)
	if err != nil {
		return nil, err
	}
	var ret []OutboxRow
	for _, e := range idx.Entries {
		entry, err := d.dbGetOutboxEntry(m, e.MsgID)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			continue
		}
		ret = append(ret, OutboxRow{
			MsgID:     e.MsgID,
			Chid:      e.Chid,
			Ord:       entry.Ord,
			State:     entry.State,
			Attempts:  entry.Attempts,
			LastError: entry.LastError,
			SendTime:  entry.Msg.Md.Md.SendTime,
		})
	}
	for i := 1; i < len(ret); i++ {
		for j := i; j > 0 && ret[j].Ord < ret[j-1].Ord; j-- {
			ret[j], ret[j-1] = ret[j-1], ret[j]
		}
	}
	return ret, nil
}

// RetryOutbox re-queues a Failed entry (the manual escape hatch for a
// misclassified or transient rejection) and drains its channel.
func (d *Minder) RetryOutbox(
	m MetaContext,
	msgID proto.RTMsgID,
) (
	*DrainResult,
	error,
) {
	entry, err := d.dbGetOutboxEntry(m, msgID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, core.RowNotFoundError{}
	}
	err = d.setOutboxState(m, msgID, lcl.RTOutboxState_Queued, nil)
	if err != nil {
		return nil, err
	}
	return d.DrainChannel(m, entry.Msg.Md.Chid)
}

// DiscardOutbox drops an entry without sending it.
func (d *Minder) DiscardOutbox(m MetaContext, msgID proto.RTMsgID) error {
	return d.removeOutbox(m, msgID)
}

// pendingMsgViews decrypts the viewer's own queued/failed outbox messages for
// one channel into app-layer views, in queue order, with seq 0 (unassigned).
// The sender's own name is attached directly; no team-mediated resolution is
// needed for one's own messages.
func (d *Minder) pendingMsgViews(
	m MetaContext,
	rtp *RTParty,
	appID proto.RTAppID,
	chid proto.RTChannelID,
) (
	[]lcl.RTMsgView,
	error,
) {
	d.outboxMu.Lock()
	idx, err := d.dbGetOutboxIndex(m)
	if err != nil {
		d.outboxMu.Unlock()
		return nil, err
	}
	var entries []lcl.RTOutboxEntry
	for _, e := range idx.Entries {
		if e.Chid != chid {
			continue
		}
		entry, err := d.dbGetOutboxEntry(m, e.MsgID)
		if err != nil {
			d.outboxMu.Unlock()
			return nil, err
		}
		if entry == nil {
			continue
		}
		entries = append(entries, *entry)
	}
	d.outboxMu.Unlock()

	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Ord < entries[j-1].Ord; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}

	selfName := d.au.Info.Username.NameUtf8
	var ret []lcl.RTMsgView
	for _, e := range entries {
		body, _, err := d.openMessage(m, rtp, appID,
			e.Msg.Md.Md, e.Msg.Mw, e.Msg.Md.Sender, e.Msg.Sit, chid)
		if err != nil {
			m.Warnw("pendingMsgViews", "stage", "open", "err", err)
			continue
		}
		mv := lcl.RTMsgView{
			MsgID:      e.Msg.Md.Md.MsgID,
			PrevID:     e.Msg.Md.Md.PrevID,
			PrevSeq:    e.Msg.Md.Md.PrevSeq,
			Typ:        e.Msg.Md.Md.Typ,
			SentAtTime: e.Msg.Md.Md.SendTime,
			Body:       body,
			SenderName: &selfName,
		}
		if e.Msg.Md.Sender != nil {
			mv.Sender = &e.Msg.Md.Sender.Party
		}
		ret = append(ret, mv)
	}
	return ret, nil
}
