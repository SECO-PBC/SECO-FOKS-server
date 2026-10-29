// Explicit read pointer and read pushes (fork-only; cross-device-read-sync in
// SECO-FOKS, server/realtime/readpush.go). The contract under test:
//   - only an explicit mark (MarkRead) or the user's own send moves
//     explicit_read_through; a plain ReadThrough -- what every page fetch
//     does -- moves only read_through;
//   - moving the explicit pointer deletes the user's queued/held message
//     pushes at or below it, and queues one 'read' row when a push in the
//     newly read range already went out and the user has a push token.
package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

func (s *privScene) markRead(t *testing.T, by *privActor, chid proto.RTChannelID, seq proto.RTMsgSeq) {
	require.NoError(t, by.minder.MarkRead(by.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), seq, false))
}

func (s *privScene) readThrough(t *testing.T, by *privActor, chid proto.RTChannelID, seq proto.RTMsgSeq) {
	require.NoError(t, by.minder.ReadThrough(by.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), seq))
}

// pointers reads one actor's (read_through, explicit_read_through) in a channel.
func (s *privScene) pointers(t *testing.T, chid proto.RTChannelID, a *privActor) (int64, int64) {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var rt, ert int64
	require.NoError(t, db.QueryRow(m.Ctx(),
		`SELECT read_through, explicit_read_through FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		m.ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB()).Scan(&rt, &ert))
	return rt, ert
}

// deliver marks one actor's queued rows in a channel as sent, as the relay would.
func (s *privScene) deliver(t *testing.T, chid proto.RTChannelID, a *privActor) {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	_, err = db.Exec(m.Ctx(),
		`UPDATE push_outbox SET status='done'
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3 AND status='queued'`,
		m.ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB())
	require.NoError(t, err)
}

// readRows lists one actor's 'read' rows in a channel as (seq, data).
func (s *privScene) readRows(t *testing.T, chid proto.RTChannelID, a *privActor) map[int64][]byte {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	rows, err := db.Query(m.Ctx(),
		`SELECT seq, data FROM push_outbox
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3 AND kind='read'`,
		m.ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB())
	require.NoError(t, err)
	defer rows.Close()
	ret := map[int64][]byte{}
	for rows.Next() {
		var seq int64
		var data []byte
		require.NoError(t, rows.Scan(&seq, &data))
		ret[seq] = data
	}
	require.NoError(t, rows.Err())
	return ret
}

// queuedMsgSeqs lists one actor's queued message pushes' seqs in a channel.
func (s *privScene) queuedMsgSeqs(t *testing.T, chid proto.RTChannelID, a *privActor) []int64 {
	var ret []int64
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	rows, err := db.Query(m.Ctx(),
		`SELECT seq FROM push_outbox
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3
		   AND kind='msg' AND status='queued'`,
		m.ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB())
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var seq int64
		require.NoError(t, rows.Scan(&seq))
		ret = append(ret, seq)
	}
	require.NoError(t, rows.Err())
	return ret
}

func (s *privScene) registerToken(t *testing.T, a *privActor) {
	require.NoError(t, a.minder.SetPushToken(a.m, proto.RTPushPlatform_Apns,
		proto.RTPushToken(a.u.uid.ExportToDB()[:8]), true))
}

func TestExplicitReadPointer(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})

	sc.sendSeq(t, sc.bob, ch, "one")
	s2 := sc.sendSeq(t, sc.bob, ch, "two")
	s3 := sc.sendSeq(t, sc.bob, ch, "three")

	// A page fetch's mark moves only the plain pointer.
	sc.readThrough(t, sc.cleo, ch, s3)
	rt, ert := sc.pointers(t, ch, sc.cleo)
	require.Equal(t, s3.Int64(), rt)
	require.Equal(t, int64(0), ert)

	// An explicit mark below the plain pointer still moves the explicit one,
	// and wakes the user's other devices.
	before := sc.inboxVersion(t, sc.cleo)
	sc.markRead(t, sc.cleo, ch, s2)
	rt, ert = sc.pointers(t, ch, sc.cleo)
	require.Equal(t, s3.Int64(), rt)
	require.Equal(t, s2.Int64(), ert)
	require.Equal(t, before+1, sc.inboxVersion(t, sc.cleo))

	// A stale explicit mark is a no-op and bumps nothing.
	sc.markRead(t, sc.cleo, ch, s2)
	require.Equal(t, before+1, sc.inboxVersion(t, sc.cleo))

	// The inbox carries it.
	view, err := sc.cleo.minder.InboxView(sc.cleo.m, proto.RTAppID_Chat, false)
	require.NoError(t, err)
	found := false
	for _, row := range view.Rows {
		if row.Ch.Id == ch {
			found = true
			require.Equal(t, s2, row.ExplicitReadThrough)
			require.Equal(t, s3, row.ReadThrough)
		}
	}
	require.True(t, found)

	// Sending is reading.
	s4 := sc.sendSeq(t, sc.cleo, ch, "cleo replies")
	_, ert = sc.pointers(t, ch, sc.cleo)
	require.Equal(t, s4.Int64(), ert)
}

func TestReadSettlesPushes(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.registerToken(t, sc.cleo)

	// s1 reached cleo's phone; s2 is still queued.
	sc.sendSeq(t, sc.bob, ch, "one")
	sc.deliver(t, ch, sc.cleo)
	s2 := sc.sendSeq(t, sc.bob, ch, "two")
	require.Equal(t, map[string]int{"done": 1, "queued": 1}, sc.pushRowsByStatus(t, ch, sc.cleo))

	// A plain mark settles nothing.
	sc.readThrough(t, sc.cleo, ch, s2)
	require.Equal(t, map[string]int{"done": 1, "queued": 1}, sc.pushRowsByStatus(t, ch, sc.cleo))
	require.Empty(t, sc.readRows(t, ch, sc.cleo))

	// Reading on another device: s2 never goes out, and s1 is taken back.
	sc.markRead(t, sc.cleo, ch, s2)
	require.Equal(t, map[string]int{"done": 1, "queued": 1}, sc.pushRowsByStatus(t, ch, sc.cleo))
	require.Empty(t, sc.queuedMsgSeqs(t, ch, sc.cleo))
	reads := sc.readRows(t, ch, sc.cleo)
	require.Len(t, reads, 1)
	require.Equal(t, ch[:], reads[s2.Int64()])

	// Nothing new delivered since: the next read queues no second read push.
	s3 := sc.sendSeq(t, sc.bob, ch, "three")
	sc.markRead(t, sc.cleo, ch, s3)
	require.Len(t, sc.readRows(t, ch, sc.cleo), 1)

	// A delivered push answered by cleo's own send is taken back too.
	sc.sendSeq(t, sc.bob, ch, "four")
	sc.deliver(t, ch, sc.cleo)
	s5 := sc.sendSeq(t, sc.cleo, ch, "cleo replies")
	reads = sc.readRows(t, ch, sc.cleo)
	require.Len(t, reads, 2)
	require.Contains(t, reads, s5.Int64())
}

func TestReadDropsHeldPushes(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.registerToken(t, sc.cleo)
	sc.setHold(t, sc.dara)

	s1 := sc.sendSeq(t, sc.bob, ch, "waiting for the daemon")
	require.Equal(t, held(1), sc.pushRowsByStatus(t, ch, sc.cleo))

	// Read on desktop before the daemon released it: the phone never buzzes.
	sc.markRead(t, sc.cleo, ch, s1)
	require.Empty(t, sc.pushRowsByStatus(t, ch, sc.cleo))
	require.NoError(t, sc.dara.minder.ReleasePushes(sc.dara.m, ch, s1, nil, nil))
	require.Empty(t, sc.pushRowsByStatus(t, ch, sc.cleo))
	// Others still get theirs.
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, ch, sc.eddie))
}

func TestReadWithoutTokenQueuesNoReadPush(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})

	s1 := sc.sendSeq(t, sc.bob, ch, "one")
	sc.deliver(t, ch, sc.eddie)
	sc.markRead(t, sc.eddie, ch, s1)
	require.Empty(t, sc.readRows(t, ch, sc.eddie))
}

// An explicit read made offline stays explicit: it shows on this device's
// inbox at once and replays as explicit, next to a plain mark of the same
// channel that replays as plain.
func TestExplicitReadOfflineReplay(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.sendSeq(t, sc.bob, ch, "one")
	s2 := sc.sendSeq(t, sc.bob, ch, "two")
	s3 := sc.sendSeq(t, sc.bob, ch, "three")
	_, err := sc.cleo.minder.SyncInbox(sc.cleo.m, proto.RTAppID_Chat)
	require.NoError(t, err)

	sc.cleo.minder.SetTestHooks(&librt.MinderTestHooks{
		ReadThroughRPC: func(librt.MetaContext, rem.RTReadThroughArg) error {
			return simTransportErr()
		},
	})
	sc.markRead(t, sc.cleo, ch, s2)
	sc.readThrough(t, sc.cleo, ch, s3)

	view, err := sc.cleo.minder.LocalInbox(sc.cleo.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	found := false
	for _, row := range view.Rows {
		if row.Ch.Id == ch {
			found = true
			require.Equal(t, s2, row.ExplicitReadThrough)
			require.Equal(t, s3, row.ReadThrough)
		}
	}
	require.True(t, found)

	sc.cleo.minder.SetTestHooks(nil)
	dres, err := sc.cleo.minder.Drain(sc.cleo.m)
	require.NoError(t, err)
	require.NoError(t, dres.TransportErr)
	rt, ert := sc.pointers(t, ch, sc.cleo)
	require.Equal(t, s3.Int64(), rt)
	require.Equal(t, s2.Int64(), ert)
}
