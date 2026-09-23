// Channel metadata mutation (fork-only; docs/rt-channel-mutation.md).
//
// Rename, edit description, archive and unarchive. Three of these tests earn
// their keep more than the rest:
//
//   - TestRenameResealsAtTierRole is the security test. The name box is sealed
//     at a role determined by the channel's TIER, and a rename that sealed at
//     the caller's own role instead would either hand an admin-tier channel's
//     name to ordinary members or lock members out of a name they could read a
//     moment earlier. Nothing else would go red.
//   - TestArchivedNameStaysReserved pins the invariant the whole archive design
//     rests on: an archived channel keeps its place in the team's channel
//     listing, which is what stops anything taking its name while it is away,
//     which is what makes unarchiving safe. Names are PTK-encrypted, so if this
//     ever stopped holding the server could not detect the resulting collision.
//   - TestArchivedLeavesTheInbox covers the one thing the server cannot do on
//     its own: a delta of rows cannot express a removal, so an archived channel
//     is delivered once carrying the flag and the CLIENT drops it. Without that
//     handshake an archived channel sits in every member's inbox forever.
//
// Cast is setupPrivScene's (see rt_private_channel_test.go): alice owns the
// team, dara is a team admin, bob and cleo are ordinary members.
package lib

import (
	"sync"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// mutScene is a privScene carrying a PUBLIC channel, which is what these tests
// mutate. setupPrivScene's own channel is private, and privacy changes which
// listing a channel appears in -- irrelevant to mutation and one more thing to
// reason about in every assertion.
type mutScene struct {
	*privScene
	pubID   proto.RTChannelID
	pubName proto.RTChannelName
}

func setupMutScene(t *testing.T) *mutScene {
	sc := setupPrivScene(t, true)
	// Every test on this scene gets the invariant suite, rather than each
	// remembering to defer it. Cleanup rather than defer, because setup
	// returns long before the test does.
	//
	// Wired here rather than in setupPrivScene, which would cover the
	// private-channel tests too. That was tried, and it does not hold yet: the
	// revoke path bumps a member's inbox version without stamping a row, on
	// purpose, and privScene can only count the revokes that go through its
	// own helper -- several tests call RevokeChannelMember directly. Covering
	// them needs either revoke to stop leaving the gap or the orphan check to
	// identify revoked users, and neither belongs in this change. Extending
	// the net that far did earn its keep once already: it is what found the
	// wasted version in fanUserIntoChannel.
	t.Cleanup(func() { sc.requireRTInvariants(t) })
	// The default channel first, as a real team has it: it is created on the
	// team's first send, before anything else. Without it the channel under
	// test would itself be the team's oldest public bottom-tier channel, which
	// is how the server identifies the default one -- and archiving it would
	// be refused for the wrong reason.
	_, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, "", "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.NoError(t, err)

	name := randomChannelName(t, "pub-")
	chid, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, name, "a public channel",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.NoError(t, err)
	return &mutScene{privScene: sc, pubID: *chid, pubName: name}
}

func randomChannelName(t *testing.T, prefix string) proto.RTChannelName {
	nm, err := core.RandomDomain()
	require.NoError(t, err)
	return proto.RTChannelName(prefix + nm)
}

// pubChannelTier reads a PUBLIC channel's tier. privScene.channelTier asserts
// the row is private, which these channels are not.
func (s *mutScene) pubChannelTier(t *testing.T, id proto.RTChannelID) proto.RTChannelTier {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var raw string
	require.NoError(t, db.QueryRow(m.Ctx(),
		`SELECT tier FROM channels WHERE short_host_id=$1 AND channel_id=$2`,
		m.ShortHostID(), id.Short().Int64()).Scan(&raw))
	var tier proto.RTChannelTier
	require.NoError(t, tier.ImportFromDB(raw))
	return tier
}

func (s *mutScene) pubSpec() lcl.RTChannelSpecifier {
	return lcl.NewRTChannelSpecifierWithId(s.pubID)
}

func (s *mutScene) specFor(id proto.RTChannelID) lcl.RTChannelSpecifier {
	return lcl.NewRTChannelSpecifierWithId(id)
}

func (s *mutScene) rename(
	t *testing.T, by *privActor, spec lcl.RTChannelSpecifier,
	nm proto.RTChannelName, desc proto.RTChannelDesc,
) error {
	return by.minder.UpdateChannel(by.m, s.teamCfg(), proto.RTAppID_Chat, spec, nm, desc)
}

func (s *mutScene) setArchived(
	t *testing.T, by *privActor, spec lcl.RTChannelSpecifier, archived bool,
) error {
	return by.minder.SetChannelArchived(by.m, s.teamCfg(), proto.RTAppID_Chat, spec, archived)
}

// find returns one channel from an actor's freshly-listed view of the team, or
// nil when it is absent. A fresh Minder per call, because the point of most of
// these assertions is what a client learns from the SERVER, not what it kept
// in a cache it populated before the mutation.
func (s *mutScene) find(
	t *testing.T, by *privActor, id proto.RTChannelID,
) *lcl.RTChannelMetadataPlaintext {
	lst, err := by.minder.ListAllChannelsForTeam(by.m, s.teamCfg(), proto.RTAppID_Chat)
	require.NoError(t, err)
	for i := range lst.Channels {
		if lst.Channels[i].Id.Eq(id) {
			return &lst.Channels[i]
		}
	}
	return nil
}

// --- rename ---------------------------------------------------------------

// Only team admins may change channel metadata. The product rule (Leaders and
// Stewards) collapses to admin-or-above in FOKS, and it is enforced on the
// server: a UI-only rule is not a rule.
func TestRenameRequiresAdmin(t *testing.T) {
	sc := setupMutScene(t)

	err := sc.rename(t, sc.bob, sc.pubSpec(), randomChannelName(t, "nope-"), "")
	require.Error(t, err)
	require.IsType(t, core.PermissionError(""), err)

	// dara is a team admin but did not create the channel; she may still rename it.
	newName := randomChannelName(t, "dara-")
	require.NoError(t, sc.rename(t, sc.dara, sc.pubSpec(), newName, "renamed by an admin"))
	require.Equal(t, newName, sc.find(t, sc.alice, sc.pubID).Name)
}

// THE security test. The name box is sealed at the role the channel's TIER
// dictates -- MinRTRole for bottom, AdminRole for admin -- never at the role
// the caller happens to hold. Getting this wrong is silent: the rename
// succeeds, and either a channel name leaks downward or members who could read
// the old name cannot read the new one.
func TestRenameResealsAtTierRole(t *testing.T) {
	sc := setupMutScene(t)

	// Bottom tier, renamed by the team OWNER (whose own role is far above
	// MinRTRole). The box must still be sealed at the bottom tier's name role,
	// which is what lets bob -- an ordinary member -- read the new name.
	bottomName := randomChannelName(t, "bottom-")
	require.NoError(t, sc.rename(t, sc.alice, sc.pubSpec(), bottomName, ""))
	require.Equal(t, proto.RTChannelTier_Bottom, sc.pubChannelTier(t, sc.pubID))
	fromBob := sc.find(t, sc.bob, sc.pubID)
	require.NotNil(t, fromBob, "an ordinary member must still see the channel")
	require.Equal(t, bottomName, fromBob.Name,
		"a bottom-tier rename must stay readable by ordinary members")

	// Admin tier: created at AdminRole, so its name is sealed at AdminRole and
	// an ordinary member must not be able to read it at all.
	adminName := randomChannelName(t, "admin-")
	adminID, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, adminName, "admins only",
		proto.RolePairOpt{Read: &proto.AdminRole, Write: &proto.AdminRole},
	)
	require.NoError(t, err)
	require.Equal(t, proto.RTChannelTier_Admin, sc.pubChannelTier(t, *adminID))

	// Renamed by ALICE, the team owner, whose role is above AdminRole. Renaming
	// as dara would seal at AdminRole whether the code used the tier's role or
	// the caller's, so that version of this assertion could not tell them
	// apart. dara must be able to read the result and bob must not.
	renamedAdmin := randomChannelName(t, "admin2-")
	require.NoError(t, sc.rename(t, sc.alice, sc.specFor(*adminID), renamedAdmin, ""))
	require.Equal(t, proto.RTChannelTier_Admin, sc.pubChannelTier(t, *adminID),
		"a rename must not move the channel between tiers")
	fromDara := sc.find(t, sc.dara, *adminID)
	require.NotNil(t, fromDara, "a team admin must still see an admin-tier channel")
	require.Equal(t, renamedAdmin, fromDara.Name,
		"an admin-tier rename must stay readable at AdminRole, not at the owner's role")
	require.Nil(t, sc.find(t, sc.bob, *adminID),
		"an admin-tier channel's name must not become visible to an ordinary member")
}

