// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"context"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-snowpack-rpc/rpc"
	"github.com/stretchr/testify/require"
)

// rttTestLatency is added to every RPC in a depth measurement, so calls the
// client issues together overlap even on loopback and Depth counts what a
// real link would make the user wait for.
const rttTestLatency = 20 * time.Millisecond

func withSimulatedLatency(m libclient.MetaContext) libclient.MetaContext {
	ret := m
	ret.MetaContext = m.MetaContext.WithContext(core.WithSimulatedRpcLatency(m.Ctx(), rttTestLatency))
	return ret
}

// TestColdListMembershipsDepth: listing memberships on a restart over warm
// local state -- the user and team half of opening the app from a
// notification -- in sequential round trips.
func TestColdListMembershipsDepth(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	const nTeams = 3
	for range nTeams {
		tew.makeTeamForOwner(t, owner)
	}
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()

	// First run warms the local state (chains, saved view tokens).
	coldListMemberships(t, tew, owner, home, nTeams, nil)

	m := tew.NewClientMetaContextAtHome(t, owner, home)
	tmm, err := m.G().TeamMinder()
	require.NoError(t, err)
	m, stats := withSimulatedLatency(m).WithRpcStats()
	m = libclient.WithPrefetchScope(m)
	scope := core.PrefetchScopeFromContext(m.Ctx())
	lst, err := tmm.ListMemberships(m, nil)
	require.NoError(t, err)
	require.Len(t, lst.Teams, nTeams)

	by := rpcCountsByMethod(stats)
	t.Logf("cold ListMemberships: %d RPCs, depth %d, %d served from sync, unused %v, by method %v",
		stats.Calls(), stats.Depth(), scope.Hits(), scope.PendingMethods(), by)

	// One batch carries every load: my chain, my membership chain, the
	// team list, and a chain and membership chain per team.
	require.Equal(t, 1, by["User.sync"])
	require.Equal(t, 3+2*nTeams, scope.Hits())
	require.Zero(t, scope.Pending(), "every reply fetched ahead was used")
	for _, meth := range []string{
		"User.loadUserChain", "User.loadGenericChain", "User.getTeamListServerTrust",
		"TeamLoader.loadTeamChain", "TeamLoader.loadTeamMembershipChain",
	} {
		require.Zero(t, by[meth], meth+" went to the network")
	}

	// The client certificate (if not saved yet), then the batch.
	require.LessOrEqual(t, stats.Depth(), 2)
}

// TestColdListMembershipsNewTeam: a team joined since the last run isn't in
// the batch -- local state doesn't know it yet -- so it loads the usual way,
// and the listing is still right.
func TestColdListMembershipsNewTeam(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()
	coldListMemberships(t, tew, owner, home, 1, nil)

	tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)

	net, served := coldListMembershipsSplit(t, tew, owner, home, 2, nil)
	require.Equal(t, 1, net["User.sync"])
	require.Equal(t, 1, served["TeamLoader.loadTeamChain"], "the known team comes in the batch")
	require.Equal(t, 1, net["TeamLoader.loadTeamChain"], "only the new team loads on its own")
}

// coldThreadViewRig: an owner and a second member in a team with a channel
// both have written in, and a home directory where the owner's earlier run
// already opened the thread -- so a new process over it is a restart over
// warm local state.
type coldThreadViewRig struct {
	tew   *TestEnvWrapper
	owner *TestUser
	other *TestUser
	fqt   *proto.FQTeamParsed
	// named is the team by name, as the SECO app names it to the agent
	// (name@host), which needs the team index to resolve.
	named  proto.FQTeamParsed
	chid   proto.RTChannelID
	home   string
	otherM librt.MetaContext
	otherR *librt.Minder
}

