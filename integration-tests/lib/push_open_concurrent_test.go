// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"sync"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// TestPushOpenDirectWithListing: the direct tap's thread read as it runs on a
// device -- alongside the Chats list's membership dump and listing, in the
// same cold process. The read must not queue behind the listing's
// exploration: it names its team by name, as the app does, resolves that
// from the persisted index, and loads its one team on its own, in one batch.
func TestPushOpenDirectWithListing(t *testing.T) {
	r := makePushOpenRig(t)
	r.sendAs(t, r.otherM, r.otherR, "the pushed message")

	base := withSimulatedLatency(r.tew.NewClientMetaContextAtHome(t, r.owner, r.home))
	tmm, err := base.G().TeamMinder()
	require.NoError(t, err)
	start := time.Now()
	var wg sync.WaitGroup
	var listDone, readDone time.Duration
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := tmm.DumpMembershipChain(base)
		require.NoError(t, err)
		_, err = tmm.ListMemberships(base, nil)
		require.NoError(t, err)
		listDone = time.Since(start)
	}()
	readBase, stats := base.WithRpcStats()
	go func() {
		defer wg.Done()
		tm := team.WrapNamed(r.named)
		m := librt.NewMetaContext(readBase)
		mr, err := librt.InitReq(m, tm)
		require.NoError(t, err)
		_, err = mr.GetThreadView(m, tm, proto.RTAppID_Chat, r.spec(), 0, 0)
		require.NoError(t, err)
		readDone = time.Since(start)
	}()
	wg.Wait()
	t.Logf("listing done %v, thread read done %v (latency %v per call); read: %d RPCs, depth %d, by method %v",
		listDone, readDone, rttTestLatency, stats.Calls(), stats.Depth(), rpcCountsByMethod(stats))
	// The client certificate (if not saved yet), then the team and the read
	// at once -- not the listing's round trips first.
	require.LessOrEqual(t, stats.Depth(), 2)
}

// TestUncheckedNameIsChecked: a name taken from the persisted index while an
// exploration runs is checked against the team once loaded. A row that now
// names another team (here planted) is not acted on: the load resolves the
// name the long way and returns the team that goes by it.
func TestUncheckedNameIsChecked(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	a := tew.makeTeamForOwner(t, owner)
	b := tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()
	coldListMemberships(t, tew, owner, home, 2, nil)

	m := tew.NewClientMetaContextAtHome(t, owner, home)
	fqtA := a.ToFQTeamParsed(t)
	hostStr, err := fqtA.Host.StringErr()
	require.NoError(t, err)
	nm, err := core.NormalizeName(a.nm)
	require.NoError(t, err)
	want := nm.String() + "@" + hostStr
	scope := owner.FQUser()
	stale := b.FQTeam(t)
	require.NoError(t, m.DbPut(libclient.DbTypeSoft, libclient.PutArg{
		Scope: &scope, Typ: lcl.DataType_TeamNameLookup, Key: "n:" + want, Val: &stale,
	}))

	tmm, err := m.G().TeamMinder()
	require.NoError(t, err)
	release := tmm.LockExploreForTest()
	byName := team.WrapNamed(proto.FQTeamParsed{Team: proto.NewParsedTeamWithTrue(a.nm), Host: fqtA.Host})
	done := make(chan *libclient.TeamWrapper, 1)
	go func() {
		m := libclient.WithReadOperationScopes(m)
		tw, err := tmm.LoadTeam(m, byName, libclient.LoadTeamOpts{Refresh: true, UnlessExplored: true})
		require.NoError(t, err)
		done <- tw
	}()
	time.Sleep(200 * time.Millisecond) // the load reaches the fallback's wait
	release()
	tw := <-done
	require.Equal(t, a.FQTeam(t), tw.Prot().Fqt, "the team that goes by the name, not the row's")
}
