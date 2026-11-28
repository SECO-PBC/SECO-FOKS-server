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
	return runSyncBatch(ctx, len(calls), rem.MaxSyncReplyBytes, func(ctx context.Context, i int) (rem.SyncReply, error) {
		return c.syncOne(ctx, calls[i])
	}), nil
}

// runSyncBatch runs n calls, at most syncParallelism at once, and keeps the
// answer within budget bytes of encoded results as they complete: a reply
// that would go past what is left is replaced with a TooBigError, and once
// one has been, or the budget is used up, the calls not yet started are not
// started and are answered TooBigError too. The client makes each of those
// on its own, as it does for any call the batch could not answer. So a
// batch of large chains never outgrows the transport's frame limit
// (core.RpcMaxSz), and the server neither runs nor holds more than about a
// budget's worth of replies. Which calls get in is completion order, not
// call order.
//
// Each reply is encoded once here to be measured; the transport encodes the
// answer again to send it, and offers no way to reuse these bytes.
func runSyncBatch(
	ctx context.Context,
	n int,
	budget int,
	run func(ctx context.Context, i int) (rem.SyncReply, error),
) []rem.SyncResult {
	ret := make([]rem.SyncResult, n)
	tooBig := func(actual int) rem.SyncResult {
		return rem.NewSyncResultWithFalse(core.ErrorToStatus(
			core.TooBigError{Limit: budget, Actual: actual, Desc: "sync reply budget"}))
	}
	var mu sync.Mutex // guards used and spent
	used := 0
	spent := false
	isSpent := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return spent
	}

	sem := make(chan struct{}, syncParallelism)
	var wg sync.WaitGroup
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			ret[i] = rem.NewSyncResultWithFalse(core.ErrorToStatus(ctx.Err()))
			continue
		}
		if isSpent() {
			<-sem
			ret[i] = tooBig(budget)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			reply, err := run(ctx, i)
			if err != nil {
				ret[i] = rem.NewSyncResultWithFalse(core.ErrorToStatus(err))
				return
			}
			res := rem.NewSyncResultWithTrue(reply)
			b, err := core.EncodeToBytes(&res)
			if err != nil {
				ret[i] = rem.NewSyncResultWithFalse(core.ErrorToStatus(err))
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if used+len(b) > budget {
				spent = true
				ret[i] = tooBig(used + len(b))
				return
			}
			used += len(b)
			if used >= budget {
				spent = true
			}
			ret[i] = res
		}()
	}
	wg.Wait()
	return ret
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