func makeColdThreadViewRig(t *testing.T) *coldThreadViewRig {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	other := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	tm.makeChanges(t, tew.MetaContext(), owner,
		[]proto.MemberRole{other.toMemberRole(t, proto.AdminRole, tm.hepks)}, nil)
	// As joining would: the other member lets the team view their chain, so
	// co-members can name them.
	_, err := tew.userCli(t, other).GrantLocalViewPermissionForUser(tew.MetaContext().Ctx(),
		rem.GrantLocalViewPermissionPayload{
			Viewee: other.uid.ToPartyID(),
			Viewer: tm.FQTeam(t).Team.ToPartyID(),
		})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	fqt := tm.ToFQTeamParsed(t)
	home := t.TempDir()

	om := librt.NewMetaContext(tew.NewClientMetaContextAtHome(t, owner, home))
	or := librt.NewMinder(om.G().ActiveUser())
	chid, err := or.MakeChannel(om, team.WrapNamedPtr(fqt), proto.RTAppID_Chat, "lounge", "",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole})
	require.NoError(t, err)

	xm := librt.NewMetaContext(tew.NewClientMetaContextWithEracer(t, other))
	xr := librt.NewMinder(xm.G().ActiveUser())
	r := &coldThreadViewRig{tew: tew, owner: owner, other: other, fqt: fqt, chid: *chid,
		home: home, otherM: xm, otherR: xr,
		named: proto.FQTeamParsed{Team: proto.NewParsedTeamWithTrue(tm.nm), Host: fqt.Host}}

	r.sendAs(t, xm, xr, "hi from other")
	view, err := or.GetThreadView(om, team.WrapNamedPtr(fqt), proto.RTAppID_Chat, r.spec(), 0, 0)
	require.NoError(t, err)
	require.Len(t, view.Msgs, 1)
	r.sendAs(t, om, or, "hi from owner")
	return r
}

func (r *coldThreadViewRig) spec() lcl.RTChannelSpecifier {
	return lcl.NewRTChannelSpecifierWithId(r.chid)
}

func (r *coldThreadViewRig) sendAs(t *testing.T, m librt.MetaContext, mr *librt.Minder, body string) {
	_, err := mr.Send(m, team.WrapNamedPtr(r.fqt), proto.RTAppID_Chat, r.spec(), []byte(body))
	require.NoError(t, err)
}

// coldOpen opens the thread's head page in a new owner process over the warm
// home, with simulated latency, and returns the view, the call stats, and the
// process (to open it again warm).
func (r *coldThreadViewRig) coldOpen(t *testing.T) (*lcl.RTThreadView, *core.RpcStats, *warmProcess) {
	base := r.tew.NewClientMetaContextAtHome(t, r.owner, r.home)
	p := &warmProcess{r: r, base: base, mr: librt.NewMinder(base.G().ActiveUser())}
	view, stats := p.open(t)
	return view, stats, p
}

// warmProcess is an owner process that has opened the thread already.
type warmProcess struct {
	r    *coldThreadViewRig
	base libclient.MetaContext
	mr   *librt.Minder
}

func (p *warmProcess) open(t *testing.T) (*lcl.RTThreadView, *core.RpcStats) {
	base, stats := withSimulatedLatency(p.base).WithRpcStats()
	view, err := p.mr.GetThreadView(librt.NewMetaContext(base), team.WrapNamedPtr(p.r.fqt),
		proto.RTAppID_Chat, p.r.spec(), 0, 0)
	require.NoError(t, err)
	return view, stats
}

