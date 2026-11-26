// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"sync"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func rpcCountsByMethod(stats *core.RpcStats) map[string]int {
	by := make(map[string]int)
	for _, ms := range stats.ByMethod() {
		by[ms.Method] = ms.Calls
	}
	return by
}

// TestLoadMeScope: inside one operation's load-me scope, loads of my own chain
// are shared; outside one they are not; and forgetting the scope (after a
// change to my sigchain) sends the next load back to the server.
func TestLoadMeScope(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	cm := tew.NewClientMetaContext(t, u)
	au := cm.G().ActiveUser()
	cm, stats := cm.WithRpcStats()
	loads := func() int { return rpcCountsByMethod(stats)["User.loadUserChain"] }

	for range 2 {
		_, err := libclient.LoadMeScoped(cm, au)
		require.NoError(t, err)
	}
	require.Equal(t, 2, loads(), "no scope, no sharing")

	scoped := libclient.WithLoadMeScope(cm)
	for range 3 {
		_, err := libclient.LoadMeScoped(scoped, au)
		require.NoError(t, err)
	}
	require.Equal(t, 3, loads(), "one load for the whole scope")

	libclient.ForgetLoadMeScope(scoped)
	_, err := libclient.LoadMeScoped(scoped, au)
	require.NoError(t, err)
	require.Equal(t, 4, loads())

	// A new scope starts empty: nothing carries from one operation to the next.
	_, err = libclient.LoadMeScoped(libclient.WithLoadMeScope(cm), au)
	require.NoError(t, err)
	require.Equal(t, 5, loads())
}

// TestThreadViewByIDListsNoChannels: the head page of a channel named by id
// goes to the server without listing the team's channels first, and marks the
// page read without listing them either. A read that can be served from the
// local cache alone still lists them: there the listing is the only check
// that the caller can still see the channel.
func TestThreadViewByIDListsNoChannels(t *testing.T) {
	r := makeRTOutboxTestRig(t)
	tm := team.WrapNamedPtr(r.fqt)
	chid, err := r.minder.MakeChannel(r.mb, tm, proto.RTAppID_Chat, "byid", "", proto.RolePairOpt{})
	require.NoError(t, err)
	spc := lcl.NewRTChannelSpecifierWithId(*chid)
	_, err = r.minder.Send(r.mb, tm, proto.RTAppID_Chat, spc, []byte("hello"))
	require.NoError(t, err)

	base, stats := r.mb.Base().WithRpcStats()
	mb := librt.NewMetaContext(base)
	view, err := r.minder.GetThreadView(mb, tm, proto.RTAppID_Chat, spc, 0, 0)
	require.NoError(t, err)
	require.Len(t, view.Msgs, 1)
	by := rpcCountsByMethod(stats)
	require.Zero(t, by["RealTime.rtListAllChannelsForTeam"])
	require.Equal(t, 1, by["RealTime.rtReadThrough"], "the view still marks its page read")

	// The message is cached now; a history read of it may never reach the
	// server, so it keeps the listing.
	_, err = r.minder.GetThreadBookended(mb, tm, proto.RTAppID_Chat, spc, 1, 1)
	require.NoError(t, err)
	require.Equal(t, 1, rpcCountsByMethod(stats)["RealTime.rtListAllChannelsForTeam"])

	// An id naming no channel the caller can see is refused by the server.
	var bogus proto.RTChannelID
	require.NoError(t, core.RandomFill(bogus[:]))
	_, err = r.minder.GetThreadView(r.mb, tm, proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(bogus), 0, 0)
	require.Error(t, err)
}

// TestDumpAndListMembershipsConcurrently: the bridge may dump the membership
// chain while a listing explores. DumpMembershipChain holds exploreMu, so the
// two take turns on userTMW; run under -race to check.
func TestDumpAndListMembershipsConcurrently(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)

	mc := tew.NewClientMetaContext(t, owner)
	tmm, err := mc.G().TeamMinder()
	require.NoError(t, err)

	for range 3 {
		var wg sync.WaitGroup
		var dumpErr, listErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, dumpErr = tmm.DumpMembershipChain(mc)
		}()
		go func() {
			defer wg.Done()
			var lst *lcl.ListMembershipsRes
			lst, listErr = tmm.ListMemberships(mc, nil)
			if listErr == nil && len(lst.Teams) != 1 {
				listErr = core.InternalError("expected one team")
			}
		}()
		wg.Wait()
		require.NoError(t, dumpErr)
		require.NoError(t, listErr)
	}
}
