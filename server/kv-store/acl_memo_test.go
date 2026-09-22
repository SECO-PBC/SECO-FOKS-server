// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

import (
	"errors"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestAclMemoCachesOnlySettledAnswers pins the rule that makes the
// per-request membership memo safe to have at all.
//
// The memo exists because a listing authorizes once per row, so without it a
// directory of N tagged files costs N pool acquires and N queries against a
// second database while the KV shard's cursor is open. Caching the decision
// collapses that to one query per channel.
//
// The subtlety is which answers may be cached. "Allowed" and "masked" are
// decisions about membership and are stable for the life of a request.
// A failure to REACH the realtime database is not a decision at all, and
// caching it would turn one blip into a denial for every remaining node in
// the request -- a listing that silently returned half a directory, which
// looks exactly like a correct ACL denial.
func TestAclMemoCachesOnlySettledAnswers(t *testing.T) {
	memo := &kvAclMemo{seen: make(map[int64]error)}

	_, ok := memo.get(7)
	require.False(t, ok, "an unseen channel must not report a cached answer")

	// A settled allow, and a settled denial, both round-trip.
	memo.put(7, nil)
	got, ok := memo.get(7)
	require.True(t, ok)
	require.NoError(t, got)

	memo.put(8, errKVNodeMasked)
	got, ok = memo.get(8)
	require.True(t, ok)
	require.ErrorIs(t, got, errKVNodeMasked)

	// Entries are per channel, not shared.
	_, ok = memo.get(9)
	require.False(t, ok)

	// kvAclSettled is the predicate kvChannelMembership itself consults, not
	// a copy of it: a test with its own copy would stay green if the real one
	// started caching infrastructure errors, which is the whole failure this
	// is here to prevent.
	require.True(t, kvAclSettled(nil))
	require.True(t, kvAclSettled(errKVNodeMasked))
	require.False(t, kvAclSettled(errors.New("connection refused")),
		"a transport failure is not a membership decision")
	require.False(t, kvAclSettled(core.InternalError("boom")))
	require.False(t, kvAclSettled(pgx.ErrNoRows),
		"a bare no-rows is not a membership decision either")
}
