// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

// Read-marks queued while offline (docs/rt_offline.md, D5). A read-through
// that can't reach the server is persisted -- one entry per channel, holding
// only the highest locally-read seq, since intermediate marks are subsumed
// and the server pointer is monotonic (stale marks no-op). The outbox drain
// replays them; local unread counts consult them so a channel read offline
// doesn't keep showing unread on this device.
//
// All entries live in one singleton soft-DB row under the party scope --
// marks are one (chid, seq) pair per channel, so a single row keeps updates
// atomic without an index. Mutations run under the per-user outbox lock (see
// outbox.go), and every write compacts the set to one entry per channel, so a
// duplicate can never survive to make clear/render semantics diverge.
//
// Fork-only (cross-device-read-sync): an explicit mark -- the user read the
// channel, not a client marking a fetched page -- is its own entry, so a
// channel holds at most one explicit and one implicit mark, and an explicit
// read made offline replays as explicit.

// pendingKey is what a pending entry is unique by.
type pendingKey struct {
	chid     proto.RTChannelID
	explicit bool
}

func keyOf(e lcl.RTReadThroughPendingEntry) pendingKey {
	return pendingKey{chid: e.Chid, explicit: e.Explicit}
}

func (d *Minder) dbGetPendingMarks(m MetaContext) (lcl.RTReadThroughPendingSet, error) {
	return dbGetSingleton[lcl.RTReadThroughPendingSet](d, m, lcl.DataType_RTReadThroughPending)
}

func (d *Minder) dbPutPendingMarks(m MetaContext, set *lcl.RTReadThroughPendingSet) error {
	return d.dbPutSingleton(m, lcl.DataType_RTReadThroughPending, set)
}

// compactMarks collapses the set to at most one entry per (channel,
// explicit), keeping the highest seq. Defense against any historical
// duplicate: rendering takes the max and clearing acts on the canonical entry
// either way.
func compactMarks(set *lcl.RTReadThroughPendingSet) {
	seen := make(map[pendingKey]int, len(set.Entries))
	kept := set.Entries[:0]
	for _, e := range set.Entries {
		if i, ok := seen[keyOf(e)]; ok {
			if e.Seq > kept[i].Seq {
				kept[i].Seq = e.Seq
			}
			continue
		}
		seen[keyOf(e)] = len(kept)
		kept = append(kept, e)
	}
	set.Entries = kept
}

// queuePendingMark records that the viewer has read through seq in chid but
// the server doesn't know yet. Max-merge: an older pending mark is subsumed.
func (d *Minder) queuePendingMark(
	m MetaContext,
	chid proto.RTChannelID,
	seq proto.RTMsgSeq,
	explicit bool,
) error {
	lk, err := d.lockOutbox(m)
	if err != nil {
		return err
	}
	defer lk.Release()

	set, err := d.dbGetPendingMarks(m)
	if err != nil {
		return err
	}
	compactMarks(&set)
	for i := range set.Entries {
		if set.Entries[i].Chid == chid && set.Entries[i].Explicit == explicit {
			if set.Entries[i].Seq >= seq {
				return nil
			}
			set.Entries[i].Seq = seq
			return d.dbPutPendingMarks(m, &set)
		}
	}
	set.Entries = append(set.Entries, lcl.RTReadThroughPendingEntry{
		Chid:     chid,
		Seq:      seq,
		Explicit: explicit,
	})
	return d.dbPutPendingMarks(m, &set)
}

// clearPendingMarks drops the given pending marks, each up to the seq the
// server has been told about; a mark raised concurrently past its acked seq
// survives for the next replay. One locked read-modify-write for the whole
// batch.
func (d *Minder) clearPendingMarks(
	m MetaContext,
	acked map[pendingKey]proto.RTMsgSeq,
) error {
	if len(acked) == 0 {
		return nil
	}
	lk, err := d.lockOutbox(m)
	if err != nil {
		return err
	}
	defer lk.Release()

	set, err := d.dbGetPendingMarks(m)
	if err != nil {
		return err
	}
	compactMarks(&set)
	kept := set.Entries[:0]
	changed := false
	for _, e := range set.Entries {
		if seq, ok := acked[keyOf(e)]; ok && e.Seq <= seq {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	if !changed {
		return nil
	}
	set.Entries = kept
	return d.dbPutPendingMarks(m, &set)
}

// attemptReadThrough issues the rtReadThrough RPC (or the test hook).
func (d *Minder) attemptReadThrough(
	m MetaContext,
	chid proto.RTChannelID,
	seq proto.RTMsgSeq,
	explicit bool,
) error {
	arg := rem.RTReadThroughArg{
		ChannelID: chid,
		Seq:       seq,
		Explicit:  explicit,
	}
	if d.testHooks != nil && d.testHooks.ReadThroughRPC != nil {
		return d.testHooks.ReadThroughRPC(m, arg)
	}
	_, cli, err := d.clientLocal(m.Base(), d.au)
	if err != nil {
		return err
	}
	return cli.RtReadThrough(m.Ctx(), arg)
}

// drainPendingMarks replays queued read-marks, highest-per-channel, clearing
// the acked ones in one batched write. A transport error leaves the rest for
// the next trigger.
func (d *Minder) drainPendingMarks(
	m MetaContext,
	only *proto.RTChannelID,
) error {
	set, err := d.dbGetPendingMarks(m)
	if err != nil {
		return err
	}
	compactMarks(&set)
	acked := make(map[pendingKey]proto.RTMsgSeq)
	var replayErr error
	for _, e := range set.Entries {
		if only != nil && e.Chid != *only {
			continue
		}
		err := d.attemptReadThrough(m, e.Chid, e.Seq, e.Explicit)
		if err != nil {
			if core.IsTransportError(err) {
				replayErr = err
				break
			}
			// A semantic refusal of a read-mark has nothing to retry; drop it
			// rather than wedging the queue.
			m.Warnw("drainPendingMarks", "chid", e.Chid, "err", err)
		}
		acked[keyOf(e)] = e.Seq
	}
	err = d.clearPendingMarks(m, acked)
	if err != nil {
		return err
	}
	return replayErr
}
