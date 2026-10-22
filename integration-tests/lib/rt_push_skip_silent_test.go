// A send flagged noPush never pushes (fork; silent-system-notices).
//
// The app sends its in-app system notices (joins, role changes, identity
// edits) this way: the Notifications blueprint makes them in-app lines only,
// and the server cannot tell them from a message because their kind is sealed
// in the body. Like an edit, a silent send skips the push_outbox fan-out and
// nothing else: the message is stored and every member's inbox version is
// bumped, so online delivery is unchanged.
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

func (s *privScene) sendSilentSeq(
	t *testing.T, by *privActor, chid proto.RTChannelID, body string,
) proto.RTMsgSeq {
	res, err := by.minder.SendSilent(by.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), []byte(body))
	require.NoError(t, err)
	return res.Seq
}

// A silent send queues no row for anyone and still bumps every recipient's
// inbox version, while an ordinary send in the same channel still pushes.
func TestSilentSendQueuesNoPushRows(t *testing.T) {
	sc, ch := setupEditScene(t)
	recipients := []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie}

	before := map[*privActor]int64{}
	for _, a := range recipients {
		before[a] = sc.inboxVersion(t, a)
	}
	sc.sendSilentSeq(t, sc.bob, ch, `{"sy":"Priya joined the community"}`)
	for _, a := range recipients {
		require.Equal(t, 0, sc.pushRowsFor(t, ch, a), "a silent send must not push")
		require.Greater(t, sc.inboxVersion(t, a), before[a],
			"a silent send must still bump the inbox version (online delivery)")
	}

	sc.sendSeq(t, sc.bob, ch, "lunch?")
	for _, a := range recipients {
		require.Equal(t, 1, sc.pushRowsFor(t, ch, a), "an ordinary send still pushes")
	}
}

// A silent send that cannot reach the server is queued, and stays silent when
// the outbox drains it: the flag lives on the outbox row, not only on the
// first attempt. Every attempt the client makes carries it.
func TestSilentSendQueuedOfflineStaysSilent(t *testing.T) {
	sc, ch := setupEditScene(t)
	var attempts []bool
	sc.bob.minder.SetTestHooks(&librt.MinderTestHooks{
		SendRPC: func(_ librt.MetaContext, arg rem.RTSendArg) (*rem.RTSendRes, error) {
			attempts = append(attempts, arg.NoPush)
			return nil, simTransportErr()
		},
	})
	_, err := sc.bob.minder.SendSilent(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(ch), []byte(`{"sy":"Priya joined the community"}`))
	require.ErrorAs(t, err, &core.RTMsgQueuedError{})
	_, err = sc.bob.minder.Drain(sc.bob.m)
	require.NoError(t, err)
	require.Equal(t, []bool{true, true}, attempts, "the direct attempt and the drain both carry the flag")

	sc.bob.minder.SetTestHooks(nil)
	dres, err := sc.bob.minder.Drain(sc.bob.m)
	require.NoError(t, err)
	require.Len(t, dres.Acked, 1)
	for _, a := range []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie} {
		require.Equal(t, 0, sc.pushRowsFor(t, ch, a), "a drained silent send must not push")
	}
}

// A silent send behind an earlier queued ordinary one is delivered by the
// channel drain, in order: the ordinary one pushes, the silent one does not.
func TestSilentSendBehindQueuedSendStaysSilent(t *testing.T) {
	sc, ch := setupEditScene(t)
	sc.bob.minder.SetTestHooks(&librt.MinderTestHooks{
		SendRPC: func(librt.MetaContext, rem.RTSendArg) (*rem.RTSendRes, error) {
			return nil, simTransportErr()
		},
	})
	_, err := sc.bob.minder.Send(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(ch), []byte("lunch?"))
	require.ErrorAs(t, err, &core.RTMsgQueuedError{})

	sc.bob.minder.SetTestHooks(nil)
	sc.sendSilentSeq(t, sc.bob, ch, `{"sy":"Priya joined the community"}`)
	for _, a := range []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie} {
		require.Equal(t, 1, sc.pushRowsFor(t, ch, a), "only the ordinary send pushes")
	}
}

// A silent send returns before the hold check, as an edit does, so it does
// not end a hold whose holder has left the team.
func TestSilentSendDoesNotEndDepartedHoldersHold(t *testing.T) {
	sc, ch := setupEditScene(t)
	sc.setHold(t, sc.dara)
	sc.sendSeq(t, sc.bob, ch, "lunch?")
	require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, sc.cleo))

	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{
			sc.dara.u.toMemberRole(t, proto.NewRoleDefault(proto.RoleType_NONE), nil),
		}, nil)

	sc.sendSilentSeq(t, sc.bob, ch, `{"sy":"Dara left the community"}`)
	require.Equal(t, 1, sc.holdCount(t), "a silent send leaves the hold in place")
	require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, sc.cleo))
}
