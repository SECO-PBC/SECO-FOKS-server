// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"context"
	"errors"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-snowpack-rpc/rpc"
)

// ── Batched loads ahead of an exploration (docs/rtt-budget.md) ──
//
// A team exploration makes its loads one after another: my chain, then my
// membership chain, then the team list, then each team's chain, then each
// team's membership chain. Each needs the previous one only to decide what to
// ask for next -- and on a restart over warm local state, the answer is
// almost always what local state already says. So before exploring, work out
// from local state alone the exact calls the exploration is going to make,
// send them in one User.sync, and park the replies in the operation's
// prefetch scope. The exploration then runs as before and its calls are
// answered from the scope; it verifies every reply as it always does.
//
// Everything here is best effort. A call planned wrong, or one the server
// refuses, just means the loader's own call misses the scope and goes to the
// network: one round trip more, never a different result.

// plannedCall is one call of a batch: what the batch carries (call) and the
// call its loader will make (wire), built from the same typed argument.
type plannedCall struct {
	call rem.SyncCall
	wire core.PlannedCall
}

// The batchable loads, each from the argument its loader builds. Each plans
// the generated client's own call, so the key a reply is kept under is the
// loader's call by construction.
var planCtx = context.Background()

func planUserChain(a rem.LoadUserChainArg) (plannedCall, error) {
	w, err := core.PlanCall(func(cli rpc.GenericClient) error {
		_, err := core.NewUserClient(cli, nil).LoadUserChain(planCtx, a)
		return err
	})
	return plannedCall{call: rem.NewSyncCallWithLoaduserchain(a), wire: w}, err
}

func planGenericChain(a rem.LoadGenericChainArg) (plannedCall, error) {
	w, err := core.PlanCall(func(cli rpc.GenericClient) error {
		_, err := core.NewUserClient(cli, nil).LoadGenericChain(planCtx, a)
		return err
	})
	return plannedCall{call: rem.NewSyncCallWithLoadgenericchain(rem.SyncLoadGenericChainArgFrom(a)), wire: w}, err
}

func planTeamListServerTrust() (plannedCall, error) {
	w, err := core.PlanCall(func(cli rpc.GenericClient) error {
		_, err := core.NewUserClient(cli, nil).GetTeamListServerTrust(planCtx)
		return err
	})
	return plannedCall{call: rem.NewSyncCallWithTeamlistservertrust(), wire: w}, err
}

func planTeamChain(a rem.LoadTeamChainArg) (plannedCall, error) {
	w, err := core.PlanCall(func(cli rpc.GenericClient) error {
		_, err := rem.TeamLoaderClient{Cli: cli, ErrorUnwrapper: core.StatusToError}.LoadTeamChain(planCtx, a)
		return err
	})
	return plannedCall{call: rem.NewSyncCallWithLoadteamchain(a), wire: w}, err
}

func planTeamMembershipChain(a rem.LoadTeamMembershipChainArg) (plannedCall, error) {
	w, err := core.PlanCall(func(cli rpc.GenericClient) error {
		_, err := rem.TeamLoaderClient{Cli: cli, ErrorUnwrapper: core.StatusToError}.LoadTeamMembershipChain(planCtx, a)
		return err
	})
	return plannedCall{call: rem.NewSyncCallWithLoadteammembershipchain(a), wire: w}, err
}

// syncMethod is User.sync, which every caller can do without: an older
// server gets the loads one at a time (core.OptionalMethod).
var syncMethod = core.MustOptionalMethodOf(func(cli rpc.GenericClient) error {
	_, err := core.NewUserClient(cli, nil).Sync(planCtx, nil)
	return err
})

// syncRefused reports whether gcli's current connection already said it has
// no User.sync.
func syncRefused(m MetaContext, gcli *core.RpcClient) (bool, error) {
	return gcli.Refused(m.Ctx(), syncMethod)
}