// A rename has to reach OTHER members, not just the device that made it. This
// is the only test that proves the channel-set version bump actually carries
// the new metadata across a second client; every single-actor test would pass
// with the bump missing entirely.
func TestRenameVisibleToOtherMembers(t *testing.T) {
	sc := setupMutScene(t)

	// Warm bob's cache at the pre-rename version, so the assertion below
	// measures an incremental update rather than a first fetch.
	require.Equal(t, sc.pubName, sc.find(t, sc.bob, sc.pubID).Name)

	newName := randomChannelName(t, "seen-")
	require.NoError(t, sc.rename(t, sc.alice, sc.pubSpec(), newName, "now with a description"))

	got := sc.find(t, sc.bob, sc.pubID)
	require.NotNil(t, got)
	require.Equal(t, newName, got.Name)
	require.NotNil(t, got.Desc)
	require.Equal(t, proto.RTChannelDesc("now with a description"), *got.Desc)
}

// Two devices renaming at once: one wins, the other loses the metadata CAS and
// its retry succeeds against the winner's seqno. The seqno lands at exactly +2
// -- proof both mutations applied in sequence rather than one silently
// clobbering the other.
func TestRenameCasRace(t *testing.T) {
	sc := setupMutScene(t)
	before := sc.find(t, sc.alice, sc.pubID).Seqno

	// Names generated HERE, on the test goroutine: randomChannelName calls
	// require, and testify's FailNow must not run off the test goroutine --
	// it would also leave errs[i] zero and let the assertions below pass.
	actors := []*privActor{sc.alice, sc.dara}
	names := []proto.RTChannelName{
		randomChannelName(t, "race-"), randomChannelName(t, "race-"),
	}
	var wg sync.WaitGroup
	errs := make([]error, len(actors))
	for i := range actors {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = actors[i].minder.UpdateChannel(actors[i].m, sc.teamCfg(),
				proto.RTAppID_Chat, sc.pubSpec(), names[i], "")
		}(i)
	}
	wg.Wait()

	// The loser retries internally, so both calls must report success.
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, before+2, sc.find(t, sc.alice, sc.pubID).Seqno,
		"both renames must have applied, each bumping the metadata seqno once")
}

