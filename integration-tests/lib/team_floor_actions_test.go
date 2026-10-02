// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"context"
	"testing"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

func setTeamFloorActions(t *testing.T, tew *TestEnvWrapper, actions []string, labels map[string]string) {
	cfg, ok := tew.MetaContext().G().Config().(*shared.ConfigJSonnet)
	require.True(t, ok)
	cfg.Lock()
	defer cfg.Unlock()
	if cfg.Data.Team == nil {
		cfg.Data.Team = &shared.TeamConfigJSON{}
	}
	cfg.Data.Team.FloorActions_ = actions
	cfg.Data.Team.RoleLabels_ = labels
}

// Channel creation as a floor action: in a team with a delegation floor,
// the host config can reserve open-channel creation for the floor role and
// up. Teams without a floor, and hosts without the config, keep the
// pre-existing rule (any member whose role clears the channel's roles).
func TestChannelCreateFloorAction(t *testing.T) {
	tew := testEnvBeta(t)
	setRosterDelegation(t, tew, true)
	setTeamFloorActions(t, tew, nil, nil)
	owner := tew.NewTestUser(t)
	delg := tew.NewTestUser(t)
	coco := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	m := tew.MetaContext()

	floor := proto.NewRoleWithMember(100)
	err := tm.setDelegationFloor(t, m, owner, floor, []proto.MemberRole{
		delg.toMemberRole(t, floor, tm.hepks),
		coco.toMemberRole(t, proto.NewRoleWithMember(0), tm.hepks),
	})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	fqt := tm.ToFQTeamParsed(t)
	mkChan := func(u *TestUser, name proto.RTChannelName) error {
		mc := librt.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
		minder := librt.NewMinder(mc.G().ActiveUser())
		_, err := minder.MakeChannel(mc, team.WrapNamedPtr(fqt), proto.RTAppID_Chat,
			name, "d", proto.RolePairOpt{})
		return err
	}

	// Without the config, a plain member of a floor team creates channels
	// as before.
	require.NoError(t, mkChan(coco, "pre-config"))

	setTeamFloorActions(t, tew, []string{"rt.channel.create_open"}, nil)

	// Now creation needs the floor: the plain member is refused...
	err = mkChan(coco, "nope")
	require.Error(t, err)
	require.ErrorContains(t, err, "delegation floor")

	// ...while the delegate and the owner pass.
	require.NoError(t, mkChan(delg, "delegate-made"))
	require.NoError(t, mkChan(owner, "owner-made"))

	// A team without a floor is untouched by the config.
	owner2 := tew.NewTestUser(t)
	plain2 := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm2 := tew.makeTeamForOwner(t, owner2)
	tm2.makeChanges(t, m, owner2, []proto.MemberRole{
		plain2.toMemberRole(t, proto.NewRoleWithMember(0), tm2.hepks),
	}, nil)
	tew.DirectDoubleMerklePokeInTest(t)
	fqt2 := tm2.ToFQTeamParsed(t)
	mc2 := librt.NewMetaContext(tew.NewClientMetaContextWithEracer(t, plain2))
	minder2 := librt.NewMinder(mc2.G().ActiveUser())
	_, err = minder2.MakeChannel(mc2, team.WrapNamedPtr(fqt2), proto.RTAppID_Chat,
		"floorless", "d", proto.RolePairOpt{})
	require.NoError(t, err)

	setTeamFloorActions(t, tew, nil, nil)
}

// GetTeamConfig serves the host's role labels and floor actions, so a
// client learns which role "delegate" is without hard-coding it. The labels
// go out only while roster delegation is on, so their presence tells a
// client the host has delegated roles at all.
func TestGetTeamConfigRoleLabels(t *testing.T) {
	tew := testEnvBeta(t)
	setTeamFloorActions(t, tew, []string{"rt.channel.create_open"},
		map[string]string{"delegate": "m/100", "helper": "m/50"})
	defer setTeamFloorActions(t, tew, nil, nil)
	defer setRosterDelegation(t, tew, false)
	u := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	ctx := context.Background()
	cli, closer := u.newTeamAdminClient(t, ctx)
	defer closer()

	setRosterDelegation(t, tew, false)
	cfg, err := cli.GetTeamConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"rt.channel.create_open"}, cfg.FloorActions)
	require.Empty(t, cfg.RoleLabels)

	setRosterDelegation(t, tew, true)
	cfg, err = cli.GetTeamConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"rt.channel.create_open"}, cfg.FloorActions)
	require.Equal(t, []rem.RoleLabel{
		{Name: "delegate", Role: proto.NewRoleWithMember(100)},
		{Name: "helper", Role: proto.NewRoleWithMember(50)},
	}, cfg.RoleLabels)
}
