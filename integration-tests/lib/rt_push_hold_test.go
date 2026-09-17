// Delegated push release (fork-only; push holds, server/realtime/pushhold.go).
//
// dara (a team admin) plays the holder throughout -- in SECO that is the
// community's daemon. The contract under test:
//   - under a hold, a send into a channel the holder can read queues nothing:
//     its rows are 'held';
//   - only the holder decides held rows (release keeps, drops, coalesces);
//   - when the hold ends (cleared, holder revoked, holder gone) what it held
//     is released rather than stranded.
package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// pushRowsByStatus counts one actor's push rows in a channel per status.
func (s *privScene) pushRowsByStatus(
	t *testing.T, chid proto.RTChannelID, a *privActor,
) map[string]int {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	rows, err := db.Query(m.Ctx(),
		`SELECT status::text, count(*) FROM push_outbox
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3
		 GROUP BY status`,
		m.ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB())
	require.NoError(t, err)
	defer rows.Close()
	ret := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		require.NoError(t, rows.Scan(&st, &n))
		ret[st] = n
	}
	require.NoError(t, rows.Err())
	return ret
}

// queuedSeqs lists one actor's queued push rows' seqs in a channel.
func (s *privScene) queuedSeqs(t *testing.T, chid proto.RTChannelID, a *privActor) []int64 {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	rows, err := db.Query(m.Ctx(),
		`SELECT seq FROM push_outbox
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3 AND status='queued'
		 ORDER BY seq`,
		m.ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB())
	require.NoError(t, err)
	defer rows.Close()
	var ret []int64
	for rows.Next() {
		var seq int64
		require.NoError(t, rows.Scan(&seq))
		ret = append(ret, seq)
	}
	return ret
}

func (s *privScene) setHold(t *testing.T, by *privActor) {
	require.NoError(t, by.minder.SetPushHold(by.m, s.teamCfg(), proto.RTAppID_Chat))
}

func (s *privScene) sendSeq(
	t *testing.T, by *privActor, chid proto.RTChannelID, body string,
) proto.RTMsgSeq {
	res, err := by.minder.Send(by.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), []byte(body))
	require.NoError(t, err)
	return res.Seq
}

func (s *privScene) holdCount(t *testing.T) int {
	return s.rtdbCount(t, `SELECT count(*) FROM push_holds WHERE team_id=$1`, s.teamID.ExportToDB())
}

func held(n int) map[string]int   { return map[string]int{"held": n} }
func queued(n int) map[string]int { return map[string]int{"queued": n} }

func TestPushHoldSetNeedsAdmin(t *testing.T) {
	sc := setupPrivScene(t, true)

	err := sc.bob.minder.SetPushHold(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat)
	require.True(t, core.IsPermissionError(err), "a plain member placed a hold: %v", err)
	require.Equal(t, 0, sc.holdCount(t))

	sc.setHold(t, sc.dara)
	require.Equal(t, 1, sc.holdCount(t))
	// Setting again (here by another admin) takes the hold over; still one row.
	sc.setHold(t, sc.alice)
	require.Equal(t, 1, sc.holdCount(t))
}

// The held/queued matrix: coverage follows the holder's read access.
func TestPushHoldMatrix(t *testing.T) {
	sc := setupPrivScene(t, false) // sc.chid: private, alice only
	public := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	quiet := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{NoPush: true})

	// No hold: today's behaviour.
	sc.sendSeq(t, sc.bob, public, "before any hold")
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, public, sc.cleo))

	sc.setHold(t, sc.dara)

	sc.sendSeq(t, sc.bob, public, "public, under the hold")
	require.Equal(t, map[string]int{"queued": 1, "held": 1}, sc.pushRowsByStatus(t, public, sc.cleo))

	// The holder's own sends are held too.
	sc.sendSeq(t, sc.dara, public, "the holder speaks")
	require.Equal(t, map[string]int{"queued": 1, "held": 2}, sc.pushRowsByStatus(t, public, sc.cleo))

	// A private channel the holder is not in queues at once.
	sc.grant(t, sc.alice, sc.bob, false)
	sc.send(t, sc.alice, "private, holder outside")
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, sc.chid, sc.bob))

	// Granting the holder brings the channel under the hold.
	sc.grant(t, sc.alice, sc.dara, false)
	sc.send(t, sc.alice, "private, holder inside")
	require.Equal(t, map[string]int{"queued": 1, "held": 1}, sc.pushRowsByStatus(t, sc.chid, sc.bob))

	// A no-push channel still writes nothing.
	sc.sendSeq(t, sc.bob, quiet, "control traffic")
	require.Empty(t, sc.pushRowsByStatus(t, quiet, sc.cleo))
}