// Renaming a channel onto a name another channel of the same tier already
// holds is refused. The server cannot compare ciphertext names, so this is the
// client's check -- but it is the only one there is.
func TestRenameOntoExistingNameFails(t *testing.T) {
	sc := setupMutScene(t)
	otherName := randomChannelName(t, "other-")
	_, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, otherName, "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.NoError(t, err)

	err = sc.rename(t, sc.alice, sc.pubSpec(), otherName, "")
	require.Error(t, err)
	require.IsType(t, core.RTChannelExistsError{}, err)

	// Renaming a channel to the name it already has is a no-op, not a clash
	// with itself.
	require.NoError(t, sc.rename(t, sc.alice, sc.pubSpec(), sc.pubName, ""))
}

// --- archive --------------------------------------------------------------

func TestArchiveRequiresAdmin(t *testing.T) {
	sc := setupMutScene(t)
	err := sc.setArchived(t, sc.bob, sc.pubSpec(), true)
	require.Error(t, err)
	require.IsType(t, core.PermissionError(""), err)
	require.NoError(t, sc.setArchived(t, sc.dara, sc.pubSpec(), true))
}

// An archived channel stops accepting messages.
func TestArchivedChannelRejectsSend(t *testing.T) {
	sc := setupMutScene(t)
	_, err := sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("before"))
	require.NoError(t, err)

	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	_, err = sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("after"))
	require.Error(t, err)
	require.IsType(t, core.RTChannelArchivedError{}, err,
		"a send into an archived channel must say so, not look like a missing channel")
}

