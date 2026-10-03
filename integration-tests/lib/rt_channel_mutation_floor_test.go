// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// Fork floor actions rt.channel.edit and rt.channel.archive_open: with the
// host config set and the team delegating, a member at or above the floor
// may rename/edit any channel they can reach and archive an OPEN one. The
// built-in admin rule stays for everyone else, for teams without a floor,
// and for archiving a private channel.

func TestChannelEditArchiveFloorActions(t *testing.T) {
	sc := setupMutScene(t)
	tew := sc.tew
	setRosterDelegation(t, tew, true)
	defer setRosterDelegation(t, tew, false)
	defer setTeamFloorActions(t, tew, nil, nil)
	m := tew.MetaContext()

	// Configured, but the team has no floor yet: nothing opens.
	setTeamFloorActions(t, tew, []string{"rt.channel.edit", "rt.channel.archive_open"}, nil)
	err := sc.rename(t, sc.eddie, sc.pubSpec(), randomChannelName(t, "early-"), "")
	require.IsType(t, core.PermissionError(""), err)

	// eddie becomes the delegate.
	floor := proto.NewRoleWithMember(100)
	err = sc.tm.setDelegationFloor(t, m, sc.alice.u, floor, []proto.MemberRole{
		sc.eddie.u.toMemberRole(t, floor, sc.tm.hepks),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// Without the actions configured, the floor changes nothing.
	setTeamFloorActions(t, tew, nil, nil)
	err = sc.rename(t, sc.eddie, sc.pubSpec(), randomChannelName(t, "nope-"), "")
	require.IsType(t, core.PermissionError(""), err)
	err = sc.setArchived(t, sc.eddie, sc.pubSpec(), true)
	require.IsType(t, core.PermissionError(""), err)

	// rt.channel.edit alone opens rename/edit, not archive.
	setTeamFloorActions(t, tew, []string{"rt.channel.edit"}, nil)
	newName := randomChannelName(t, "steward-")
	require.NoError(t, sc.rename(t, sc.eddie, sc.pubSpec(), newName, "renamed by a delegate"))
	require.Equal(t, newName, sc.find(t, sc.alice, sc.pubID).Name)
	err = sc.setArchived(t, sc.eddie, sc.pubSpec(), true)
	require.IsType(t, core.PermissionError(""), err)

	// A plain member is still refused.
	err = sc.rename(t, sc.bob, sc.pubSpec(), randomChannelName(t, "bob-"), "")
	require.IsType(t, core.PermissionError(""), err)

	// The floor never reaches an admin-tier channel: the tier gate runs first.
	adminID, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, randomChannelName(t, "admin-"), "",
		proto.RolePairOpt{Read: &proto.AdminRole, Write: &proto.AdminRole},
	)
	require.NoError(t, err)
	err = sc.rename(t, sc.eddie, sc.specFor(*adminID), randomChannelName(t, "tier-"), "")
	require.Error(t, err)

	// rt.channel.archive_open opens archive and unarchive of an open channel.
	setTeamFloorActions(t, tew, []string{"rt.channel.edit", "rt.channel.archive_open"}, nil)
	err = sc.setArchived(t, sc.bob, sc.pubSpec(), true)
	require.IsType(t, core.PermissionError(""), err)
	require.NoError(t, sc.setArchived(t, sc.eddie, sc.pubSpec(), true))
	require.NoError(t, sc.setArchived(t, sc.eddie, sc.pubSpec(), false))
}

// Archiving a PRIVATE channel stays admin-only even with both actions
// configured; renaming one the delegate is in opens like an open channel;
// and a delegate outside a private channel still sees nothing.
func TestChannelArchivePrivateStaysAdmin(t *testing.T) {
	sc := setupPrivScene(t, false)
	tew := sc.tew
	setRosterDelegation(t, tew, true)
	defer setRosterDelegation(t, tew, false)
	defer setTeamFloorActions(t, tew, nil, nil)
	m := tew.MetaContext()

	fredU := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	floor := proto.NewRoleWithMember(100)
	err := sc.tm.setDelegationFloor(t, m, sc.alice.u, floor, []proto.MemberRole{
		sc.eddie.u.toMemberRole(t, floor, sc.tm.hepks),
		fredU.toMemberRole(t, floor, sc.tm.hepks),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	fred := newPrivActor(t, tew, fredU)
	sc.grant(t, sc.alice, sc.eddie, false)
	setTeamFloorActions(t, tew, []string{"rt.channel.edit", "rt.channel.archive_open"}, nil)

	spec := lcl.NewRTChannelSpecifierWithId(sc.chid)
	err = sc.eddie.minder.SetChannelArchived(sc.eddie.m, sc.teamCfg(), proto.RTAppID_Chat, spec, true)
	require.IsType(t, core.PermissionError(""), err)

	require.NoError(t, sc.eddie.minder.UpdateChannel(sc.eddie.m, sc.teamCfg(), proto.RTAppID_Chat,
		spec, randomChannelName(t, "priv-"), "", false))

	// A delegate outside it cannot even find it: the client resolves the
	// channel from its own listing first, where it is absent.
	err = fred.minder.UpdateChannel(fred.m, sc.teamCfg(), proto.RTAppID_Chat,
		spec, randomChannelName(t, "outside-"), "", false)
	require.IsType(t, core.RTNotFoundError(""), err)

	// An admin still archives it.
	require.NoError(t, sc.dara.minder.SetChannelArchived(sc.dara.m, sc.teamCfg(), proto.RTAppID_Chat, spec, true))
}