// prefetchForExplore sends User.sync for what an exploration of my teams is
// about to load, and stores the replies in m's prefetch scope. A no-op
// without a scope.
func (t *TeamMinder) prefetchForExplore(m MetaContext) {
	scope := core.PrefetchScopeFromContext(m.Ctx())
	if scope == nil {
		return
	}
	err := t.prefetchForExploreWithScope(m, scope)
	if err != nil {
		m.Infow("TeamMinder.prefetchForExplore", "err", err,
			"note", "falling back to one load at a time")
	}
}

func (t *TeamMinder) prefetchForExploreWithScope(m MetaContext, scope *core.PrefetchScope) error {
	au := t.au
	if au == nil {
		return core.NoActiveUserError{}
	}
	gcli, err := au.UserGCli(m)
	if err != nil {
		return err
	}
	// A server without User.sync: nothing to plan for.
	if refused, err := syncRefused(m, gcli); err != nil || refused {
		return err
	}

	var plan []plannedCall
	add := func(pc plannedCall, err error) error {
		if err != nil {
			return err
		}
		plan = append(plan, pc)
		return nil
	}

	// My own chain, as LoadMe will ask for it.
	ua, err := planLoadMe(m, au)
	if err != nil {
		return err
	}
	if err := add(planUserChain(ua)); err != nil {
		return err
	}

	// My membership chain, read as far as local state has it.
	tmlu, err := NewTMLUser(m, au.Info.Role)
	if err != nil {
		return err
	}
	tml := NewTeamMembershipLoader(tmlu)
	err = tml.gcl.loadExistingChain(m)
	if err != nil {
		return err
	}
	if err := add(planGenericChain(tml.gcl.serverArg())); err != nil {
		return err
	}
	if err := add(planTeamListServerTrust()); err != nil {
		return err
	}

	// The teams I'm directly in, by my membership chain as stored locally.
	// Teams reached through other teams, and teams new since the last run,
	// are left to the exploration's own calls.
	puks, err := localPUKSet(m, au)
	if err != nil {
		m.Infow("TeamMinder.prefetchForExplore", "stage", "puks", "skipTeams", err)
	}
	if puks != nil {
		local := &TeamMembershipLoaderAndWrapper{
			Loader:  tml,
			Wrapper: &TeamMembershipWrapper{Map: tml.data, RoleInChain: tmlu.RoleInChain()},
		}
		nodes, err := local.explore(nil)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			tc, err := planTeamLoad(m, au, puks, node)
			if err != nil {
				m.Infow("TeamMinder.prefetchForExplore", "team", node.Fqt, "skip", err)
				continue
			}
			plan = append(plan, tc...)
		}
	}

	return sendPlan(m, scope, gcli, plan)
}

// prefetchForTeam sends one User.sync for what loading fqt on its own
// (directLoadTeam) is about to ask for -- my chain and the team's chain --
// and stores the replies in m's prefetch scope. A no-op without a scope.
func (t *TeamMinder) prefetchForTeam(m MetaContext, fqt proto.FQTeam) {
	scope := core.PrefetchScopeFromContext(m.Ctx())
	if scope == nil {
		return
	}
	err := t.prefetchForTeamWithScope(m, scope, fqt)
	if err != nil {
		m.Infow("TeamMinder.prefetchForTeam", "err", err, "note", "loading it one call at a time")
	}
}

func (t *TeamMinder) prefetchForTeamWithScope(m MetaContext, scope *core.PrefetchScope, fqt proto.FQTeam) error {
	au := t.au
	if au == nil {
		return core.NoActiveUserError{}
	}
	gcli, err := au.UserGCli(m)
	if err != nil {
		return err
	}
	if refused, err := syncRefused(m, gcli); err != nil || refused {
		return err
	}
	ua, err := planLoadMe(m, au)
	if err != nil {
		return err
	}
	me, err := planUserChain(ua)
	if err != nil {
		return err
	}
	plan := []plannedCall{me}
	puks, err := localPUKSet(m, au)
	if err != nil {
		return err
	}
	l, err := planTeamLoader(m, au, t.directTeamArg(fqt, puks))
	if err != nil {
		return err
	}
	tc, err := planTeamChain(l.serverArg())
	if err != nil {
		return err
	}
	return sendPlan(m, scope, gcli, append(plan, tc))
}