// The invariant the archive design rests on. An archived channel stays in the
// team's channel listing carrying the flag, which is what reserves its name.
func TestArchivedStaysInTheListing(t *testing.T) {
	sc := setupMutScene(t)
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	for _, a := range []*privActor{sc.alice, sc.bob} {
		got := sc.find(t, a, sc.pubID)
		require.NotNil(t, got, "an archived channel must stay in the listing")
		require.True(t, got.Archived)
		require.Equal(t, sc.pubName, got.Name, "and keep its name, which is the point")
	}
}

// Nothing may take an archived channel's name while it is away. This is what
// makes unarchiving safe: the server cannot detect a name collision, so the
// only defence is that the collision never becomes possible.
func TestArchivedNameStaysReserved(t *testing.T) {
	sc := setupMutScene(t)
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	_, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, sc.pubName, "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.Error(t, err)
	require.IsType(t, core.RTChannelExistsError{}, err)

	// And unarchiving therefore cannot collide with anything.
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), false))
	require.False(t, sc.find(t, sc.alice, sc.pubID).Archived)
}

// Renaming an archived channel is how its reserved name is released -- which
// is why accessMutate passes the archived gate. Without this, a name taken by
// a channel somebody archived could never be used again.
func TestRenamedArchivedFreesTheName(t *testing.T) {
	sc := setupMutScene(t)
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	require.NoError(t, sc.rename(t, sc.alice, sc.pubSpec(), randomChannelName(t, "retired-"), ""))

	_, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, sc.pubName, "reusing the freed name",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.NoError(t, err)
}

// The default channel is protected by the CLIENT, and only by the client.
//
// "#general is never archived" is a SECO product rule (channels/CHANNELS.md
// 5.1), not a FOKS one: the realtime server has no concept of a default
// channel, cannot read channel names to recognise one, and another app on this
// server may well want to archive whatever it calls its first channel. An
// earlier version of this guessed -- the oldest public bottom-tier channel --
// and guessed wrong whenever anything public was created before #general,
// protecting the wrong channel while leaving the real one archivable. A rule
// the server enforces approximately is worse than one it does not enforce at
// all, so the server now has no opinion and librt refuses by name, which it
// can do exactly.
func TestCannotArchiveDefaultChannel(t *testing.T) {
	sc := setupPrivScene(t, true)
	_, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, "", "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.NoError(t, err)

	lst, err := sc.alice.minder.ListAllChannelsForTeam(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat)
	require.NoError(t, err)
	var defID *proto.RTChannelID
	for i := range lst.Channels {
		if lst.Channels[i].Name.IsEmpty() {
			defID = &lst.Channels[i].Id
		}
	}
	require.NotNil(t, defID, "the default channel should exist")

	err = sc.alice.minder.SetChannelArchived(sc.alice.m, sc.teamCfg(),
		proto.RTAppID_Chat, lcl.NewRTChannelSpecifierWithId(*defID), true)
	require.Error(t, err, "librt must refuse to archive the default channel")
	require.IsType(t, core.RTGenericError(""), err)
}

// --- archive and the inbox ------------------------------------------------