// TestColdThreadViewDepth: opening a thread by id after a restart -- the
// RealTime half of opening the app from a notification.
func TestColdThreadViewDepth(t *testing.T) {
	r := makeColdThreadViewRig(t)
	r.sendAs(t, r.otherM, r.otherR, "new one")
	r.sendAs(t, r.otherM, r.otherR, "and another")

	view, stats, p := r.coldOpen(t)
	require.Len(t, view.Msgs, 4)
	require.Equal(t, "and another", string(view.Msgs[0].Body))
	by := rpcCountsByMethod(stats)
	t.Logf("cold GetThreadView: %d RPCs, depth %d, by method %v",
		stats.Calls(), stats.Depth(), by)
	for _, mv := range view.Msgs {
		require.NotNil(t, mv.SenderName, "sender names resolve")
	}

	// The team (my chain, my teams, this team) comes in one User.sync, run
	// alongside the read. The page is marked once it has been built and
	// shown, outside the view's round trips (markLater).
	require.Equal(t, 1, by["User.sync"])
	require.Equal(t, 1, by["RealTime.rtGetThreadRecents"])
	require.Zero(t, by["RealTime.rtReadThrough"], "the mark doesn't hold up the view")
	require.Zero(t, by["TeamLoader.loadTeamChain"], "no refresh of a team the exploration just loaded")

	// The client certificate (if not saved yet), then the two in parallel.
	require.LessOrEqual(t, stats.Depth(), 2)

	p.mr.WaitLaterMarks()
	require.Equal(t, proto.RTMsgSeq(4), r.ownerReadThrough(t))

	// Open again in the same process: the team is loaded, so the read is the
	// view's one round trip; the mark follows once the page is shown.
	r.sendAs(t, r.otherM, r.otherR, "one more")
	view, stats = p.open(t)
	require.Len(t, view.Msgs, 5)
	by = rpcCountsByMethod(stats)
	require.Equal(t, 1, by["RealTime.rtGetThreadRecents"])
	require.Zero(t, by["RealTime.rtReadThrough"])
	require.Equal(t, 1, stats.Depth())
	p.mr.WaitLaterMarks()
	require.Equal(t, proto.RTMsgSeq(5), r.ownerReadThrough(t))
}

// TestThreadViewFailedPageNotMarked: a head page that fails the client's
// checks is never marked read -- the mark goes out only once the page has
// been built and verified, so a view that failed leaves the unread state as
// it was (on this device's inbox and the user's others).
func TestThreadViewFailedPageNotMarked(t *testing.T) {
	r := makeColdThreadViewRig(t)
	_, _, p := r.coldOpen(t) // loads the team, as a warm process has it
	p.mr.WaitLaterMarks()
	require.Equal(t, proto.RTMsgSeq(2), r.ownerReadThrough(t))

	r.sendAs(t, r.otherM, r.otherR, "garbled in transit")
	p.mr.SetTestHooks(&librt.MinderTestHooks{
		MutateRecentsRes: func(l *rem.RTMsgList) {
			for i := range l.Lst {
				l.Lst[i].Mw = proto.RTMsgWrapper{}
			}
		},
	})
	base, _ := withSimulatedLatency(p.base).WithRpcStats()
	_, err := p.mr.GetThreadView(librt.NewMetaContext(base), team.WrapNamedPtr(r.fqt),
		proto.RTAppID_Chat, r.spec(), 0, 0)
	require.Error(t, err)
	p.mr.WaitLaterMarks()
	require.Equal(t, proto.RTMsgSeq(2), r.ownerReadThrough(t), "nothing shown, nothing marked")
}

// ownerReadThrough is the owner's read pointer in the rig's channel, from a
// fresh client's inbox sync.
func (r *coldThreadViewRig) ownerReadThrough(t *testing.T) proto.RTMsgSeq {
	m := librt.NewMetaContext(r.tew.NewClientMetaContextWithEracer(t, r.owner))
	mr := librt.NewMinder(m.G().ActiveUser())
	delta, err := mr.GetChangedThreads(m, proto.RTAppID_Chat, 0, 0)
	require.NoError(t, err)
	for _, ch := range delta.Channels {
		if ch.Md.Id == r.chid {
			return ch.ReadThrough
		}
	}
	require.Fail(t, "channel not in the owner's inbox")
	return 0
}

// TestColdThreadViewNothingNew: with nothing new since the last run, the
// page is the cached tail and the mark covers it.
func TestColdThreadViewNothingNew(t *testing.T) {
	r := makeColdThreadViewRig(t)
	view, _, p := r.coldOpen(t)
	require.Len(t, view.Msgs, 2)
	p.mr.WaitLaterMarks()
	require.Equal(t, proto.RTMsgSeq(2), r.ownerReadThrough(t))
}

