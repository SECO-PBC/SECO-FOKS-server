// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"errors"

	"github.com/foks-proj/go-foks/client/libclient"
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
// atomic without an index.

func (d *Minder) dbGetPendingMarks(m MetaContext) (lcl.RTReadThroughPendingSet, error) {
	var ret lcl.RTReadThroughPendingSet
	scope := d.outboxScope()
	_, err := m.DbGet(&ret, libclient.DbTypeSoft, &scope,
		lcl.DataType_RTReadThroughPending, core.EmptyKey{})
	if errors.Is(err, core.RowNotFoundError{}) {
		return lcl.RTReadThroughPendingSet{}, nil
	}
	if err != nil {
		return ret, err
	}
	return ret, nil
}

func (d *Minder) dbPutPendingMarks(m MetaContext, set *lcl.RTReadThroughPendingSet) error {
	scope := d.outboxScope()
	return m.DbPut(libclient.DbTypeSoft, libclient.PutArg{
		Scope: &scope,
		Typ:   lcl.DataType_RTReadThroughPending,
		Key:   core.EmptyKey{},
		Val:   set,
	})
}

// queuePendingMark records that the viewer has read through seq in chid but
// the server doesn't know yet. Max-merge: an older pending mark is subsumed.
func (d *Minder) queuePendingMark(
	m MetaContext,
	chid proto.RTChannelID,
	seq proto.RTMsgSeq,
) error {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()

	set, err := d.dbGetPendingMarks(m)
	if err != nil {
		return err
	}
	for i := range set.Entries {
		if set.Entries[i].Chid == chid {
			if set.Entries[i].Seq >= seq {
				return nil
			}
			set.Entries[i].Seq = seq
			return d.dbPutPendingMarks(m, &set)
		}
	}
	set.Entries = append(set.Entries, lcl.RTReadThroughPendingEntry{
		Chid: chid,
		Seq:  seq,
	})
	return d.dbPutPendingMarks(m, &set)
}

// clearPendingMark drops a channel's pending mark once the server has been
// told about a read-through at or past it.
func (d *Minder) clearPendingMark(
	m MetaContext,
	chid proto.RTChannelID,
	seq proto.RTMsgSeq,
) error {
	d.outboxMu.Lock()
	defer d.outboxMu.Unlock()

	set, err := d.dbGetPendingMarks(m)
	if err != nil {
		return err
	}
	for i := range set.Entries {
		if set.Entries[i].Chid == chid && set.Entries[i].Seq <= seq {
			set.Entries = append(set.Entries[:i], set.Entries[i+1:]...)
			return d.dbPutPendingMarks(m, &set)
		}
	}
	return nil
}

// pendingMarkFor returns the pending read-mark for a channel, or 0.
func (d *Minder) pendingMarkFor(
	m MetaContext,
	chid proto.RTChannelID,
) (
	proto.RTMsgSeq,
	error,
) {
	set, err := d.dbGetPendingMarks(m)
	if err != nil {
		return 0, err
	}
	for _, e := range set.Entries {
		if e.Chid == chid {
			return e.Seq, nil
		}
	}
	return 0, nil
}

// attemptReadThrough issues the rtReadThrough RPC (or the test hook).
func (d *Minder) attemptReadThrough(
	m MetaContext,
	chid proto.RTChannelID,
	seq proto.RTMsgSeq,
) error {
	arg := rem.RTReadThroughArg{
		ChannelID: chid,
		Seq:       seq,
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

// drainPendingMarks replays queued read-marks, highest-per-channel, deleting
// each on ack. A transport error leaves the rest for the next trigger.
func (d *Minder) drainPendingMarks(
	m MetaContext,
	only *proto.RTChannelID,
) error {
	set, err := d.dbGetPendingMarks(m)
	if err != nil {
		return err
	}
	for _, e := range set.Entries {
		if only != nil && e.Chid != *only {
			continue
		}
		err := d.attemptReadThrough(m, e.Chid, e.Seq)
		if err != nil {
			if core.IsTransportError(err) {
				return err
			}
			// A semantic refusal of a read-mark has nothing to retry; drop it
			// rather than wedging the queue.
			m.Warnw("drainPendingMarks", "chid", e.Chid, "err", err)
		}
		err = d.clearPendingMark(m, e.Chid, e.Seq)
		if err != nil {
			return err
		}
	}
	return nil
}