// The half the server cannot do alone. A delta of rows cannot express a
// removal, so archiving re-stamps every member's delivery row and the channel
// arrives once more carrying the flag; the client is what drops it. Without
// that handshake an archived channel would sit in the inbox forever.
func TestArchivedLeavesTheInbox(t *testing.T) {
	sc := setupMutScene(t)
	_, err := sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("hello"))
	require.NoError(t, err)

	// bob syncs and sees it.
	_, err = sc.bob.minder.SyncInbox(sc.bob.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	require.True(t, inboxHas(t, sc.bob, sc.pubID), "bob should see a live channel")

	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	_, err = sc.bob.minder.SyncInbox(sc.bob.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	require.False(t, inboxHas(t, sc.bob, sc.pubID),
		"an archived channel must drop out of the inbox on the next sync")
	// Against the stored index, not just the rendered view: LocalInbox also
	// skips an archived row defensively, so rendering alone cannot tell a
	// channel that was removed from one that is merely hidden. Removal is what
	// archiving is supposed to do.
	require.NotContains(t, inboxIndex(t, sc.bob), sc.pubID,
		"the archived channel must be gone from the stored inbox index")

	// Unarchiving brings it back, with its history.
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), false))
	_, err = sc.bob.minder.SyncInbox(sc.bob.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	require.True(t, inboxHas(t, sc.bob, sc.pubID),
		"unarchiving must restore the channel to the inbox")
	require.Contains(t, inboxIndex(t, sc.bob), sc.pubID)
}

func inboxIndex(t *testing.T, a *privActor) []proto.RTChannelID {
	ids, err := a.minder.InboxChannelIDs(a.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	return ids
}

func inboxHas(t *testing.T, a *privActor, id proto.RTChannelID) bool {
	view, err := a.minder.LocalInbox(a.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	for i := range view.Rows {
		if view.Rows[i].Ch.Id.Eq(id) {
			return true
		}
	}
	return false
}

// Archiving destroys nothing. After a round trip the thread is byte-identical,
// which is the whole difference between archive and the delete we did not
// build.
func TestArchiveIsReversible(t *testing.T) {
	sc := setupMutScene(t)
	const n = 4
	for i := 0; i < n; i++ {
		_, err := sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
			sc.pubSpec(), []byte{byte('a' + i)})
		require.NoError(t, err)
	}
	before, err := sc.alice.minder.GetThreadRecentMsgs(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, sc.pubSpec(), n)
	require.NoError(t, err)
	require.Len(t, before, n)

	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), false))

	after, err := sc.alice.minder.GetThreadRecentMsgs(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, sc.pubSpec(), n)
	require.NoError(t, err)
	require.Len(t, after, n)
	for i := range before {
		require.Equal(t, before[i].Seq, after[i].Seq)
		require.Equal(t, before[i].Body, after[i].Body)
	}

	// Sending works again once the channel is back.
	_, err = sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("after the round trip"))
	require.NoError(t, err)
}

// The late-join fan-in must never create a delivery row for an archived
// channel. It runs on every sync whose membership marker moved, so a miss here
// would re-create the row -- and burn an inbox version -- for as long as the
// account exists.
func TestArchivedNotFannedInOnJoin(t *testing.T) {
	sc := setupMutScene(t)
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	frank := sc.tew.NewTestUser(t)
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{frank.toMemberRole(t, proto.DefaultRole, sc.tm.hepks)}, nil)
	fa := newPrivActor(t, sc.tew, frank)

	// Baseline BEFORE frank's first sync. Taking it afterwards would fold a
	// first-sync fan-in bug into the baseline itself, and every later
	// comparison would then agree with it.
	deliveryRows := func() int {
		return sc.rtdbCount(t,
			`SELECT count(*) FROM user_channels WHERE short_host_id=$1 AND channel_id=$2`,
			sc.tew.MetaContext().ShortHostID(), sc.pubID.Short().Int64())
	}
	before := deliveryRows()

	// Three syncs: the first is the one that would fan frank in, the rest
	// catch a fan-in that re-creates the row on every pass.
	for i := 0; i < 3; i++ {
		_, err := fa.minder.SyncInbox(fa.m, proto.RTAppID_Chat)
		require.NoError(t, err)
		require.False(t, inboxHas(t, fa, sc.pubID))
		require.Equal(t, before, deliveryRows(),
			"the fan-in must not create a delivery row for an archived channel")
	}
}