// TestThreadHistoryPageReadsOldWay: a page back in history is not the
// head, so it reads the old way and is marked by the separate call.
func TestThreadHistoryPageReadsOldWay(t *testing.T) {
	r := makeColdThreadViewRig(t)
	base := r.tew.NewClientMetaContextAtHome(t, r.owner, r.home)
	mr := librt.NewMinder(base.G().ActiveUser())
	base, stats := base.WithRpcStats()
	_, err := mr.GetThreadView(librt.NewMetaContext(base), team.WrapNamedPtr(r.fqt),
		proto.RTAppID_Chat, r.spec(), 2, 0)
	require.NoError(t, err)
	require.Equal(t, 1, rpcCountsByMethod(stats)["RealTime.rtReadThrough"],
		"marked in the view, as before")
}

// TestColdKVGetFileDepth: reading a team file after a restart over warm
// local state (KV caches, team state, saved view token).
func TestColdKVGetFileDepth(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()

	cfg := lcl.KVConfig{
		ActingAs: team.WrapNamed(proto.FQTeamParsed{
			Team: proto.NewParsedTeamWithFalse(tm.FQTeam(t).Team),
		}),
		Roles: proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	}
	putCfg := cfg
	putCfg.MkdirP = true
	const path = proto.KVPath("/profiles/owner/card.json")

	m := libkv.NewMetaContext(tew.NewClientMetaContextAtHome(t, owner, home))
	kvm := libkv.NewMinder(m.G().ActiveUser())
	want := writeRandomFileWithConfig(t, kvm, m, path, 100, putCfg, 0)
	got, err := kvm.GetFile(m, cfg, path)
	require.NoError(t, err)
	require.Equal(t, want, []byte(got.Chunk.Chunk))

	base := tew.NewClientMetaContextAtHome(t, owner, home)
	kvm = libkv.NewMinder(base.G().ActiveUser())
	base, stats := withSimulatedLatency(base).WithRpcStats()
	got, err = kvm.GetFile(libkv.NewMetaContext(base), cfg, path)
	require.NoError(t, err)
	require.Equal(t, want, []byte(got.Chunk.Chunk))

	by := rpcCountsByMethod(stats)
	t.Logf("cold KV GetFile: %d RPCs, depth %d, by method %v", stats.Calls(), stats.Depth(), by)

	// The team comes in one User.sync; then the cache check, which can't go
	// earlier: it is built by walking the cache, and the walk needs the
	// team's keys to name each path segment.
	require.Equal(t, 1, by["User.sync"])
	require.Equal(t, 1, by["KVStore.kvCacheCheck"])
	require.Zero(t, by["TeamLoader.loadTeamChain"])
	require.Zero(t, by["User.loadUserChain"])

	// Client certificate (if not saved yet), the team, the cache check.
	require.LessOrEqual(t, stats.Depth(), 3)

	// The app's order on a cold start: list memberships (which explores and
	// loads every team), then read the team's files. The read is a separate
	// operation; measure what it costs after the listing.
	base = tew.NewClientMetaContextAtHome(t, owner, home)
	tmm, err := base.G().TeamMinder()
	require.NoError(t, err)
	_, err = tmm.ListMemberships(base, nil)
	require.NoError(t, err)
	kvm = libkv.NewMinder(base.G().ActiveUser())
	base, stats = withSimulatedLatency(base).WithRpcStats()
	got, err = kvm.GetFile(libkv.NewMetaContext(base), cfg, path)
	require.NoError(t, err)
	require.Equal(t, want, []byte(got.Chunk.Chunk))
	t.Logf("KV GetFile after ListMemberships: %d RPCs, depth %d, by method %v",
		stats.Calls(), stats.Depth(), rpcCountsByMethod(stats))
}

// withOlderServer makes the context's calls to the given methods fail as a
// server from before them would.
func withOlderServer(m libclient.MetaContext, methods ...string) libclient.MetaContext {
	ret := m
	ret.MetaContext = m.MetaContext.WithContext(core.WithSimulatedMissingMethods(m.Ctx(), methods...))
	return ret
}

