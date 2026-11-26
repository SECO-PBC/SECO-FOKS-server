// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/keybase/clockwork"
	"github.com/stretchr/testify/require"
)

// coldListMemberships lists memberships in a fresh context over home, as the
// next process would, and returns its RPC counts by method.
func coldListMemberships(t *testing.T, tew *TestEnvWrapper, u *TestUser, home string, nTeams int,
	prep func(libclient.MetaContext)) map[string]int {
	m := tew.NewClientMetaContextAtHome(t, u, home)
	if prep != nil {
		prep(m)
	}
	tmm, err := m.G().TeamMinder()
	require.NoError(t, err)
	m, stats := m.WithRpcStats()
	lst, err := tmm.ListMemberships(m, nil)
	require.NoError(t, err)
	require.Len(t, lst.Teams, nTeams)
	return rpcCountsByMethod(stats)
}

// TestColdStartReusesSavedViewTokens: a cold start presents the team view
// tokens an earlier run minted, within the reuse window, instead of minting
// one per team (a challenge and an activation, two round trips each). Past
// the window, or once the tokens are forgotten (revoke, lock), it mints again.
func TestColdStartReusesSavedViewTokens(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	const nTeams = 2
	for range nTeams {
		tew.makeTeamForOwner(t, owner)
	}
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()
	mints := func(by map[string]int) int { return by["TeamLoader.getTeamVOBearerTokenChallenge"] }

	require.Equal(t, nTeams, mints(coldListMemberships(t, tew, owner, home, nTeams, nil)), "first run mints")

	by := coldListMemberships(t, tew, owner, home, nTeams, nil)
	require.Equal(t, nTeams, by["TeamLoader.loadTeamChain"], "the teams still load")
	require.Zero(t, mints(by), "the next run reuses what the first saved")
	require.Zero(t, by["TeamLoader.activateTeamVOBearerToken"])

	// Past the reuse window by the loader's clock, saved tokens are not used.
	by = coldListMemberships(t, tew, owner, home, nTeams, func(m libclient.MetaContext) {
		m.G().SetClock(clockwork.NewFakeClockAt(time.Now().Add(2 * time.Hour)))
	})
	require.Equal(t, nTeams, mints(by), "too old to reuse")

	// Forgotten tokens are gone.
	by = coldListMemberships(t, tew, owner, home, nTeams, func(m libclient.MetaContext) {
		require.NoError(t, libclient.ForgetSavedViewTokens(m, m.G().ActiveUser().Info.Fqu))
	})
	require.Equal(t, nTeams, mints(by), "nothing saved to reuse")
}
