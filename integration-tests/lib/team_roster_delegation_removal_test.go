// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"context"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

func (tm *teamObj) uidChange(t *testing.T, u *TestUser, to string) lcl.RoleChange {
	fqu, err := u.FQUser().StringErr()
	require.NoError(t, err)
	rc, err := core.ParseRoleChangeString(lcl.RoleChangeString(fqu + "/o->" + to))
	require.NoError(t, err)
	return *rc
}

func loadTeamAs(t *testing.T, tew *TestEnvWrapper, tm *teamObj, u *TestUser) (*libclient.TeamWrapper, error) {
	mc := tew.NewClientMetaContext(t, u)
	return libclient.LoadTeam(mc, libclient.LoadTeamArg{
		Team:    tm.FQTeam(t),
		As:      u.FQUser().FQParty(),
		Keys:    u.KeySeq(t, proto.OwnerRole),
		SrcRole: proto.OwnerRole,
	})
}

// The whole removal story through real clients: the owner raw-adds bob and
// a future delegate, promotes the delegate and sets the floor in one real
// TeamChangeRoles call (which bulk-fills the delegate boxes for the
// pre-floor members), and the delegate -- no admin key, no token -- removes
// bob, then adds and removes carl. The owner can still read everything.
func TestDelegateRemovesMemberEndToEnd(t *testing.T) {
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

	// insLocalPermsFor mirrors what a real add does on an open host, so the
	// owner's member-loading client can read these members' chains later.
	_, err := tm.makeChangesFull(t, m, owner, []proto.MemberRole{
		delg.toMemberRole(t, m0, tm.hepks),
		bob.toMemberRole(t, m0, tm.hepks),
	}, nil, makeChangesKnobs{
		insLocalPermsFor: []proto.PartyID{delg.uid.ToPartyID(), bob.uid.ToPartyID()},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// Promote + set the floor through the real client; this is the link
	// that owes (and bulk-builds) the delegate fills for bob.
	mo, tmo := tew.teamMinderFor(t, owner)
	err = tmo.TeamChangeRoles(mo, lcl.TeamChangeRolesArg{
		Team:            *tm.ToFQTeamParsed(t),
		Changes:         []lcl.RoleChange{tm.uidChange(t, delg, "m/100")},
		DelegationFloor: ptrTo(proto.NewRoleWithMember(100)),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// Upstream, a delegate may remove any plain member; this fork narrows
	// that to members the delegate added themselves (the own-invitees rule,
	// docs/team-roster-delegation.md). bob was added by the OWNER, so the
	// delegate's removal is refused and bob stays a member.
	ms, tms := tew.teamMinderFor(t, delg)
	err = tms.TeamChangeRoles(ms, lcl.TeamChangeRolesArg{
		Team:    *tm.ToFQTeamParsed(t),
		Changes: []lcl.RoleChange{tm.uidChange(t, bob, "n")},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "signer added")

	wrp0, err := loadTeamAs(t, tew, tm, bob)
	require.NoError(t, err)
	require.NotNil(t, wrp0)

	// Delegate adds carl (the add boxes carl's delegate key) and removes him.
	err = tms.Add(ms, lcl.TeamAddArg{
		Team: *tm.ToFQTeamParsed(t),
		Members: []lcl.FQPartyParsedAndRole{{
			Fqp: proto.FQPartyParsed{Party: proto.NewParsedPartyWithFalse(carl.uid.ToPartyID())},
		}},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	wrp, err := loadTeamAs(t, tew, tm, carl)
	require.NoError(t, err)
	require.NotNil(t, wrp)

	err = tms.TeamChangeRoles(ms, lcl.TeamChangeRolesArg{
		Team:    *tm.ToFQTeamParsed(t),
		Changes: []lcl.RoleChange{tm.uidChange(t, carl, "n")},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	_, err = loadTeamAs(t, tew, tm, carl)
	require.Error(t, err)

	// The owner is untouched by the delegate's rotations.
	wrp, err = loadTeamAs(t, tew, tm, owner)
	require.NoError(t, err)
	require.NotNil(t, wrp)
}

func ptrTo[T any](v T) *T { return &v }

// Server gates on the delegate-box fetch, driven as raw RPCs.
func TestDelegatedRemovalKeyBoxGates(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	owner := tew.NewTestUser(t)
	delg := tew.NewTestUser(t)
	vip := tew.NewTestUser(t)
	bob := tew.NewTestUser(t)
	plain := tew.NewTestUser(t)
	outsider := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()

	floor := proto.NewRoleWithMember(100)
	err := tm.setDelegationFloor(t, m, owner, floor, []proto.MemberRole{
		delg.toMemberRole(t, floor, tm.hepks),
		vip.toMemberRole(t, proto.NewRoleWithMember(5), tm.hepks),
		bob.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
		plain.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	tmid, err := tm.id.ToTeamID()
	require.NoError(t, err)

	ctx := context.Background()
	fetch := func(u *TestUser, target *TestUser) (proto.TeamRemovalKeyBox, error) {
		cli, closer := u.newTeamMemberClient(t, ctx)
		defer closer()
		return cli.LoadDelegatedRemovalKeyBox(ctx, rem.LoadDelegatedRemovalKeyBoxArg{
			Team:    tmid,
			Member:  target.FQUser().FQParty(),
			SrcRole: proto.OwnerRole,
		})
	}

	// The delegate gets bob's box, and it holds the very key the admins have.
	box, err := fetch(delg, bob)
	require.NoError(t, err)
	fptk := tm.ptks[core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 100}]
	require.NotNil(t, fptk)
	var payload rem.TeamRemovalKeyBoxPayload
	_, err = fptk.UnboxFor(&payload, box.Box, nil)
	require.NoError(t, err)
	fqef, err := bob.FQUser().FQParty().FQEntity().Fixed()
	require.NoError(t, err)
	wantKey := tm.removalKeys[teamMemberID(*fqef)]
	require.Equal(t, wantKey, payload.Key)

	// A plain member is below the floor.
	_, err = fetch(plain, bob)
	require.Error(t, err)
	require.ErrorContains(t, err, "below the team's roster delegation floor")

	// A non-member gets nothing.
	_, err = fetch(outsider, bob)
	require.Error(t, err)

	// No delegate box exists for a member above m/0.
	_, err = fetch(delg, vip)
	require.Error(t, err)

	// With the floor cleared, even the delegate is refused.
	err = tm.setDelegationFloor(t, m, owner, proto.NewRoleDefault(proto.RoleType_NONE), nil)
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	_, err = fetch(delg, bob)
	require.Error(t, err)
	require.ErrorContains(t, err, "does not delegate")
}

func teamMemberID(fqe proto.FQEntityFixed) team.MemberID {
	return team.MemberID{Fqe: fqe, SrcRole: core.OwnerRole}
}

// The coverage invariant and the fills gate, with clients that misbehave.
func TestDelegateRemovalKeyCoverageEnforced(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	owner := tew.NewTestUser(t)
	delg := tew.NewTestUser(t)
	bob := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()
	m0 := proto.NewRoleWithMember(0)
	floor := proto.NewRoleWithMember(100)

	tm.makeChanges(t, m, owner, []proto.MemberRole{
		bob.toMemberRole(t, m0, tm.hepks),
	}, nil)
	tew.DirectDoubleMerklePokeInTest(t)

	// Setting the floor without the fills it owes (for bob) is refused.
	_, err := tm.makeChangesFull(t, m, owner, []proto.MemberRole{
		delg.toMemberRole(t, floor, tm.hepks),
	}, nil, makeChangesKnobs{
		md: []proto.ChangeMetadata{
			proto.NewChangeMetadataWithRosterdelegationfloor(floor),
		},
		skipDelegate: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "boxed for the floor role")

	// Done right, it goes through.
	err = tm.setDelegationFloor(t, m, owner, floor, []proto.MemberRole{
		delg.toMemberRole(t, floor, tm.hepks),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// An add that omits the new member's delegate box is refused.
	carl := tew.NewTestUser(t)
	_, err = tm.makeChangesFull(t, m, owner, []proto.MemberRole{
		carl.toMemberRole(t, m0, tm.hepks),
	}, nil, makeChangesKnobs{skipDelegate: true})
	require.Error(t, err)
	require.ErrorContains(t, err, "boxed for the floor role")

	// A delegated signer may not send fills: only floor changes and
	// demotions owe them, and both are admin-only.
	fqef, err := bob.FQUser().FQParty().FQEntity().Fixed()
	require.NoError(t, err)
	key := tm.removalKeys[teamMemberID(*fqef)]
	fptk := tm.ptks[core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 100}]
	boxer, err := core.PublicizeToSPSBoxer(fptk, tm.FQTeam(t).FQParty())
	require.NoError(t, err)
	sender := delg.puks[core.OwnerRole]
	fbox, err := team.BoxRemovalKeyForReceiver(&sender, boxer,
		rem.TeamRemovalKeyMetadata{
			Tm:      tm.FQTeam(t),
			Member:  bob.FQUser().FQParty(),
			SrcRole: proto.OwnerRole,
		}, &key)
	require.NoError(t, err)
	floorRk := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 100}
	_, err = tm.makeChangesFull(t, m, delg, []proto.MemberRole{
		carl.toMemberRole(t, m0, tm.hepks),
	}, nil, makeChangesKnobs{
		gameplanOpts: &team.GameplanOpts{DelegationFloor: &floorRk},
		extraFills: []rem.TeamDelegateRemovalKeyFill{{
			Member:  bob.FQUser().FQParty(),
			SrcRole: proto.OwnerRole,
			Box:     *fbox,
		}},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "only an admin may send delegate removal key fills")
}

// A delegate whose floor key rotated after they added someone can still
// open that member's delegate box, made at the old generation. (Upstream's
// variant of this test has a late-promoted delegate open the old box; this
// fork's own-invitees rule means only the adder may remove, so here the
// adder opens their own old-generation box after a rotation.)
func TestDelegateOpensOldGenerationDelegateBox(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	vh := tew.openVHost(t)
	owner := tew.NewTestUserAtVHost(t, vh)
	delg1 := tew.NewTestUserAtVHost(t, vh)
	delg2 := tew.NewTestUserAtVHost(t, vh)
	bob := tew.NewTestUserAtVHost(t, vh)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()
	floor := proto.NewRoleWithMember(100)
	floorKey := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 100}

	// Floor on, with two delegates.
	tm.delegationFloor = &floorKey
	_, err := tm.makeChangesFull(t, m, owner, []proto.MemberRole{
		delg1.toMemberRole(t, floor, tm.hepks),
		delg2.toMemberRole(t, floor, tm.hepks),
	}, nil, makeChangesKnobs{
		md: []proto.ChangeMetadata{
			proto.NewChangeMetadataWithRosterdelegationfloor(floor),
		},
		insLocalPermsFor: []proto.PartyID{delg1.uid.ToPartyID(), delg2.uid.ToPartyID()},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// delg1 adds bob; bob's delegate box is made at floor generation 1.
	floorRk := floorKey
	_, err = tm.makeChangesFull(t, m, delg1, []proto.MemberRole{
		bob.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	}, nil, makeChangesKnobs{
		gameplanOpts:     &team.GameplanOpts{DelegationFloor: &floorRk},
		insLocalPermsFor: []proto.PartyID{bob.uid.ToPartyID()},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	// Removing delg2 rotates every key at or below m/100. skipDelegate so
	// bob's box stays at the old generation -- the coverage check is
	// satisfied by the existing box, whose generation it does not care
	// about.
	_, err = tm.makeChangesFull(t, m, owner, []proto.MemberRole{
		delg2.toMemberRole(t, proto.NewRoleDefault(proto.RoleType_NONE), tm.hepks),
	}, nil, makeChangesKnobs{skipDelegate: true})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	require.False(t, tm.ptks[floorKey].Metadata().Gen.IsFirst(),
		"the floor PTK must have rotated for this test to mean anything")

	// delg1 removes bob through the real client, which fetches bob's
	// delegate box (generation 1) and must open it under the rotated key
	// ring (generation 2 current).
	ms, tms := tew.teamMinderFor(t, delg1)
	err = tms.TeamChangeRoles(ms, lcl.TeamChangeRolesArg{
		Team:    *tm.ToFQTeamParsed(t),
		Changes: []lcl.RoleChange{tm.uidChange(t, bob, "n")},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	_, err = loadTeamAs(t, tew, tm, bob)
	require.Error(t, err)
}