// A channel whose DESCRIPTION will not decrypt must keep its name in the
// listing, because that name is what the collision map reserves. Dropping the
// row instead would let the next create -- or an unarchive -- claim a name
// that is still in use, and the server cannot detect that: names are
// PTK-encrypted, so the client's map is the only check there is.
//
// The name and the description are sealed under different key derivations, so
// one can fail while the other opens. Simulated here by flipping a byte inside
// the stored description ciphertext, which leaves the RTBoxRG well-formed (the
// server still decodes and serves it) and makes only the NaCl open fail, on
// the client, exactly as a box sealed with the wrong derivation would.
func TestUndecryptableDescriptionKeepsTheName(t *testing.T) {
	sc := setupMutScene(t)

	require.Equal(t, sc.pubName, sc.find(t, sc.alice, sc.pubID).Name)
	sc.corruptDescBox(t, sc.pubID)

	got := sc.find(t, sc.bob, sc.pubID)
	require.NotNil(t, got, "a bad description must not cost the channel its row")
	require.Equal(t, sc.pubName, got.Name)
	require.Nil(t, got.Desc, "the unreadable description is dropped, not guessed")

	// The invariant that matters: the name is still taken.
	_, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, sc.pubName, "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.Error(t, err)
	require.IsType(t, core.RTChannelExistsError{}, err)
}

// corruptDescBox flips one byte of a channel's stored description ciphertext,
// leaving the encoded RTBoxRG structurally valid so the server still serves it.
func (s *mutScene) corruptDescBox(t *testing.T, id proto.RTChannelID) {
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var box []byte
	require.NoError(t, db.QueryRow(m.Ctx(),
		`SELECT desc_box FROM channels WHERE short_host_id=$1 AND channel_id=$2`,
		m.ShortHostID(), id.Short().Int64()).Scan(&box))
	require.NotEmpty(t, box, "the channel under test needs a description")
	box[len(box)-1] ^= 0xff
	_, err = db.Exec(m.Ctx(),
		`UPDATE channels SET desc_box=$3 WHERE short_host_id=$1 AND channel_id=$2`,
		m.ShortHostID(), id.Short().Int64(), box)
	require.NoError(t, err)
}

// Re-granting a member who already has a delivery row must not allocate an
// inbox version.
//
// fanUserIntoChannel used to bump user_inbox and then insert ON CONFLICT DO
// NOTHING, so every re-grant left a version that stamped no row. A version no
// row carries cannot be reached by a client's cursor, so the user's head sits
// above it: SyncInbox stops on an empty delta without advancing and PollInbox
// returns instantly until the next real delivery. The source called it "a
// harmless gap"; it is the same defect that made unarchive leave clients
// polling, and the invariant suite found it once it was pointed at the grant
// path.
//
// Counted directly rather than through requireRTInvariants, because the point
// is that this operation adds exactly zero -- not that it stays under an
// allowance.
func TestRegrantDoesNotWasteInboxVersion(t *testing.T) {
	sc := setupPrivScene(t, false)

	sc.grant(t, sc.alice, sc.bob, false)
	before := sc.rtInvariantCounts(t)["orphan inbox versions"]

	// Same member again, promoted to owner: the ACL row changes, the delivery
	// row is already there and needs no new version.
	sc.grant(t, sc.alice, sc.bob, true)

	require.Equal(t, before, sc.rtInvariantCounts(t)["orphan inbox versions"],
		"re-granting an existing member allocated an inbox version that stamps "+
			"no row, leaving that member's inbox head permanently above anything "+
			"their client can sync to")
}

// --- the gates archive puts on other paths -------------------------------

// Reading an archived channel by explicit id still works (§6 Q1). Archive
// hides a channel and closes it to new activity; it does not destroy it, and
// the UI says "archived" rather than "deleted", so the history staying
// readable is the honest behaviour. In practice no client reaches one, because
// it is gone from the inbox.
func TestArchivedThreadStillReadableById(t *testing.T) {
	sc := setupMutScene(t)
	_, err := sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("before the archive"))
	require.NoError(t, err)
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	msgs, err := sc.alice.minder.GetThreadRecentMsgs(sc.alice.m, sc.teamCfg(),
		proto.RTAppID_Chat, sc.pubSpec(), 10)
	require.NoError(t, err, "an archived channel is hidden, not destroyed")
	require.Len(t, msgs, 1)
	require.Equal(t, []byte("before the archive"), msgs[0].Body)
}

