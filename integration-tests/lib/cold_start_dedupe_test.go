// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"sync"
	"testing"

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

// TestLoadMeScopeEndsWithOperation: a listing loads my chain once (pinned
// in TestListMembershipsRoundTripBudget), and the next listing loads it
// again: the scope is the operation's, never carried to the next one. The
// sharing itself is unit-tested in client/libclient/load_me_scope_test.go.
func TestLoadMeScopeEndsWithOperation(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)

	cm := tew.NewClientMetaContext(t, owner)
	tmm, err := cm.G().TeamMinder()
	require.NoError(t, err)
	cm, stats := cm.WithRpcStats()
	loads := func() int { return rpcCountsByMethod(stats)["User.loadUserChain"] }

	_, err = tmm.ListMemberships(cm, nil)
	require.NoError(t, err)
	first := loads()
	require.Positive(t, first)
	_, err = tmm.ListMemberships(cm, nil)
	require.NoError(t, err)
	require.Greater(t, loads(), first, "a second listing loads my chain afresh")
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

// TestThreadViewByIDOfflineChecksListing: a head read named by id skips the
// listing only while the server answers. Served from the local cache alone
// (server unreachable), it must still find the channel in the caller's
// last-known listing, or a channel the caller lost access to would keep
// serving its cached messages.
func TestThreadViewByIDOfflineChecksListing(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.send(t, sc.alice, "before the revoke")

	// A caching minder for bob (the scene's actors cache nothing).
	bm := librt.NewMinder(sc.bob.m.G().ActiveUser())
	mb := sc.bob.m
	tm := sc.teamCfg()
	offline := &librt.MinderTestHooks{ReadsFail: simTransportErr(), ListFail: simTransportErr()}
	view := func() (*lcl.RTThreadView, error) {
		return bm.GetThreadView(mb, tm, proto.RTAppID_Chat, sc.spec(), 0, 0)
	}
	inListing := func() bool {
		lst, err := bm.ListAllChannelsForTeam(mb, tm, proto.RTAppID_Chat)
		require.NoError(t, err)
		for _, ch := range lst.Channels {
			if ch.Id.Eq(sc.chid) {
				return true
			}
		}
		return false
	}

	v, err := view()
	require.NoError(t, err)
	require.Len(t, v.Msgs, 1)
	require.True(t, inListing())

	// Offline while still a member: the cached page is served, stale.
	bm.SetTestHooks(offline)
	v, err = view()
	require.NoError(t, err)
	require.True(t, v.Stale)
	require.Len(t, v.Msgs, 1)
	bm.SetTestHooks(nil)

	// Revoked, and the listing has caught up; then offline again.
	sc.revoke(t, sc.alice, sc.bob)
	require.False(t, inListing())
	bm.SetTestHooks(offline)
	v, err = view()
	require.Nil(t, v)
	var nf core.RTNotFoundError
	require.ErrorAs(t, err, &nf, "a channel gone from the listing serves nothing from cache")
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
