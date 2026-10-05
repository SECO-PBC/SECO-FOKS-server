// Per-member channel push mute (fork-only; follow-scoped-push).
//
// The contract: a muted member gets no push_outbox row from a send or a
// notify, and nothing else about the channel changes for them. The flag is
// set only by its owner, a change reaches their other devices through an
// inbox-version bump, and a channel created startMuted pushes nobody but its
// creator until they unmute.
package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

func (s *privScene) setMuted(t *testing.T, a *privActor, chid proto.RTChannelID, muted bool) error {
	return a.minder.SetChannelMuted(a.m, s.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(chid), muted)
}

// mutedInRow reads one member's user_channels.muted.
func (s *privScene) mutedInRow(t *testing.T, chid proto.RTChannelID, a *privActor) bool {
	return s.rtdbCount(t,
		`SELECT count(*) FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3 AND muted`,
		s.tew.MetaContext().ShortHostID(), chid.Short().Int64(), a.u.uid.ExportToDB()) == 1
}

func TestMutedMemberGetsNoPush(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})

	require.NoError(t, sc.setMuted(t, sc.cleo, ch, true))
	sc.sendTo(t, sc.bob, ch, "cleo muted this")

	require.Equal(t, 0, sc.pushRowsFor(t, ch, sc.cleo), "a muted member must get no push row")
	for _, a := range []*privActor{sc.alice, sc.dara, sc.eddie} {
		require.Equal(t, 1, sc.pushRowsFor(t, ch, a), "an unmuted member still gets one")
		require.False(t, sc.mutedInRow(t, ch, a), "cleo's call touched another member's row")
	}

	// Unmuting restores the push for the next send.
	require.NoError(t, sc.setMuted(t, sc.cleo, ch, false))
	sc.sendTo(t, sc.bob, ch, "cleo unmuted")
	require.Equal(t, 1, sc.pushRowsFor(t, ch, sc.cleo))
}

// Muting changes pushes only: the muted member still gets the inbox bump that
// drives online delivery.
func TestMutedMemberStillGetsInboxBump(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	require.NoError(t, sc.setMuted(t, sc.cleo, ch, true))

	before := sc.inboxVersion(t, sc.cleo)
	sc.sendTo(t, sc.bob, ch, "still delivered")
	require.Greater(t, sc.inboxVersion(t, sc.cleo), before)
}

// A change bumps the caller's inbox version once, so their other devices see
// it; the same value again changes nothing and bumps nothing.
func TestSetChannelMutedBumpsOnlyOnChange(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})

	v0 := sc.inboxVersion(t, sc.cleo)
	require.NoError(t, sc.setMuted(t, sc.cleo, ch, true))
	v1 := sc.inboxVersion(t, sc.cleo)
	require.Equal(t, v0+1, v1)
	require.True(t, sc.mutedInRow(t, ch, sc.cleo))

	require.NoError(t, sc.setMuted(t, sc.cleo, ch, true))
	require.Equal(t, v1, sc.inboxVersion(t, sc.cleo), "a repeat must not bump")

	// The flag reaches the inbox the member's devices sync.
	_, err := sc.cleo.minder.SyncInbox(sc.cleo.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	inbox, err := sc.cleo.minder.LocalInbox(sc.cleo.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	var found bool
	for _, r := range inbox.Rows {
		if r.Ch.Id == ch {
			found = true
			require.True(t, r.Muted)
		}
	}
	require.True(t, found)
}

// No access: a private channel the caller is not on -- or was revoked from --
// gives the same not-found as a channel that does not exist. Raw client, so the
// server answers rather than librt's own channel lookup.
func TestSetChannelMutedWithoutAccess(t *testing.T) {
	sc := setupPrivScene(t, false) // private: alice only
	sc.grant(t, sc.alice, sc.bob, false)
	arg := rem.RtSetChannelMutedArg{ChannelID: sc.chid, Muted: true}
	require.NoError(t, sc.bob.raw(t).RtSetChannelMuted(sc.bob.m.Ctx(), arg))

	requireHidden(t, sc.cleo.raw(t).RtSetChannelMuted(sc.cleo.m.Ctx(), arg), "rtSetChannelMuted")
	sc.revoke(t, sc.alice, sc.bob)
	requireHidden(t, sc.bob.raw(t).RtSetChannelMuted(sc.bob.m.Ctx(), arg), "rtSetChannelMuted after revoke")
}

// A startMuted channel fans everyone but the creator in muted, so a send
// pushes nobody until they unmute.
func TestStartMutedChannelPushesNobodyElse(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{StartMuted: true})

	require.False(t, sc.mutedInRow(t, ch, sc.bob), "the creator must start unmuted")
	for _, a := range []*privActor{sc.alice, sc.cleo, sc.dara, sc.eddie} {
		require.True(t, sc.mutedInRow(t, ch, a))
	}

	sc.sendTo(t, sc.alice, ch, "nobody joined yet")
	require.Equal(t, 1, sc.pushRowsFor(t, ch, sc.bob), "the creator is pushed")
	for _, a := range []*privActor{sc.cleo, sc.dara, sc.eddie} {
		require.Equal(t, 0, sc.pushRowsFor(t, ch, a))
	}

	// Joining (unmuting) turns pushes on.
	require.NoError(t, sc.setMuted(t, sc.cleo, ch, false))
	sc.sendTo(t, sc.alice, ch, "cleo joined")
	require.Equal(t, 1, sc.pushRowsFor(t, ch, sc.cleo))
}

func TestPushNotifySkipsMuted(t *testing.T) {
	sc := setupPrivScene(t, true)
	ch := sc.makeChannelWithOpts(t, sc.bob, librt.MakeChannelOpts{})
	sc.setHold(t, sc.dara)
	require.NoError(t, sc.setMuted(t, sc.cleo, ch, true))

	require.NoError(t, sc.dara.minder.NotifyMembers(sc.dara.m, ch, []rem.RTPushNotify{
		{Uid: sc.alice.u.uid},
		{Uid: sc.cleo.u.uid},
	}))
	require.Equal(t, queued(1), sc.pushRowsByStatus(t, ch, sc.alice))
	require.Empty(t, sc.pushRowsByStatus(t, ch, sc.cleo))
}
