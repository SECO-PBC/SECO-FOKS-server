// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// The whole delegate story, through the real client stack: an owner sets the
// floor and promotes a delegate in one link; the delegate -- who holds no
// admin key and gets no bearer token -- adds a new member as the logged-in
// signer; the new member can then load the team and its keys.
func TestDelegateAddsMemberEndToEnd(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	vh := tew.openVHost(t)
	owner := tew.NewTestUserAtVHost(t, vh)
	delg := tew.NewTestUserAtVHost(t, vh)
	newbie := tew.NewTestUserAtVHost(t, vh)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()

	floor := proto.NewRoleWithMember(100)
	err := tm.setDelegationFloor(t, m, owner, floor, []proto.MemberRole{
		delg.toMemberRole(t, floor, tm.hepks),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	ms, tms := tew.teamMinderFor(t, delg)
	err = tms.Add(ms, lcl.TeamAddArg{
		Team: *tm.ToFQTeamParsed(t),
		Members: []lcl.FQPartyParsedAndRole{{
			Fqp: proto.FQPartyParsed{Party: proto.NewParsedPartyWithFalse(newbie.uid.ToPartyID())},
		}},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// The added member loads the team, which proves both the roster entry
	// and that the delegate boxed the keys (and the removal key) correctly.
	mcn := tew.NewClientMetaContext(t, newbie)
	wrp, err := libclient.LoadTeam(mcn, libclient.LoadTeamArg{
		Team:    tm.FQTeam(t),
		As:      newbie.FQUser().FQParty(),
		Keys:    newbie.KeySeq(t, proto.OwnerRole),
		SrcRole: proto.OwnerRole,
	})
	require.NoError(t, err)
	require.NotNil(t, wrp)
	require.NotNil(t, wrp.RemovalKey())
}

// Server-side enforcement of the delegated-change rule: a client that lies
// to itself (wrong floor in its own Gameplan opts, or no client checks at
// all) still cannot get an illegal link past the server, which opens every
// link against its own stored floor.
func TestDelegateRosterRuleEnforcedServerSide(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	owner := tew.NewTestUser(t)
	delg := tew.NewTestUser(t)
	newbie := tew.NewTestUser(t)
	plain := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()

	floor := proto.NewRoleWithMember(100)
	floorKey := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 100}
	err := tm.setDelegationFloor(t, m, owner, floor, []proto.MemberRole{
		delg.toMemberRole(t, floor, tm.hepks),
		plain.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	honest := &team.GameplanOpts{DelegationFloor: &floorKey}
	noCheck := &team.GameplanOpts{TestingNoCheck: true}

	// The delegate's legitimate raw add is accepted by the server.
	_, err = tm.makeChangesFull(t, m, delg, []proto.MemberRole{
		newbie.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	}, nil, makeChangesKnobs{gameplanOpts: honest})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// With client checks disabled, the server still refuses an add above
	// m/0...
	badRole := tew.NewTestUser(t)
	_, err = tm.makeChangesFull(t, m, delg, []proto.MemberRole{
		badRole.toMemberRole(t, proto.NewRoleWithMember(5), tm.hepks),
	}, nil, makeChangesKnobs{gameplanOpts: noCheck})
	require.Error(t, err)
	require.ErrorContains(t, err, "m/0 or below")

	// ...and a delegated link that tries to carry the floor itself.
	_, err = tm.makeChangesFull(t, m, delg, []proto.MemberRole{
		badRole.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	}, nil, makeChangesKnobs{
		gameplanOpts: noCheck,
		md: []proto.ChangeMetadata{
			proto.NewChangeMetadataWithRosterdelegationfloor(proto.NewRoleWithMember(100)),
		},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "link metadata")

	// A plain member below the floor gets nowhere even with a lying client.
	_, err = tm.makeChangesFull(t, m, plain, []proto.MemberRole{
		badRole.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	}, nil, makeChangesKnobs{gameplanOpts: noCheck})
	require.Error(t, err)
	require.ErrorContains(t, err, "delegation floor")

	// Once an admin clears the floor, the delegate's next link -- built on a
	// client that still believes in the old floor -- is refused: the server
	// decides from its own copy, read under the team lock.
	err = tm.setDelegationFloor(t, m, owner, proto.NewRoleDefault(proto.RoleType_NONE), nil)
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	_, err = tm.makeChangesFull(t, m, delg, []proto.MemberRole{
		badRole.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	}, nil, makeChangesKnobs{gameplanOpts: honest})
	require.Error(t, err)
	require.ErrorContains(t, err, "privileged role")
}
