// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package rem

// MaxSyncCalls is the most calls one User.sync batch may carry. The server
// refuses a longer batch; a client with more to ask splits it into batches of
// at most this many, sent at once (docs/rtt-budget.md).
const MaxSyncCalls = 512

// MaxSyncReplyBytes bounds the encoded replies in one User.sync answer. The
// server answers the calls that fit, in order, and refuses the rest as too
// big, which the client then makes on its own. Well under the transport's
// frame limit (core.RpcMaxSz; TestSyncReplyBudgetFitsFrame), so a batch's
// answer always fits in one frame.
const MaxSyncReplyBytes = 4 << 20

// SyncLoadGenericChainArgFrom is the batched form of a loadGenericChain
// call. The two structs carry the same fields; this pair of conversions is
// the one place that copies them (TestSyncLoadGenericChainArgFields keeps
// them in step).
func SyncLoadGenericChainArgFrom(a LoadGenericChainArg) SyncLoadGenericChainArg {
	return SyncLoadGenericChainArg{Eid: a.Eid, Typ: a.Typ, Start: a.Start}
}

// Import is the loadGenericChain call a batched one stands in for.
func (a SyncLoadGenericChainArg) Import() LoadGenericChainArg {
	return LoadGenericChainArg{Eid: a.Eid, Typ: a.Typ, Start: a.Start}
}
