// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package cli

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// The CLI shape of roster delegation: the owner raises a member to the
// floor, sets the floor with --delegation-floor, and the member -- who
// holds no admin key -- adds a plain member with "team add" but can
// neither promote anyone nor touch the floor.
func TestTeamDelegationFloorCLI(t *testing.T) {
	cfg, ok := globalTestEnv.MetaContext().G().Config().(*shared.ConfigJSonnet)
	require.True(t, ok)
	setSwitch := func(on bool) {
		cfg.Lock()
		defer cfg.Unlock()
		if cfg.Data.Team == nil {
			cfg.Data.Team = &shared.TeamConfigJSON{}
		}
		cfg.Data.Team.RosterDelegation_ = on
	}
	setSwitch(true)
	defer setSwitch(false)

	x := newTestAgent(t)
	x.runAgent(t)
	defer x.stop(t)
	// VHost "4" is the host dedicated to open viewership (as in lib/ tests),
	// which "team add" by username needs.
	newUserWithAgentAtVHost(t, x, 4)
	merklePoke(t)
	merklePoke(t)

	var res lcl.TeamCreateRes
	rs, err := core.RandomBase36String(5)
	require.NoError(t, err)
	teamName := "team_" + rs
	x.runCmdToJSON(t, &res, "team", "create", teamName)
	merklePoke(t)

	var xUser lcl.UserMetadataAndSigchainState
	x.runCmdToJSON(t, &xUser, "user", "load-me")
	m := globalTestEnv.MetaContext()
	chid, err := m.G().HostIDMap().LookupByHostID(m, xUser.Fqu.HostID)
	require.NoError(t, err)
	err = shared.VHostSetUserViewership(m.WithHostID(chid), proto.ViewershipMode_Open)
	require.NoError(t, err)

	mkUser := func() (*testAgent, string) {
		a := newTestAgent(t)
		a.runAgent(t)
		newUserWithAgentAtVHost(t, a, 4)
		merklePoke(t)
		merklePoke(t)
		var u lcl.UserMetadataAndSigchainState
		a.runCmdToJSON(t, &u, "user", "load-me")
		fqu, err := u.Fqu.StringErr()
		require.NoError(t, err)
		return a, fqu
	}
	y, yuid := mkUser()
	defer y.stop(t)
	z, zuid := mkUser()
	defer z.stop(t)

	// Raise y to the floor role and set the floor.
	x.runCmd(t, nil, "team", "add", teamName, yuid+"/o", "-r", "m/100")
	merklePoke(t)
	x.runCmd(t, nil, "team", "change-roles", teamName, "--delegation-floor", "m/100")
	merklePoke(t)

	// y, with no admin key and no bearer token, adds z as a plain member.
	y.runCmd(t, nil, "team", "add", teamName, zuid+"/o")
	merklePoke(t)

	// ...but cannot promote, and cannot touch the floor.
	err = y.runCmdErr(nil, "team", "change-roles", teamName, zuid+"->m/5")
	require.Error(t, err)
	err = y.runCmdErr(nil, "team", "change-roles", teamName, "--delegation-floor", "none")
	require.Error(t, err)

	// z really is in: they can load the team roster.
	var ros lcl.TeamRoster
	z.runCmdToJSON(t, &ros, "team", "ls", teamName)
	require.Equal(t, 3, len(ros.Members))

	// The owner turns delegation off; y is shut out again.
	x.runCmd(t, nil, "team", "change-roles", teamName, "--delegation-floor", "none")
	merklePoke(t)
	w, wuid := mkUser()
	defer w.stop(t)
	err = y.runCmdErr(nil, "team", "add", teamName, wuid+"/o")
	require.Error(t, err)
	_ = w
}
