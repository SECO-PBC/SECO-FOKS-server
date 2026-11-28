// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"context"
	"sync"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
)

const (
	// syncParallelism caps how many of a batch's calls run at once, and so
	// how many database connections one client's batch can hold.
	syncParallelism = 8
)

// Sync runs a batch of this server's read-only loads in one round trip.
//
// Each call goes to the handler the client would otherwise have called on
// its own, on this same connection, so authorization and the shape of every
// reply are exactly those of the standalone RPC -- the client verifies the
// replies with the code it already has. The batch adds no new way to read
// anything. A call that fails reports its own error; the others still answer.
func (c *UserClientConn) Sync(ctx context.Context, calls []rem.SyncCall) ([]rem.SyncResult, error) {
	if len(calls) > rem.MaxSyncCalls {
		return nil, core.BadArgsError("sync: too many calls")
	}
	ret := make([]rem.SyncResult, len(calls))
	sem := make(chan struct{}, syncParallelism)
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				ret[i] = rem.NewSyncResultWithFalse(core.ErrorToStatus(ctx.Err()))
				return
			}
			reply, err := c.syncOne(ctx, call)
			if err != nil {
				ret[i] = rem.NewSyncResultWithFalse(core.ErrorToStatus(err))
				return
			}
			ret[i] = rem.NewSyncResultWithTrue(reply)
		}()
	}
	wg.Wait()
	budgetSyncReply(ret, rem.MaxSyncReplyBytes)
	return ret, nil
}

// budgetSyncReply keeps the batch's reply within budget bytes of encoded
// results, in call order: a reply that would go past it is replaced with a
// TooBigError, so the client makes that call on its own (as it does for any
// call the batch could not answer). Without it, a batch of large chains could
// outgrow the transport's frame limit (core.RpcMaxSz) and fail as a whole.
func budgetSyncReply(ret []rem.SyncResult, budget int) {
	used := 0
	for i := range ret {
		ok, err := ret[i].GetOk()
		if err != nil || !ok {
			continue
		}
		b, err := core.EncodeToBytes(&ret[i])
		if err == nil && used+len(b) <= budget {
			used += len(b)
			continue
		}
		if err == nil {
			err = core.TooBigError{Limit: budget, Actual: used + len(b), Desc: "sync reply budget"}
		}
		ret[i] = rem.NewSyncResultWithFalse(core.ErrorToStatus(err))
	}
}

func (c *UserClientConn) syncOne(ctx context.Context, call rem.SyncCall) (rem.SyncReply, error) {
	var ret rem.SyncReply
	typ, err := call.GetT()
	if err != nil {
		return ret, err
	}
	// Each load is observed under the method it stands in for, as if it had
	// come on its own (Telemetry.ObserveCall).
	observe := func(method string, f func(ctx context.Context) error) error {
		return shared.NewMetaContextConn(ctx, c).G().Telemetry().ObserveCall(ctx, method, f)
	}
	switch typ {
	case rem.SyncCallType_LoadUserChain:
		err = observe("User.loadUserChain", func(ctx context.Context) error {
			res, err := c.LoadUserChain(ctx, call.Loaduserchain())
			ret = rem.NewSyncReplyWithLoaduserchain(res)
			return err
		})
	case rem.SyncCallType_LoadGenericChain:
		err = observe("User.loadGenericChain", func(ctx context.Context) error {
			res, err := c.LoadGenericChain(ctx, call.Loadgenericchain().Import())
			ret = rem.NewSyncReplyWithLoadgenericchain(res)
			return err
		})
	case rem.SyncCallType_TeamListServerTrust:
		err = observe("User.getTeamListServerTrust", func(ctx context.Context) error {
			res, err := c.GetTeamListServerTrust(ctx)
			ret = rem.NewSyncReplyWithTeamlistservertrust(res)
			return err
		})
	case rem.SyncCallType_LoadTeamChain:
		err = observe("TeamLoader.loadTeamChain", func(ctx context.Context) error {
			res, err := c.LoadTeamChain(ctx, call.Loadteamchain())
			ret = rem.NewSyncReplyWithLoadteamchain(res)
			return err
		})
	case rem.SyncCallType_LoadTeamMembershipChain:
		err = observe("TeamLoader.loadTeamMembershipChain", func(ctx context.Context) error {
			res, err := c.LoadTeamMembershipChain(ctx, call.Loadteammembershipchain())
			ret = rem.NewSyncReplyWithLoadteammembershipchain(res)
			return err
		})
	default:
		err = core.BadArgsError("sync: unknown call type")
	}
	return ret, err
}
