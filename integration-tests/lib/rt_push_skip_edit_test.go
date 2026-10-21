// Edits and deletes never push (fork; the Notifications blueprint's "message
// edited or deleted: no notification of any kind").
//
// A send whose type is Edit or Delete skips the push_outbox fan-out and
// nothing else: the message is stored and every member's inbox version is
// bumped, so online delivery is unchanged. The app deletes by editing to an
// empty body, so both cases here are Edit sends; Delete, which librt cannot
// send, is covered by TestTypeWakesPhone in server/realtime.
package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// setupEditScene is a privScene with one open channel made by bob, checked
// against the realtime invariants at the end: the skip returns early from the
// function that also stamps memberships, so a bump without a stamp must fail.
func setupEditScene(t *testing.T) (*privScene, proto.RTChannelID) {
	sc := setupPrivScene(t, true)
	t.Cleanup(func() { sc.requireRTInvariants(t) })
	return sc, sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
}

// msgIDAt is the id of the message at seq in chid, read as `by`: the peg
// target an Edit or Reactji needs.
func (s *privScene) msgIDAt(
	t *testing.T, by *privActor, chid proto.RTChannelID, seq proto.RTMsgSeq,
) proto.RTMsgID {
	msgs, err := recents(by.m, by.minder, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), 0)
	require.NoError(t, err)
	for _, msg := range msgs {
		if msg.Seq == seq {
			return msg.MsgID
		}
	}
	require.FailNow(t, "message not found", "seq %d", seq)
	return proto.RTMsgID{}
}

func (s *privScene) sendTypedSeq(
	t *testing.T, by *privActor, chid proto.RTChannelID, body string,
	typ proto.RTMsgType, target proto.RTMsgID,
) proto.RTMsgSeq {
	res, err := by.minder.SendTyped(by.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), []byte(body), typ, &target)
	require.NoError(t, err)
	return res.Seq
}

// An edit and a delete queue no push row for anyone, while a message and a
// reaction in the same channel still queue one per recipient, and the edits
// still bump the recipients' inbox versions.
func TestEditQueuesNoPushRows(t *testing.T) {
	sc, ch := setupEditScene(t)
	recipients := []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie}

	s1 := sc.sendSeq(t, sc.bob, ch, "lunch?")
	target := sc.msgIDAt(t, sc.bob, ch, s1)
	for _, a := range recipients {
		require.Equal(t, 1, sc.pushRowsFor(t, ch, a), "a message pushes")
	}

	before := map[*privActor]int64{}
	for _, a := range recipients {
		before[a] = sc.inboxVersion(t, a)
	}
	sc.sendTypedSeq(t, sc.bob, ch, "lunch at 1?", proto.RTMsgType_Edit, target)
	sc.sendTypedSeq(t, sc.bob, ch, "", proto.RTMsgType_Edit, target) // the app's delete
	for _, a := range recipients {
		require.Equal(t, 1, sc.pushRowsFor(t, ch, a), "an edit or delete must not push")
		require.Greater(t, sc.inboxVersion(t, a), before[a],
			"an edit must still bump the inbox version (online delivery)")
	}

	sc.sendTypedSeq(t, sc.cleo, ch, `{"emoji":"👍"}`, proto.RTMsgType_Reactji, target)
	require.Equal(t, 0, sc.pushRowsFor(t, ch, sc.cleo),
		"the reactor is not pushed to, and reacting read the channel")
	require.Equal(t, 1, sc.pushRowsFor(t, ch, sc.bob), "a reaction still pushes")
	for _, a := range []*privActor{sc.alice, sc.dara, sc.eddie} {
		require.Equal(t, 2, sc.pushRowsFor(t, ch, a), "a reaction still pushes")
	}
}

// Under a push hold an edit writes no held row, and the daemon's drop pair
// for the edit's seq, which now matches nothing, is accepted and changes
// nothing.
func TestEditUnderHoldWritesNoHeldRow(t *testing.T) {
	sc, ch := setupEditScene(t)
	sc.setHold(t, sc.dara)

	s1 := sc.sendSeq(t, sc.bob, ch, "lunch?")
	s2 := sc.sendTypedSeq(t, sc.bob, ch, "lunch at 1?", proto.RTMsgType_Edit,
		sc.msgIDAt(t, sc.bob, ch, s1))
	recipients := []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie}
	for _, a := range recipients {
		require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, a))
	}

	require.NoError(t, sc.dara.minder.ReleasePushes(sc.dara.m, ch, s2, nil,
		[]rem.RTPushDrop{{Uid: sc.eddie.u.uid, Seq: s2}}))
	for _, a := range recipients {
		require.Equal(t, []int64{s1.Int64()}, sc.queuedSeqs(t, ch, a))
	}
}

// Sending an edit still counts as reading: the sender's queued pushes for
// what came before are settled, as for any send.
func TestEditSettlesSendersPushes(t *testing.T) {
	sc, ch := setupEditScene(t)

	s1 := sc.sendSeq(t, sc.bob, ch, "lunch?")
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, ch, sc.cleo))

	sc.sendTypedSeq(t, sc.cleo, ch, "lunch at 1?", proto.RTMsgType_Edit,
		sc.msgIDAt(t, sc.cleo, ch, s1))
	require.Empty(t, sc.pushRowsByStatus(t, ch, sc.cleo),
		"the edit's sender has read the channel, so their push is dropped")
}