// Marking read through is permitted on an archived channel: it writes only the
// caller's own row and cannot produce activity for anyone else. Refusing would
// make a client that marks read as a pane closes throw errors for no gain.
func TestArchivedAllowsReadThrough(t *testing.T) {
	sc := setupMutScene(t)
	res, err := sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("read me"))
	require.NoError(t, err)
	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))

	require.NoError(t, sc.alice.minder.ReadThrough(sc.alice.m, sc.teamCfg(),
		proto.RTAppID_Chat, sc.pubSpec(), res.Seq))
}

// Changing who is in a private channel is activity, so an archived one refuses
// it. This is what keeps archive reversible: the ACL it restores is the ACL it
// had.
func TestArchivedRejectsGrant(t *testing.T) {
	sc := setupPrivScene(t, false)
	spec := lcl.NewRTChannelSpecifierWithId(sc.chid)
	require.NoError(t, sc.alice.minder.SetChannelArchived(sc.alice.m,
		sc.teamCfg(), proto.RTAppID_Chat, spec, true))

	err := sc.alice.minder.GrantChannelMember(sc.alice.m, sc.chid, sc.bob.u.uid, false)
	require.Error(t, err)
	require.IsType(t, core.RTChannelArchivedError{}, err)

	// Unarchive and the same grant works, which is the point of refusing it.
	require.NoError(t, sc.alice.minder.SetChannelArchived(sc.alice.m,
		sc.teamCfg(), proto.RTAppID_Chat, spec, false))
	require.NoError(t, sc.alice.minder.GrantChannelMember(
		sc.alice.m, sc.chid, sc.bob.u.uid, false))
}

// pendingPushes counts a channel's undelivered push rows of one status.
func (s *mutScene) pendingPushes(t *testing.T, status string) int {
	return s.rtdbScalar(t, `
		SELECT count(*) FROM push_outbox
		WHERE short_host_id=$1 AND channel_id=$2 AND status=$3`,
		s.tew.MetaContext().ShortHostID(), s.pubID.Short().Int64(), status)
}

// Archiving discards the channel's queued push rows, which would otherwise
// fire after the room closed and buzz members about a channel that has just
// been shut.
func TestArchiveDropsQueuedPushes(t *testing.T) {
	sc := setupMutScene(t)
	_, err := sc.bob.minder.Send(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("buzz"))
	require.NoError(t, err)
	require.Positive(t, sc.pendingPushes(t, "queued"),
		"a send should have queued push rows to drop")

	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))
	require.Zero(t, sc.pendingPushes(t, "queued"),
		"archiving must discard the channel's queued pushes, or they fire after "+
			"the room has closed")
}

// And its HELD rows, which matter more: only a push hold's holder may decide
// one, and after the archive nothing can write to the channel again, so
// nothing will ever release them. They would sit in push_outbox forever.
//
// Worth its own test rather than folding into the queued case: a send under a
// hold produces held rows and a send without one produces queued rows, so a
// single scenario cannot cover both, and counting the two together would let
// either path regress unnoticed. An earlier version of this file claimed
// exactly that coverage and did not have it.
func TestArchiveDropsHeldPushes(t *testing.T) {
	sc := setupMutScene(t)

	// dara holds; bob's send is then written 'held' rather than 'queued'.
	sc.setHold(t, sc.dara)
	_, err := sc.bob.minder.Send(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
		sc.pubSpec(), []byte("under the hold"))
	require.NoError(t, err)
	require.Positive(t, sc.pendingPushes(t, "held"),
		"a send under a push hold should have written held rows")

	require.NoError(t, sc.setArchived(t, sc.alice, sc.pubSpec(), true))
	require.Zero(t, sc.pendingPushes(t, "held"),
		"archiving must discard the channel's held pushes: only the hold's "+
			"holder may decide one, and nothing can write to an archived channel "+
			"again, so nothing would ever release them")
	require.Zero(t, sc.pendingPushes(t, "queued"),
		"and archiving must not quietly release them into the queue instead")
}
