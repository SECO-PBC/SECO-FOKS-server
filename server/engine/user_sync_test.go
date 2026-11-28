// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// TestSyncReplyBudget: replies are kept in call order while they fit the
// budget; one that would go past it is refused as too big, and a smaller one
// after it that still fits is kept. Errors pass through.
func TestSyncReplyBudget(t *testing.T) {
	list := func(n int) rem.SyncResult {
		return rem.NewSyncResultWithTrue(rem.NewSyncReplyWithTeamlistservertrust(
			make(rem.LocalTeamList, n)))
	}
	size := func(r rem.SyncResult) int {
		b, err := core.EncodeToBytes(&r)
		require.NoError(t, err)
		return len(b)
	}
	small, big := list(1), list(50)
	failed := rem.NewSyncResultWithFalse(core.ErrorToStatus(core.PermissionError("no")))
	ret := []rem.SyncResult{small, failed, big, small}
	budgetSyncReply(ret, size(small)+size(big)-1)

	okAt := func(i int) bool {
		ok, err := ret[i].GetOk()
		require.NoError(t, err)
		return ok
	}
	require.True(t, okAt(0))
	require.False(t, okAt(1))
	require.Equal(t, proto.StatusCode_PERMISSION_ERROR, ret[1].False().Sc)
	require.False(t, okAt(2), "past the budget")
	require.Equal(t, proto.StatusCode_KV_TOO_BIG_ERROR, ret[2].False().Sc)
	require.True(t, okAt(3), "still fits after the refused one")
}
