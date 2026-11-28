// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"errors"
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
	// The read starts once the listing holds the exploration lock, as on a
	// device where the Chats list got there first; otherwise the read may
	// simply run first and explore itself.
	listingRunning := make(chan struct{})
	var once sync.Once
	tmm.TestHooks = &libclient.TeamMinderTestHooks{
		LoadUnderExploreMu: func() { once.Do(func() { close(listingRunning) }) },
	}
	t.Cleanup(func() { tmm.TestHooks = nil })
	start := time.Now()
	var wg sync.WaitGroup
	var listDone, readDone time.Duration
	listErr := make(chan error, 1)
	readErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		listErr <- func() error {
			if _, err := tmm.DumpMembershipChain(base); err != nil {
				return err
			}
			_, err := tmm.ListMemberships(base, nil)
			listDone = time.Since(start)
			return err
		}()
	}()
	readBase, stats := base.WithRpcStats()
	go func() {
		defer wg.Done()
		select {
		case <-listingRunning:
		case <-time.After(time.Minute):
			readErr <- errors.New("the listing never took the exploration lock")
			return
		}
		readErr <- func() error {
			tm := team.WrapNamed(r.named)
			m := librt.NewMetaContext(readBase)
			mr, err := librt.InitReq(m, tm)
			if err != nil {
				return err
			}
			_, err = mr.GetThreadView(m, tm, proto.RTAppID_Chat, r.spec(), 0, 0)
			readDone = time.Since(start)
			return err
		}()
	}()
	wg.Wait()
	require.NoError(t, <-listErr, "listing")
	require.NoError(t, <-readErr, "thread read")
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
	type indexOutcome struct {
		fqt  proto.FQTeam
		used bool
	}
	outcomes := make(chan indexOutcome, 4)
	tmm.TestHooks = &libclient.TeamMinderTestHooks{
		IndexResolution: func(fqt proto.FQTeam, used bool) {
			outcomes <- indexOutcome{fqt: fqt, used: used}
		},
	}
	t.Cleanup(func() { tmm.TestHooks = nil })
	release := tmm.LockExploreForTest()
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	byName := team.WrapNamed(proto.FQTeamParsed{Team: proto.NewParsedTeamWithTrue(a.nm), Host: fqtA.Host})
	type loadRes struct {
		tw  *libclient.TeamWrapper
		err error
	}
	done := make(chan loadRes, 1)
	go func() {
		m := libclient.WithReadOperationScopes(m)
		tw, err := tmm.LoadTeam(m, byName, libclient.LoadTeamOpts{Refresh: true, UnlessExplored: true})
		done <- loadRes{tw, err}
	}()
	// The unchecked path ran -- on the row's team -- and was refused; the
	// load now waits for the exploration to resolve the name the long way.
	select {
	case o := <-outcomes:
		require.Equal(t, stale, o.fqt, "the index answer is the planted row's team")
		require.False(t, o.used, "a row naming another team is not acted on")
	case res := <-done:
		t.Fatalf("LoadTeam returned before trying the index: %v", res.err)
	case <-time.After(time.Minute):
		t.Fatal("LoadTeam never tried the persisted index")
	}
	released = true
	release()
	res := <-done
	require.NoError(t, res.err)
	require.Equal(t, a.FQTeam(t), res.tw.Prot().Fqt, "the team that goes by the name, not the row's")
}

// TestUnloadedIndexNameWaitsForExploration: LoadTeam without Refresh, while
// an exploration runs, for a team the persisted index names but no
// exploration has loaded yet. The index answer can't be served (nothing is
// loaded to serve), so LoadTeam waits for the exploration, as it did before
// the shortcut, rather than reporting the team not found.
func TestUnloadedIndexNameWaitsForExploration(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	a := tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()
	coldListMemberships(t, tew, owner, home, 1, nil)

	m := tew.NewClientMetaContextAtHome(t, owner, home)
	fqtA := a.ToFQTeamParsed(t)
	tmm, err := m.G().TeamMinder()
	require.NoError(t, err)
	outcomes := make(chan bool, 4)
	tmm.TestHooks = &libclient.TeamMinderTestHooks{
		IndexResolution: func(_ proto.FQTeam, used bool) { outcomes <- used },
	}
	t.Cleanup(func() { tmm.TestHooks = nil })
	release := tmm.LockExploreForTest()
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	byName := team.WrapNamed(proto.FQTeamParsed{Team: proto.NewParsedTeamWithTrue(a.nm), Host: fqtA.Host})
	type loadRes struct {
		tw  *libclient.TeamWrapper
		err error
	}
	done := make(chan loadRes, 1)
	go func() {
		tw, err := tmm.LoadTeam(m, byName, libclient.LoadTeamOpts{})
		done <- loadRes{tw, err}
	}()
	select {
	case used := <-outcomes:
		require.False(t, used, "nothing loaded yet to serve")
	case res := <-done:
		t.Fatalf("LoadTeam returned before trying the index: %v", res.err)
	case <-time.After(time.Minute):
		t.Fatal("LoadTeam never tried the persisted index")
	}
	released = true
	release()
	res := <-done
	require.NoError(t, res.err)
	require.Equal(t, a.FQTeam(t), res.tw.Prot().Fqt)
}

// TestAdHocLoadedAloneCarriesNames: an ad-hoc (DM) team the party cache
// loads on its own while an exploration runs (loadOnlyThisTeam) is loaded
// with member names, as an exploration loads it -- such a team is named by
// its member list -- and the same operation's refresh, skipped because the
// team was just loaded, never hands back a record without them. The
// teamname cache is filled either way.
func TestAdHocLoadedAloneCarriesNames(t *testing.T) {
	f := createAdHocTeamForTest(t, 1)
	fqt := proto.FQTeam{Host: f.vhost.HostID.Id, Team: f.tid}

	// A minder with nothing loaded, as in a cold process.
	tmm := libclient.NewTeamMinder(f.ma.G().ActiveUser())
	release := tmm.LockExploreForTest()
	defer release()

	m := libclient.WithReadOperationScopes(f.ma)
	opts := libclient.LoadTeamOpts{Refresh: true, UnlessExplored: true}
	for i := range 2 {
		tr, err := tmm.LoadTeamWithFQTeam(m, fqt, opts)
		require.NoError(t, err)
		require.GreaterOrEqual(t, tr.LoadArg().MemberLoad, libclient.MemberLoadNames, "load %d", i)
		nm, err := tr.Tw().AdHocDisplayName()
		require.NoError(t, err, "load %d", i)
		require.NotEmpty(t, nm)
		cached, ok := m.G().TeamnameLoader().Get(m, fqt)
		require.True(t, ok, "load %d: teamname cache filled", i)
		require.Equal(t, nm, cached)
	}
}
