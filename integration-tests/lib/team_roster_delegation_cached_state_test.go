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

// A client that predates roster delegation replays the floor-setting link
// without complaint -- it ignores change metadata it does not know -- and
// saves its chain state with no floor. Once that device upgrades, the loader
// resumes from that state, so the first delegate-signed link opens against
// "no floor" and fails as "doer doesn't have privileged role". The load never
// succeeds, so the state is never rewritten: the team stays unloadable on that
// device until its cache is wiped.
func TestDelegateLinkLoadsOverFloorlessCachedState(t *testing.T) {
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

	mco := tew.NewClientMetaContext(t, owner)
	arg := libclient.LoadTeamArg{
		Team:    tm.FQTeam(t),
		As:      owner.FQUser().FQParty(),
		Keys:    owner.KeySeq(t, proto.OwnerRole),
		SrcRole: proto.OwnerRole,
	}
	_, err = libclient.LoadTeam(mco, arg)
	require.NoError(t, err)

	// Rewrite the saved state the way a pre-delegation client wrote it.
	scoper := mco.G().ActiveUser().FQU()
	var st lcl.TeamChainState
	_, err = mco.DbGet(&st, libclient.DbTypeSoft, &scoper, lcl.DataType_TeamChainState, arg)
	require.NoError(t, err)
	require.NotNil(t, st.RosterDelegationFloor)
	st.RosterDelegationFloor = nil
	err = mco.DbPut(libclient.DbTypeSoft, libclient.PutArg{
		Scope: &scoper,
		Typ:   lcl.DataType_TeamChainState,
		Val:   &st,
		Key:   arg,
	})
	require.NoError(t, err)

	ms, tms := tew.teamMinderFor(t, delg)
	err = tms.Add(ms, lcl.TeamAddArg{
		Team: *tm.ToFQTeamParsed(t),
		Members: []lcl.FQPartyParsedAndRole{{
			Fqp: proto.FQPartyParsed{Party: proto.NewParsedPartyWithFalse(newbie.uid.ToPartyID())},
		}},
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	wrp, err := libclient.LoadTeam(mco, arg)
	require.NoError(t, err)
	require.NotNil(t, wrp.RosterDelegationFloor())

	// The from-scratch replay rewrote the cache, so this device is healed
	// rather than replaying the whole chain on every load.
	st = lcl.TeamChainState{}
	_, err = mco.DbGet(&st, libclient.DbTypeSoft, &scoper, lcl.DataType_TeamChainState, arg)
	require.NoError(t, err)
	require.NotNil(t, st.RosterDelegationFloor)
}
