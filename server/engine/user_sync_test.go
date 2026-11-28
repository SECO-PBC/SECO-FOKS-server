// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

func syncListReply(n int) rem.SyncReply {
	return rem.NewSyncReplyWithTeamlistservertrust(make(rem.LocalTeamList, n))
}

func syncResultSize(t *testing.T, r rem.SyncResult) int {
	b, err := core.EncodeToBytes(&r)
	require.NoError(t, err)
	return len(b)
}

func syncOK(t *testing.T, r rem.SyncResult) bool {
	ok, err := r.GetOk()
	require.NoError(t, err)
	return ok
}

// TestSyncReplyBudget: replies are kept while they fit the budget; errors
// pass through; a reply past it is refused as too big. small and big don't
// both fit, and whichever completes second is the one refused.
func TestSyncReplyBudget(t *testing.T) {
	small, big := syncListReply(1), syncListReply(50)
	smallSz := syncResultSize(t, rem.NewSyncResultWithTrue(small))
	bigSz := syncResultSize(t, rem.NewSyncResultWithTrue(big))

	run := func(budget int, replies []rem.SyncReply, errs []error) []rem.SyncResult {
		return runSyncBatch(context.Background(), len(replies), budget,
			func(_ context.Context, i int) (rem.SyncReply, error) {
				return replies[i], errs[i]
			})
	}
	ret := run(smallSz+bigSz-1,
		[]rem.SyncReply{small, {}, big},
		[]error{nil, core.PermissionError("no"), nil})
	require.True(t, syncOK(t, ret[0]))
	require.False(t, syncOK(t, ret[1]))
	require.Equal(t, proto.StatusCode_PERMISSION_ERROR, ret[1].False().Sc)
	if syncOK(t, ret[2]) {
		// big completed first and took the budget; small must then be out.
		require.False(t, syncOK(t, ret[0]))
	} else {
		require.Equal(t, proto.StatusCode_KV_TOO_BIG_ERROR, ret[2].False().Sc)
	}
}

// TestSyncBudgetStopsStartingCalls: a batch of many large calls stops
// starting calls once the budget is spent -- it neither runs nor holds all
// of them -- and answers the rest too big. What it keeps fits the budget.
func TestSyncBudgetStopsStartingCalls(t *testing.T) {
	big := syncListReply(2000)
	bigSz := syncResultSize(t, rem.NewSyncResultWithTrue(big))
	budget := 4*bigSz + bigSz/2
	n := rem.MaxSyncCalls

	var started atomic.Int32
	ret := runSyncBatch(context.Background(), n, budget,
		func(context.Context, int) (rem.SyncReply, error) {
			started.Add(1)
			return big, nil
		})

	kept, used := 0, 0
	for _, r := range ret {
		if syncOK(t, r) {
			kept++
			used += syncResultSize(t, r)
			continue
		}
		require.Equal(t, proto.StatusCode_KV_TOO_BIG_ERROR, r.False().Sc)
	}
	require.Equal(t, 4, kept)
	require.LessOrEqual(t, used, budget)
	// The first refusal ends the starting; at most the calls already in
	// flight then (syncParallelism) still run.
	require.LessOrEqual(t, int(started.Load()), kept+1+syncParallelism)
	require.Less(t, int(started.Load()), n)
}

// A cancelled batch answers the calls it hadn't started with the context's
// error.
func TestSyncBatchCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ret := runSyncBatch(ctx, 3*syncParallelism, rem.MaxSyncReplyBytes,
		func(context.Context, int) (rem.SyncReply, error) {
			return syncListReply(1), nil
		})
	notOK := 0
	for _, r := range ret {
		if !syncOK(t, r) {
			notOK++
		}
	}
	require.Positive(t, notOK)
}
