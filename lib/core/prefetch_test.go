// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"context"
	"testing"

	"github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-snowpack-rpc/rpc"
	"github.com/stretchr/testify/require"
)

// wrapUserArg builds the argument exactly as the generated UserClient does.
func wrapUserArg(a rem.LoadUserChainArg) *rpc.DataWrap[lib.Header, *rem.UserLoadUserChainArgInternal__] {
	return &rpc.DataWrap[lib.Header, *rem.UserLoadUserChainArgInternal__]{
		Header: MakeProtoHeader(),
		Data:   rem.UserLoadUserChainArg{A: a}.Export(),
	}
}

func planUserChain(t *testing.T, a rem.LoadUserChainArg) PlannedCall {
	pc, err := PlanCall(func(cli rpc.GenericClient) error {
		_, err := NewUserClient(cli, nil).LoadUserChain(context.Background(), a)
		return err
	})
	require.NoError(t, err)
	return pc
}

func TestPlanCall(t *testing.T) {
	pc := planUserChain(t, rem.LoadUserChainArg{Start: 5})
	require.Equal(t, "User.loadUserChain", pc.Method())

	// A planning run must make exactly one call.
	_, err := PlanCall(func(rpc.GenericClient) error { return nil })
	require.Error(t, err)
	_, err = PlanCall(func(cli rpc.GenericClient) error {
		_, _ = NewUserClient(cli, nil).Ping(context.Background())
		_, err := NewUserClient(cli, nil).Ping(context.Background())
		return err
	})
	require.Error(t, err)
}

func TestPrefetchScope(t *testing.T) {
	var uid lib.UID
	uid[0] = 0x01
	arg := rem.LoadUserChainArg{Uid: uid, Start: 5}
	want := rem.UserChain{UsernameUtf8: "alice", NumUsernameLinks: 1}
	const method = "User.loadUserChain"

	cli, other := &RpcClient{}, &RpcClient{}

	// No scope: nothing served.
	var nilScope *PrefetchScope
	require.False(t, nilScope.serve(cli, method, wrapUserArg(arg), nil))

	ctx, s := WithPrefetchScope(context.Background())
	ctx2, s2 := WithPrefetchScope(ctx)
	require.Same(t, s, s2, "a nested operation shares its parent's scope")
	require.Equal(t, ctx, ctx2)

	require.NoError(t, s.Put(cli, planUserChain(t, arg), want.Export()))
	require.Equal(t, 1, s.Pending())

	// Another client, method or argument misses.
	var res rpc.DataWrap[lib.Header, rem.UserChainInternal__]
	require.False(t, s.serve(other, method, wrapUserArg(arg), &res))
	require.False(t, s.serve(cli, "User.loadGenericChain", wrapUserArg(arg), &res))
	require.False(t, s.serve(cli, method, wrapUserArg(rem.LoadUserChainArg{Uid: uid, Start: 6}), &res))

	// The exact call hits, with the header a real reply would carry.
	require.True(t, s.serve(cli, method, wrapUserArg(arg), &res))
	require.Equal(t, want, res.Data.Import())
	v, err := res.Header.GetV()
	require.NoError(t, err)
	require.Equal(t, lib.HeaderVersion_V1, v)
	require.Equal(t, 1, s.Hits())
	require.Equal(t, map[string]int{method: 1}, s.HitsByMethod())

	// Single use: the same call again goes to the network.
	require.False(t, s.serve(cli, method, wrapUserArg(arg), &res))
	require.Equal(t, 0, s.Pending())
}

// A stored reply that won't decode into the call's result is a miss, and
// leaves the result as it found it for the network reply.
func TestPrefetchScopeUndecodableReplyMisses(t *testing.T) {
	_, s := WithPrefetchScope(context.Background())
	cli := &RpcClient{}
	arg := rem.LoadUserChainArg{Start: 3}
	require.NoError(t, s.Put(cli, planUserChain(t, arg), "not a user chain"))
	var res rpc.DataWrap[lib.Header, rem.UserChainInternal__]
	require.False(t, s.serve(cli, "User.loadUserChain", wrapUserArg(arg), &res))
	require.Equal(t, rpc.DataWrap[lib.Header, rem.UserChainInternal__]{}, res)
	require.Zero(t, s.Hits())
}

// A plain (unwrapped) argument and result, as the TeamLoader protocol uses.
func TestPrefetchScopePlainArgs(t *testing.T) {
	_, s := WithPrefetchScope(context.Background())
	cli := &RpcClient{}
	arg := rem.LoadTeamMembershipChainArg{Start: 3}
	pc, err := PlanCall(func(cli rpc.GenericClient) error {
		_, err := rem.TeamLoaderClient{Cli: cli}.LoadTeamMembershipChain(context.Background(), arg)
		return err
	})
	require.NoError(t, err)
	require.NoError(t, s.Put(cli, pc, rem.GenericChain{}.Export()))
	var res rem.GenericChainInternal__
	require.True(t, s.serve(cli, pc.Method(), arg.Export(), &res))
}

// A refusal is remembered for the connection it came over, and only that
// one: a re-dial may reach an upgraded server.
func TestRefusalsPerConnection(t *testing.T) {
	type fakeXp struct{ rpc.Transporter }
	xp1, xp2 := &fakeXp{}, &fakeXp{}
	var r refusals
	require.False(t, r.has(xp1, "User.sync"))
	r.note(xp1, "User.sync")
	require.True(t, r.has(xp1, "User.sync"))
	require.False(t, r.has(xp1, "User.ping"))
	require.False(t, r.has(xp2, "User.sync"), "a new connection asks again")
	r.note(xp2, "User.ping")
	require.False(t, r.has(xp1, "User.sync"), "the old connection's set is gone")
	require.True(t, r.has(xp2, "User.ping"))
}

// Only a call made under WithOptionalMethod for its own method is optional.
func TestOptionalMethodContext(t *testing.T) {
	o := MustOptionalMethodOf(func(cli rpc.GenericClient) error {
		_, err := NewUserClient(cli, nil).Sync(context.Background(), nil)
		return err
	})
	require.Equal(t, "User.sync", o.Name())
	ctx := WithOptionalMethod(context.Background(), o)
	require.Equal(t, o.Name(), optionalMethodFromContext(ctx))
	require.Empty(t, optionalMethodFromContext(context.Background()))
}

// A User.sync answer at its budget still fits in one transport frame, with
// room for the framing and the error statuses of the calls left out.
func TestSyncReplyBudgetFitsFrame(t *testing.T) {
	require.LessOrEqual(t, 2*rem.MaxSyncReplyBytes, int(RpcMaxSz))
}
