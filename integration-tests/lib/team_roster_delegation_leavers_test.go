// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// Fork-only: besides members they added, a delegated remover may remove
// members who have left the team themselves (TeamLeaveSelf) since their
// current membership was added, whoever added them.
func TestDelegatedRemovalOfSelfLeftMembers(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	vh := tew.openVHost(t)
	owner := tew.NewTestUserAtVHost(t, vh)
	delg := tew.NewTestUserAtVHost(t, vh)
	bob := tew.NewTestUserAtVHost(t, vh)
	carl := tew.NewTestUserAtVHost(t, vh)
	dave := tew.NewTestUserAtVHost(t, vh)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()
	fqtp := *tm.ToFQTeamParsed(t)
	// Invites (the re-invite case below) need a team index range.
	tm.setIndexRange(t, m, owner, index0)
	tew.DirectDoubleMerklePokeInTest(t)

	_, err := tm.makeChangesFull(t, m, owner, []proto.MemberRole{
		delg.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	}, nil, makeChangesKnobs{
		insLocalPermsFor: []proto.PartyID{delg.uid.ToPartyID()},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	mo, tmo := tew.teamMinderFor(t, owner)
	err = tmo.TeamChangeRoles(mo, lcl.TeamChangeRolesArg{
		Team:            fqtp,
		Changes:         []lcl.RoleChange{tm.uidChange(t, delg, "m/100")},
		DelegationFloor: ptrTo(proto.NewRoleWithMember(100)),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	ms, tms := tew.teamMinderFor(t, delg)
	addAs := func(mc libclient.MetaContext, by *libclient.TeamMinder, who *TestUser) {
		err := by.Add(mc, lcl.TeamAddArg{
			Team: fqtp,
			Members: []lcl.FQPartyParsedAndRole{{
				Fqp: proto.FQPartyParsed{Party: proto.NewParsedPartyWithFalse(who.uid.ToPartyID())},
			}},
		})
		require.NoError(t, err)
		tew.DirectDoubleMerklePokeInTest(t)
	}
	remove := func(mc libclient.MetaContext, by *libclient.TeamMinder, who *TestUser) error {
		err := by.TeamChangeRoles(mc, lcl.TeamChangeRolesArg{
			Team:    fqtp,
			Changes: []lcl.RoleChange{tm.uidChange(t, who, "n")},
		})
		tew.DirectDoubleMerklePokeInTest(t)
		return err
	}
	// The leaver's client writes Approved on its membership chain once it
	// finds the team, then Removed on leaving; the roster is unchanged.
	leave := func(who *TestUser) libclient.MetaContext {
		mw, tmw := tew.teamMinderFor(t, who)
		require.NoError(t, tmw.ExploreAndIndex(mw, nil))
		tew.DirectDoubleMerklePokeInTest(t)
		require.NoError(t, tmw.TeamLeaveSelf(mw, fqtp))
		tew.DirectDoubleMerklePokeInTest(t)
		return mw
	}
	const refusal = "a delegated change may only remove members its signer added"

	for _, u := range []*TestUser{bob, carl, dave} {
		addAs(mo, tmo, u)
	}

	// Leader-added and not left: refused, with the text the app keys on.
	err = remove(ms, tms, bob)
	require.Error(t, err)
	require.ErrorContains(t, err, refusal)

	// Leader-added, then left: the Steward may clear them.
	leave(bob)
	require.NoError(t, remove(ms, tms, bob))

	// Left, then removed and re-added by the Leader: the leave belongs to
	// the earlier membership, so it does not cover the current one.
	leave(carl)
	require.NoError(t, remove(mo, tmo, carl))
	addAs(mo, tmo, carl)
	err = remove(ms, tms, carl)
	require.Error(t, err)
	require.ErrorContains(t, err, refusal)

	// Left, then accepted a re-invite: accepting posts a Requested link
	// after the Removed one, and the leave still counts. This is the
	// Steward's Accept of a returning member, which clears the old entry.
	md := leave(dave)
	inv, err := tmo.CreateInvite(mo, fqtp)
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	_, tmd := tew.teamMinderFor(t, dave)
	_, err = tmd.AcceptInvite(md, lcl.TeamAcceptInviteArg{I: *inv})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	require.NoError(t, remove(ms, tms, dave))
}
