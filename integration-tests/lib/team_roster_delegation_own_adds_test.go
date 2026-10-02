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

// Fork-only: a delegated remover may only remove members whose current
// membership they added themselves. The adder is the signer of the add
// link, so re-adding someone moves them to whoever signed the re-add.
func TestDelegatedRemovalOwnAddsOnly(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	vh := tew.openVHost(t)
	owner := tew.NewTestUserAtVHost(t, vh)
	delg := tew.NewTestUserAtVHost(t, vh)
	bob := tew.NewTestUserAtVHost(t, vh)
	carl := tew.NewTestUserAtVHost(t, vh)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()
	m0 := proto.NewRoleWithMember(0)
	floor := proto.NewRoleWithMember(100)

	_, err := tm.makeChangesFull(t, m, owner, []proto.MemberRole{
		delg.toMemberRole(t, m0, tm.hepks),
		bob.toMemberRole(t, m0, tm.hepks),
	}, nil, makeChangesKnobs{
		insLocalPermsFor: []proto.PartyID{delg.uid.ToPartyID(), bob.uid.ToPartyID()},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	mo, tmo := tew.teamMinderFor(t, owner)
	err = tmo.TeamChangeRoles(mo, lcl.TeamChangeRolesArg{
		Team:            *tm.ToFQTeamParsed(t),
		Changes:         []lcl.RoleChange{tm.uidChange(t, delg, "m/100")},
		DelegationFloor: ptrTo(proto.NewRoleWithMember(100)),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	_ = floor

	ms, tms := tew.teamMinderFor(t, delg)
	addAs := func(mc libclient.MetaContext, by *libclient.TeamMinder, who *TestUser) error {
		return by.Add(mc, lcl.TeamAddArg{
			Team: *tm.ToFQTeamParsed(t),
			Members: []lcl.FQPartyParsedAndRole{{
				Fqp: proto.FQPartyParsed{Party: proto.NewParsedPartyWithFalse(who.uid.ToPartyID())},
			}},
		})
	}
	require.NoError(t, addAs(ms, tms, carl))
	tew.DirectDoubleMerklePokeInTest(t)

	remove := func(mc libclient.MetaContext, by *libclient.TeamMinder, who *TestUser) error {
		return by.TeamChangeRoles(mc, lcl.TeamChangeRolesArg{
			Team:    *tm.ToFQTeamParsed(t),
			Changes: []lcl.RoleChange{tm.uidChange(t, who, "n")},
		})
	}

	// Not the delegate's add: refused, and the member stays.
	err = remove(ms, tms, bob)
	require.Error(t, err)
	require.ErrorContains(t, err, "signer added")

	// Their own add: fine.
	require.NoError(t, remove(ms, tms, carl))
	tew.DirectDoubleMerklePokeInTest(t)

	// The owner re-adds carl; carl's current membership now belongs to the
	// owner, so the delegate who added him the first time is refused.
	require.NoError(t, addAs(mo, tmo, carl))
	tew.DirectDoubleMerklePokeInTest(t)
	err = remove(ms, tms, carl)
	require.Error(t, err)
	require.ErrorContains(t, err, "signer added")

	// Admins are untouched by the rule.
	require.NoError(t, remove(mo, tmo, carl))
	tew.DirectDoubleMerklePokeInTest(t)
	require.NoError(t, remove(mo, tmo, bob))
}
