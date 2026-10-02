// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// Fork-only floor action rt.channel.revoke_private: with the host config
// set and the team delegating, a member at or above the floor may remove
// another member from a private channel they are in. Everything else about
// the ACL stays as it was: grant is still owner-or-admin, a floor member
// outside the channel still sees nothing, and a plain member in it still
// cannot revoke.
func TestPrivateRevokeOpenedToDelegationFloor(t *testing.T) {
	sc := setupPrivScene(t, false)
	tew := sc.tew
	setRosterDelegation(t, tew, true)
	defer setRosterDelegation(t, tew, false)
	defer setTeamFloorActions(t, tew, nil, nil)
	m := tew.MetaContext()

	// eddie becomes the delegate; fred is a second delegate who never joins
	// the channel.
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
	sc.grant(t, sc.alice, sc.bob, false)
	sc.grant(t, sc.alice, sc.cleo, false)

	// Without the action configured, the floor changes nothing.
	err = sc.eddie.minder.RevokeChannelMember(sc.eddie.m, sc.chid, sc.bob.u.uid)
	require.Error(t, err)
	require.ErrorContains(t, err, "channel owner or team admin")

	setTeamFloorActions(t, tew, []string{"rt.channel.revoke_private"}, nil)

	// A plain member in the channel still cannot revoke...
	err = sc.cleo.minder.RevokeChannelMember(sc.cleo.m, sc.chid, sc.bob.u.uid)
	require.Error(t, err)
	require.ErrorContains(t, err, "channel owner or team admin")

	// ...the delegate in the channel can...
	sc.revoke(t, sc.eddie, sc.bob)

	// ...but cannot grant anyone in.
	err = sc.eddie.minder.GrantChannelMember(sc.eddie.m, sc.chid, sc.bob.u.uid, false)
	require.Error(t, err)
	require.ErrorContains(t, err, "channel owner or team admin")

	// A delegate with no ACL row sees nothing: the missing-row gate is
	// untouched, so the channel stays hidden rather than refused.
	err = fred.minder.RevokeChannelMember(fred.m, sc.chid, sc.cleo.u.uid)
	requireHidden(t, err, "revoke from outside")

	// Self-revoke (leave) is a different access kind and stays open to any
	// member of the channel.
	err = sc.cleo.minder.RevokeChannelMember(sc.cleo.m, sc.chid, sc.cleo.u.uid)
	require.NoError(t, err)
	sc.deliberateOrphans++
}
