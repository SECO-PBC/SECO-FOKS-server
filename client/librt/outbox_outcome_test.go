// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	rem "github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// TestOutcomeHandOff: a hand-off reaches every Send registered for the
// msgID (two Sends of one pinned msgID don't clobber each other), one Send's
// deregistration leaves the other's in place, and a hand-off nobody waits
// for is dropped.
func TestOutcomeHandOff(t *testing.T) {
	d := &Minder{}
	var id, other proto.RTMsgID
	id[0], other[0] = 1, 2

	w1 := d.awaitOutcome(id)
	w2 := d.awaitOutcome(id)
	_, ok := w1.take()
	require.False(t, ok)

	d.handOff(id, sendAcked(rem.RTSendRes{Seq: 7}))
	for _, w := range []*outcomeWaiter{w1, w2} {
		o, ok := w.take()
		require.True(t, ok)
		ack, err := o.result()
		require.NoError(t, err)
		require.Equal(t, proto.RTMsgSeq(7), ack.Seq)
	}

	w1.done()
	d.handOff(id, sendRefused(core.BadArgsError("no")))
	o, ok := w2.take()
	require.True(t, ok)
	ack, err := o.result()
	require.Nil(t, ack)
	require.ErrorIs(t, err, core.BadArgsError("no"))
	_, ok = w1.take()
	require.False(t, ok)

	w2.done()
	d.handOff(other, sendAcked(rem.RTSendRes{}))
	require.Empty(t, d.outcomes)
}

// TestSendRefusedNeverNilError: a refusal always carries an error, so a
// Send handed one never returns a nil ack with a nil error.
func TestSendRefusedNeverNilError(t *testing.T) {
	ack, err := sendRefused(nil).result()
	require.Nil(t, ack)
	require.Error(t, err)
}