// TestListMembershipsOlderServer: a server without User.sync still lists
// right, one load at a time; and the refusal is remembered for the
// connection, so the next listing over it doesn't ask again -- but a new
// connection (which may reach an upgraded server) does.
func TestListMembershipsOlderServer(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	const nTeams = 2
	for range nTeams {
		tew.makeTeamForOwner(t, owner)
	}
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()
	coldListMemberships(t, tew, owner, home, nTeams, nil)

	m := withOlderServer(tew.NewClientMetaContextAtHome(t, owner, home), "User.sync")
	tmm, err := m.G().TeamMinder()
	require.NoError(t, err)
	for i := range 2 {
		mi, stats := m.WithRpcStats()
		lst, err := tmm.ListMemberships(mi, nil)
		require.NoError(t, err)
		require.Len(t, lst.Teams, nTeams)
		by := rpcCountsByMethod(stats)
		require.Equal(t, 1-i, by["User.sync"], "asked once, then remembered")
		require.Equal(t, 1, by["User.loadUserChain"], "loads go out on their own")
	}

	gcli, err := m.G().ActiveUser().UserGCli(m)
	require.NoError(t, err)
	gcli.Reset()
	mi, stats := m.WithRpcStats()
	_, err = tmm.ListMemberships(mi, nil)
	require.NoError(t, err)
	require.Equal(t, 1, rpcCountsByMethod(stats)["User.sync"], "a new connection asks again")
}

// TestBatchedReplyThatFailsFallsBack: a reply fetched ahead that fails the
// loader's checks (here: an empty chain where the chain has links) is not
// the end of the load. The loader runs again, the batch's reply is used up,
// and the network answers.
func TestBatchedReplyThatFailsFallsBack(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	m := tew.NewClientMetaContext(t, u)
	au := m.G().ActiveUser()
	gcli, err := au.UserGCli(m)
	require.NoError(t, err)

	m, stats := m.WithRpcStats()
	m = libclient.WithPrefetchScope(m)
	scope := core.PrefetchScopeFromContext(m.Ctx())
	// LoadMe's call on a fresh home: the whole chain, as the local user.
	pc, err := core.PlanCall(func(cli rpc.GenericClient) error {
		_, err := core.NewUserClient(cli, nil).LoadUserChain(context.Background(), rem.LoadUserChainArg{
			Uid:   u.uid,
			Start: proto.ChainEldestSeqno,
			Auth:  rem.NewLoadUserChainAuthWithAslocaluser(),
		})
		return err
	})
	require.NoError(t, err)
	require.NoError(t, scope.Put(gcli, pc, rem.UserChain{}.Export()))

	uw, err := libclient.LoadMe(m, au)
	require.NoError(t, err)
	require.NotNil(t, uw)
	require.Equal(t, 1, scope.Hits(), "the planted reply was used")
	require.Equal(t, 1, rpcCountsByMethod(stats)["User.loadUserChain"], "then the network answered")
}

// TestExploredTeamRefresh: in one operation, a team its exploration just
// loaded is not reloaded by the party cache's ordinary refresh, but a forced
// refresh (a stale token, new keys) still reloads it.
func TestExploredTeamRefresh(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	tew.DirectDoubleMerklePokeInTest(t)
	cfg := team.WrapNamed(*tm.ToFQTeamParsed(t))

	m := tew.NewClientMetaContext(t, owner)
	tmm, err := m.G().TeamMinder()
	require.NoError(t, err)
	m, stats := m.WithRpcStats()
	m = libclient.WithReadOperationScopes(m)
	loads := func() int { return rpcCountsByMethod(stats)["TeamLoader.loadTeamChain"] }

	// Resolving the team explores; the cache's refresh right after is skipped.
	_, err = tmm.LoadTeam(m, cfg, libclient.LoadTeamOpts{Refresh: true, UnlessExplored: true})
	require.NoError(t, err)
	before := loads()

	// A forced refresh in the same operation reloads.
	_, err = tmm.LoadTeam(m, cfg, libclient.LoadTeamOpts{Refresh: true})
	require.NoError(t, err)
	require.Equal(t, before+1, loads())
}
