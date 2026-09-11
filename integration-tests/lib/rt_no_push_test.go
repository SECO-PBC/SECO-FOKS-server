// No-push channels (fork-only; dm-handshake-over-rt, see p6.sql).
//
// The contract under test, stated as the two halves that must move
// independently: a send into a no-push channel queues ZERO push_outbox rows,
// while the inbox-version fan-out — online delivery, the long-poll wakes —
// is untouched. Getting the first half wrong buzzes every phone in a
// community for every DM handshake; getting the second half wrong makes the
// handshake undeliverable to online members, which is the entire point of
// moving it onto RT.
//
// Reuses the privScene harness: same team, same actors, same direct-DB
// probes. The channel here is NON-private on purpose — no-push is orthogonal
// to privacy, and bob (an ordinary member) creating it doubles as the
// regression pin for the dm-handshake spike's finding that members can
// create bottom-tier m/0 channels.
package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// makeChannelWithOpts creates a bottom-tier m/0 channel as `by` and returns
// its id, failing the test on error.
func (s *privScene) makeChannelWithOpts(
	t *testing.T, by *privActor, opts librt.MakeChannelOpts,
) proto.RTChannelID {
	nm, err := core.RandomDomain()
	require.NoError(t, err)
	chid, err := by.minder.MakeChannelWithOpts(
		by.m, s.teamCfg(), proto.RTAppID_Chat,
		proto.RTChannelName("np-"+nm), "no-push test channel",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		opts, nil,
	)
	require.NoError(t, err)
	return *chid
}

// pushRowsFor counts push_outbox rows for one actor in one channel. The
// scene's own pushRows is this with s.chid bound, so the query lives once.
func (s *privScene) pushRowsFor(t *testing.T, chid proto.RTChannelID, a *privActor) int {
	return s.rtdbCount(t,
		`SELECT count(*) FROM push_outbox WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		s.tew.MetaContext().ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB())
}

// sendTo posts one message as `by` into the given channel.
func (s *privScene) sendTo(t *testing.T, by *privActor, chid proto.RTChannelID, body string) {
	_, err := by.minder.Send(by.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), []byte(body))
	require.NoError(t, err)
}

// A send into a no-push channel queues no push rows for anyone — while the
// same send still bumps every member's inbox version, so parked long-pollers
// wake and online delivery is unchanged.
func TestNoPushChannelQueuesNoPushRows(t *testing.T) {
	sc := setupPrivScene(t, true)

	// bob is an ORDINARY member: creation must not require admin.
	chid := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{NoPush: true})

	before := map[*privActor]int64{}
	for _, a := range []*privActor{sc.alice, sc.cleo, sc.dara} {
		before[a] = sc.inboxVersion(t, a)
	}

	sc.sendTo(t, sc.bob, chid, "control traffic; nobody's phone should buzz")

	for _, a := range []*privActor{sc.alice, sc.bob, sc.cleo, sc.dara, sc.eddie} {
		require.Equal(t, 0, sc.pushRowsFor(t, chid, a),
			"a no-push channel must queue no push_outbox row for anyone")
	}
	// The other half of the contract: inbox wakes are NOT suppressed.
	for _, a := range []*privActor{sc.alice, sc.cleo, sc.dara} {
		require.Greater(t, sc.inboxVersion(t, a), before[a],
			"no-push must not suppress the inbox-version fan-out (online delivery)")
	}
}

// The control case that keeps the guard honest: an ordinary channel made by
// the same creator in the same team still fans out one push row per
// recipient, sender excluded. If this ever goes red alongside the test above
// staying green, the flag has leaked into the default path.
func TestOrdinaryChannelStillQueuesPushRows(t *testing.T) {
	sc := setupPrivScene(t, true)

	chid := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.sendTo(t, sc.bob, chid, "conversation; members' phones may buzz")

	require.Equal(t, 0, sc.pushRowsFor(t, chid, sc.bob), "the sender is never pushed to")
	for _, a := range []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie} {
		require.Equal(t, 1, sc.pushRowsFor(t, chid, a),
			"an ordinary channel's send must still queue one push row per recipient")
	}
}

// The flag lands in the row the send path reads, and reaches a DIFFERENT
// reader back out again.
//
// Both halves matter and they fail independently. The row is what suppresses
// the fan-out; the listing is how a client tells an existing control channel
// that was created no-push from one that was not. MakeChannel treats "already
// exists" as success, so a peer on an older build can leave an UNFLAGGED
// `seco-ctl` behind — and without the read-back reaching the plaintext
// struct, no client can see the difference or repair it, and every control
// message posted there pushes to the whole team.
func TestNoPushStoredPerChannel(t *testing.T) {
	sc := setupPrivScene(t, true)

	quiet := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{NoPush: true})
	loud := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})

	noPushInRow := func(chid proto.RTChannelID) bool {
		m := sc.tew.MetaContext()
		db, err := m.Db(shared.DbTypeRealTime)
		require.NoError(t, err)
		defer db.Release()
		var v bool
		require.NoError(t, db.QueryRow(m.Ctx(),
			`SELECT no_push FROM channels WHERE short_host_id=$1 AND channel_id=$2`,
			m.ShortHostID(), chid.Short().Int64()).Scan(&v))
		return v
	}
	require.True(t, noPushInRow(quiet), "no-push flag lost between MakeChannel and the row")
	require.False(t, noPushInRow(loud), "no-push flag leaked onto an ordinary channel")

	// Read back through a DIFFERENT member's client, so this exercises the
	// decrypt path rather than the creator's own cache.
	lst, err := sc.alice.minder.ListAllChannelsForTeam(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat)
	require.NoError(t, err)
	seen := map[proto.RTChannelID]bool{}
	for i := range lst.Channels {
		ch := &lst.Channels[i]
		switch {
		case ch.Id.Eq(quiet):
			seen[quiet] = true
			require.True(t, ch.NoPush, "no-push flag dropped on the way out to a client")
		case ch.Id.Eq(loud):
			seen[loud] = true
			require.False(t, ch.NoPush, "no-push flag invented for an ordinary channel")
		}
	}
	require.True(t, seen[quiet] && seen[loud], "both channels must appear in the listing")
}

// The default channel may never be born silent. librt is the only layer that
// can enforce this — the server sees an encrypted name and cannot tell the
// default channel from any other — and the flag has no flip path, so a
// mistake here would permanently kill push for every message in the team.
func TestDefaultChannelCannotBeNoPush(t *testing.T) {
	sc := setupPrivScene(t, true)

	_, err := sc.bob.minder.MakeChannelWithOpts(
		sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		proto.RTChannelName(""), "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		librt.MakeChannelOpts{NoPush: true}, nil,
	)
	require.Error(t, err, "creating the default channel no-push must be refused")

	// The same call without the flag is the ordinary self-heal, and must work.
	_, err = sc.bob.minder.MakeChannelWithOpts(
		sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		proto.RTChannelName(""), "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		librt.MakeChannelOpts{}, nil,
	)
	require.NoError(t, err, "the guard must not block ordinary default-channel creation")
}