// sendPlan sends the plan as User.sync batches of at most rem.MaxSyncCalls
// calls, all at once, and keeps each reply under its call.
func sendPlan(m MetaContext, scope *core.PrefetchScope, gcli *core.RpcClient, plan []plannedCall) error {
	ucli := core.NewUserClient(gcli, m)
	errs := make(chan error, len(plan)/rem.MaxSyncCalls+1)
	nBatches := 0
	for lo := 0; lo < len(plan); lo += rem.MaxSyncCalls {
		batch := plan[lo:min(lo+rem.MaxSyncCalls, len(plan))]
		nBatches++
		go func() { errs <- sendBatch(m, scope, gcli, ucli, batch) }()
	}
	var first error
	for range nBatches {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func sendBatch(m MetaContext, scope *core.PrefetchScope, gcli *core.RpcClient, ucli rem.UserClient, batch []plannedCall) error {
	calls := make([]rem.SyncCall, len(batch))
	for i, p := range batch {
		calls[i] = p.call
	}
	res, err := ucli.Sync(core.WithOptionalMethod(m.Ctx(), syncMethod), calls)
	if err != nil {
		return err
	}
	if len(res) != len(batch) {
		return core.InternalError("sync: reply count does not match the batch")
	}
	for i, r := range res {
		ok, err := r.GetOk()
		if err != nil {
			return err
		}
		if !ok {
			// That load goes out on its own when its loader makes it.
			m.Debugw("TeamMinder.prefetchForExplore", "call", batch[i].wire.Method(),
				"err", core.StatusToError(r.False()))
			continue
		}
		reply := r.True()
		want, err := batch[i].call.GetT()
		if err != nil {
			return err
		}
		got, err := reply.GetT()
		if err != nil {
			return err
		}
		if got != want {
			return core.InternalError("sync: a reply's type does not match its call")
		}
		replyW, err := syncReplyWire(reply)
		if err != nil {
			return err
		}
		err = scope.Put(gcli, batch[i].wire, replyW)
		if err != nil {
			return err
		}
	}
	return nil
}

// syncReplyWire is the wire form of a batched reply, as its standalone call
// would have received it.
func syncReplyWire(r rem.SyncReply) (any, error) {
	typ, err := r.GetT()
	if err != nil {
		return nil, err
	}
	switch typ {
	case rem.SyncCallType_LoadUserChain:
		return r.Loaduserchain().Export(), nil
	case rem.SyncCallType_LoadGenericChain:
		return r.Loadgenericchain().Export(), nil
	case rem.SyncCallType_TeamListServerTrust:
		return r.Teamlistservertrust().Export(), nil
	case rem.SyncCallType_LoadTeamChain:
		return r.Loadteamchain().Export(), nil
	case rem.SyncCallType_LoadTeamMembershipChain:
		return r.Loadteammembershipchain().Export(), nil
	default:
		return nil, core.InternalError("sync: unknown reply type")
	}
}

// localPUKSet is my PUK set as an exploration will use it, from local state
// only: the one in memory if this process has it, else the last verified
// snapshot of my chain and the PUK box cached for this device. A rotation
// since then leaves it a generation behind; the team loads planned with it
// then miss the batch and run as usual.
func localPUKSet(m MetaContext, au *UserContext) (*PUKSet, error) {
	if ps := au.PrivKeys.GetPUKSet(); ps != nil {
		return ps, nil
	}
	uw, err := LoadMeFromCache(m, au)
	if err != nil {
		return nil, err
	}
	gen, err := uw.LatestPUKGenForRole(au.Role())
	if err != nil {
		return nil, err
	}
	devkey, err := au.Devkey(m.Ctx())
	if err != nil {
		return nil, err
	}
	eid, err := devkey.EntityID()
	if err != nil {
		return nil, err
	}
	// Checking the box needs my chain; hand it the snapshot, or it would
	// load mine from the server.
	pm := NewPUKMinder(au).SetUser(uw)
	raw, err := pm.getPUKParcelForRoleFromDB(m, au.Role(), eid)
	if err != nil {
		return nil, err
	}
	if raw == nil || raw.Box.Gen < gen {
		return nil, core.KeyNotFoundError{Which: "cached PUK"}
	}
	return pm.decryptPUKParcel(m, raw)
}

// planLoadMe is the loadUserChain call LoadMe will make for au.
func planLoadMe(m MetaContext, au *UserContext) (rem.LoadUserChainArg, error) {
	l := NewUserLoader(LoadUserArg{
		Uid:        au.Info.Fqu.Uid,
		LoadMode:   LoadModeSelf,
		ActiveUser: au,
	})
	l.resetState()
	err := l.checkArgs(m)
	if err != nil {
		return rem.LoadUserChainArg{}, err
	}
	// For the active user this is the home server it already holds, and the
	// user client the caller just made: no round trip.
	err = l.connectHost(m)
	if err != nil {
		return rem.LoadUserChainArg{}, err
	}
	err = l.loadExistingUser(m)
	if err != nil {
		return rem.LoadUserChainArg{}, err
	}
	return l.serverArg()
}

// errNoSavedToken: the team has no reusable saved view token, so loading it
// starts with minting one, which a batch can't do ahead of time.
var errNoSavedToken = errors.New("no reusable saved view token")

// planTeamLoader is a TeamLoader for arg run up to its server call, from
// local state alone: what it holds, and the saved view token it presents.
// errNoSavedToken when it would have to mint one first.
func planTeamLoader(m MetaContext, au *UserContext, arg LoadTeamArg) (*TeamLoader, error) {
	l := NewTeamLoader(au, arg)
	err := l.checkArg(m)
	if err != nil {
		return nil, err
	}
	err = l.loadExistingTeam(m)
	if err != nil {
		return nil, err
	}
	if !l.reuseViewToken(m) || l.voTok == nil {
		return nil, errNoSavedToken
	}
	return l, nil
}

// planTeamLoad is the loadTeamChain and loadTeamMembershipChain calls an
// exploration makes for a team I'm directly in, as TeamMinder.explore loads
// it (loadTeamArg, then NewTMLTeamFromLoaded).
func planTeamLoad(m MetaContext, au *UserContext, puks *PUKSet, node ExploreNode) ([]plannedCall, error) {
	if !node.Fqt.Host.Eq(au.HostID()) {
		return nil, core.InternalError("not on my home server")
	}
	arg := LoadTeamArg{
		Team:         node.Fqt,
		As:           au.FQU().FQParty(),
		SrcRole:      team.UserSrcRole,
		Keys:         puks,
		KeyRefresher: au.KeyRefresher,
	}
	if node.Fqt.Team.IsAdHocTeam() {
		arg.MemberLoad = MemberLoadNames
	}
	l, err := planTeamLoader(m, au, arg)
	if err != nil {
		return nil, err
	}
	ta, err := planTeamChain(l.serverArg())
	if err != nil {
		return nil, err
	}

	// The membership chain, as the exploration's TMLTeam fetches it: with
	// the same token, from where local state leaves off.
	tmlt := &TMLTeam{au: au, arg: arg, tmLoader: l}
	tml := NewTeamMembershipLoader(tmlt)
	err = tml.gcl.loadExistingChain(m)
	if err != nil {
		return nil, err
	}
	ma, err := tmlt.fetchArg(tml.gcl.serverArg())
	if err != nil {
		return nil, err
	}
	mc, err := planTeamMembershipChain(ma)
	if err != nil {
		return nil, err
	}
	return []plannedCall{ta, mc}, nil
}