func TestPushReleaseKeepDropCoalesce(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.setHold(t, sc.dara)

	sc.sendSeq(t, sc.bob, ch, "one")
	s2 := sc.sendSeq(t, sc.bob, ch, "two")
	s3 := sc.sendSeq(t, sc.bob, ch, "three")

	release := func(keep []proto.UID, drop []rem.RTPushDrop) {
		require.NoError(t, sc.dara.minder.ReleasePushes(sc.dara.m, ch, s3, keep, drop))
	}
	release(
		[]proto.UID{sc.cleo.u.uid},
		[]rem.RTPushDrop{{Uid: sc.eddie.u.uid, Seq: s3}},
	)

	// alice: a backlog of three becomes one push, the newest.
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, ch, sc.alice))
	require.Equal(t, []int64{s3.Int64()}, sc.queuedSeqs(t, ch, sc.alice))
	// cleo: kept back, untouched.
	require.Equal(t, held(3), sc.pushRowsByStatus(t, ch, sc.cleo))
	// eddie: seq 3 dropped; of what is left, only the newest is sent.
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, ch, sc.eddie))
	require.Equal(t, []int64{s2.Int64()}, sc.queuedSeqs(t, ch, sc.eddie))

	// Repeating the call changes nothing.
	release(
		[]proto.UID{sc.cleo.u.uid},
		[]rem.RTPushDrop{{Uid: sc.eddie.u.uid, Seq: s3}},
	)
	require.Equal(t, held(3), sc.pushRowsByStatus(t, ch, sc.cleo))
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, ch, sc.alice))

	// Deciding cleo later: one push.
	release(nil, nil)
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, ch, sc.cleo))
	require.Equal(t, []int64{s3.Int64()}, sc.queuedSeqs(t, ch, sc.cleo))
}

func TestPushReleaseNonHolderDenied(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.setHold(t, sc.dara)
	s := sc.sendSeq(t, sc.bob, ch, "held")

	// alice is an admin, but not the holder.
	err := sc.alice.minder.ReleasePushes(sc.alice.m, ch, s, nil, nil)
	require.True(t, core.IsPermissionError(err), "got %v", err)
	err = sc.alice.minder.NotifyMembers(sc.alice.m, ch,
		[]rem.RTPushNotify{{Uid: sc.bob.u.uid}})
	require.True(t, core.IsPermissionError(err), "got %v", err)
	require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, sc.cleo))
}

func TestPushNotifySkipsNonReaders(t *testing.T) {
	sc := setupPrivScene(t, false) // private: alice only
	sc.grant(t, sc.alice, sc.bob, false)
	sc.grant(t, sc.alice, sc.dara, false)
	sc.setHold(t, sc.dara)

	handle := []byte("0123456789abcdef")
	require.NoError(t, sc.dara.minder.NotifyMembers(sc.dara.m, sc.chid, []rem.RTPushNotify{
		{Uid: sc.bob.u.uid, Handle: handle},
		{Uid: sc.cleo.u.uid, Handle: handle}, // not on the access list
	}))
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, sc.chid, sc.bob))
	require.Empty(t, sc.pushRowsByStatus(t, sc.chid, sc.cleo))
	require.Equal(t, 1, sc.rtdbCount(t,
		`SELECT count(*) FROM push_outbox WHERE channel_id=$1 AND uid=$2 AND kind='system' AND data=$3`,
		sc.chid.Short().Int64(), sc.bob.u.uid.ExportToDB(), handle))

	err := sc.dara.minder.NotifyMembers(sc.dara.m, sc.chid, []rem.RTPushNotify{
		{Uid: sc.bob.u.uid, Handle: make([]byte, 33)},
	})
	require.Error(t, err, "a handle over 32 bytes must be refused")
}

func TestPushHoldClearReleases(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.setHold(t, sc.dara)
	sc.sendSeq(t, sc.bob, ch, "one")
	sc.sendSeq(t, sc.bob, ch, "two")

	err := sc.bob.minder.ClearPushHold(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat)
	require.True(t, core.IsPermissionError(err), "a plain member cleared a hold: %v", err)

	// Another admin (the owner) clears it: everything held goes out, unfiltered.
	require.NoError(t, sc.alice.minder.ClearPushHold(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat))
	require.Equal(t, queued(2), sc.pushRowsByStatus(t, ch, sc.cleo))
	sc.sendSeq(t, sc.bob, ch, "after")
	require.Equal(t, queued(3), sc.pushRowsByStatus(t, ch, sc.cleo))
}

func TestPushHoldRevokeHolderReleasesChannel(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.grant(t, sc.alice, sc.dara, false)
	sc.setHold(t, sc.dara)

	sc.send(t, sc.alice, "held")
	require.Equal(t, held(1), sc.pushRowsByStatus(t, sc.chid, sc.bob))

	sc.revoke(t, sc.alice, sc.dara)
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, sc.chid, sc.bob))
	sc.send(t, sc.alice, "after the holder left the channel")
	require.Equal(t, queued(2), sc.pushRowsByStatus(t, sc.chid, sc.bob))
}

func TestPushHoldHolderLeftTeamEndsOnSend(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.setHold(t, sc.dara)
	sc.sendSeq(t, sc.bob, ch, "held")
	require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, sc.cleo))

	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{
			sc.dara.u.toMemberRole(t, proto.NewRoleDefault(proto.RoleType_NONE), nil),
		}, nil)

	sc.sendSeq(t, sc.bob, ch, "the send that notices")
	require.Equal(t, queued(2), sc.pushRowsByStatus(t, ch, sc.cleo),
		"the earlier held row is released and the new one queued")
	require.Equal(t, 0, sc.holdCount(t))
}
