// A send can name the one member it pushes (fork; reaction-push-to-author).
//
// The app sends an added reaction this way, for the author of the message it
// reacts to: without it, every member of a channel no daemon holds pushes for
// was woken by "X reacted". The fan-out is narrowed, nothing else: the
// message is stored and every member's inbox version is bumped as usual.
package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

const reactionOn = `{"e":"8J+Rjw==","on":true}`

func (s *privScene) sendPushTo(
	t *testing.T, by *privActor, chid proto.RTChannelID, target proto.RTMsgID, to *privActor,
) {
	_, err := by.minder.SendTypedPushTo(by.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), []byte(reactionOn),
		proto.RTMsgType_Reactji, &target, to.u.uid)
	require.NoError(t, err)
}

// pushCounts is the push row count in chid of everyone but bob, by name.
// Bob sends in these tests, and a send marks the sender's own earlier pushes
// in the channel read (cross-device-read-state), so his count moves for a
// reason of its own.
func (s *privScene) pushCounts(t *testing.T, chid proto.RTChannelID) map[string]int {
	out := map[string]int{}
	for _, a := range []*privActor{s.alice, s.cleo, s.dara, s.eddie} {
		out[string(a.u.name)] = s.pushRowsFor(t, chid, a)
	}
	return out
}

// Only the named member gets a row; every member's inbox is still bumped.
func TestPushToWakesOnlyThatMember(t *testing.T) {
	sc, ch := setupEditScene(t)
	s1 := sc.sendSeq(t, sc.cleo, ch, "seeds are in")
	target := sc.msgIDAt(t, sc.bob, ch, s1)

	rows := sc.pushCounts(t, ch)
	inbox := map[*privActor]int64{}
	for _, a := range []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie} {
		inbox[a] = sc.inboxVersion(t, a)
	}
	sc.sendPushTo(t, sc.bob, ch, target, sc.cleo)

	rows[string(sc.cleo.u.name)]++
	require.Equal(t, rows, sc.pushCounts(t, ch), "only the named member is pushed")
	for a, v := range inbox {
		require.Greater(t, sc.inboxVersion(t, a), v, "every member's inbox is still bumped")
	}
}

// Naming the sender, or a member who muted the channel, writes no row.
func TestPushToSenderOrMutedMemberWakesNobody(t *testing.T) {
	sc, ch := setupEditScene(t)
	s1 := sc.sendSeq(t, sc.bob, ch, "seeds are in")
	target := sc.msgIDAt(t, sc.bob, ch, s1)

	rows := sc.pushCounts(t, ch)
	sc.sendPushTo(t, sc.bob, ch, target, sc.bob)
	require.Equal(t, rows, sc.pushCounts(t, ch), "naming the sender pushes nobody")

	require.NoError(t, sc.setMuted(t, sc.cleo, ch, true))
	sc.sendPushTo(t, sc.bob, ch, target, sc.cleo)
	require.Equal(t, rows, sc.pushCounts(t, ch), "naming a muted member pushes nobody")
}

// Under a push hold the named member's row is held, and nobody else's exists.
func TestPushToUnderHoldHoldsOnlyThatMember(t *testing.T) {
	sc, ch := setupEditScene(t)
	sc.setHold(t, sc.dara)
	s1 := sc.sendSeq(t, sc.cleo, ch, "seeds are in")
	target := sc.msgIDAt(t, sc.bob, ch, s1)

	sc.sendPushTo(t, sc.bob, ch, target, sc.cleo)
	require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, sc.cleo))
	require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, sc.eddie), "only the message before it")
}

// A send naming a member that cannot reach the server is queued, and the
// drain still names that member: the field lives on the outbox row.
func TestPushToQueuedOfflineKeepsItsMember(t *testing.T) {
	sc, ch := setupEditScene(t)
	s1 := sc.sendSeq(t, sc.cleo, ch, "seeds are in")
	target := sc.msgIDAt(t, sc.bob, ch, s1)
	rows := sc.pushCounts(t, ch)

	var attempts []*proto.UID
	sc.bob.minder.SetTestHooks(&librt.MinderTestHooks{
		SendRPC: func(_ librt.MetaContext, arg rem.RTSendArg) (*rem.RTSendRes, error) {
			attempts = append(attempts, arg.PushTo)
			return nil, simTransportErr()
		},
	})
	_, err := sc.bob.minder.SendTypedPushTo(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(ch), []byte(reactionOn),
		proto.RTMsgType_Reactji, &target, sc.cleo.u.uid)
	require.ErrorAs(t, err, &core.RTMsgQueuedError{})
	_, err = sc.bob.minder.Drain(sc.bob.m)
	require.NoError(t, err)
	require.Len(t, attempts, 2)
	for _, a := range attempts {
		require.NotNil(t, a, "the direct attempt and the drain both name the member")
		require.Equal(t, sc.cleo.u.uid, *a)
	}

	sc.bob.minder.SetTestHooks(nil)
	dres, err := sc.bob.minder.Drain(sc.bob.m)
	require.NoError(t, err)
	require.Len(t, dres.Acked, 1)
	rows[string(sc.cleo.u.name)]++
	require.Equal(t, rows, sc.pushCounts(t, ch), "the drained send pushes only the named member")
}

// MsgSender reads a held message's sender locally, and is nil for a message
// the device does not hold.
func TestMsgSenderReadsHeldMessages(t *testing.T) {
	sc, ch := setupEditScene(t)
	s1 := sc.sendSeq(t, sc.cleo, ch, "seeds are in")
	target := sc.msgIDAt(t, sc.bob, ch, s1)
	spec := lcl.NewRTChannelSpecifierWithId(ch)

	got, err := sc.bob.minder.MsgSender(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat, spec, target)
	require.NoError(t, err)
	require.NotNil(t, got)
	uid, err := got.UID()
	require.NoError(t, err)
	require.Equal(t, sc.cleo.u.uid, uid)

	var unknown proto.RTMsgID
	unknown[0] = 0xff
	got, err = sc.bob.minder.MsgSender(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat, spec, unknown)
	require.NoError(t, err)
	require.Nil(t, got)
}
