// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"context"
	"testing"
	"time"

	lcl "github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/keybase/clockwork"
	"github.com/stretchr/testify/require"
)

func registered(c *connectionMgr) bool {
	liveConns.Lock()
	defer liveConns.Unlock()
	_, ok := liveConns.conns[c]
	return ok
}

// A server that never answers stands in for a network that stopped carrying
// packets: the call is written, and its reply never comes. Closing every
// connection must release that call at once, with a transport error, and the
// next call must dial again.
func TestCloseConnectionsReleasesPendingCall(t *testing.T) {
	fc := clockwork.NewFakeClock() // never advanced: Slow never returns
	srv := testServer{cl: fc}
	srv.start(t)
	defer srv.stop(t)
	m := newRpcLogMetaContext()
	gcli := NewRpcClient(m, proto.TCPAddr(srv.connectTo), srv.tlsConfig.RootCAs, nil, nil)
	defer gcli.Shutdown()
	cli := lcl.TestLibsClient{Cli: gcli, ErrorUnwrapper: StatusToError}
	ctx := context.Background()

	_, err := cli.Fast(ctx, 1)
	require.NoError(t, err)
	require.True(t, registered(gcli.cm))

	errCh := make(chan error, 1)
	go func() {
		_, err := cli.Slow(ctx, lcl.SlowArg{X: 1, Wait: proto.ExportDurationMilli(time.Hour)})
		errCh <- err
	}()
	// Let the Slow call reach the server before pulling the plug.
	fc.BlockUntil(1)

	// Another host's connections are left alone...
	CloseConnectionsTo("elsewhere.example")
	select {
	case err := <-errCh:
		t.Fatalf("closing another host released this call: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// ...and this host's are dropped.
	start := time.Now()
	CloseConnectionsTo("LOCALHOST")
	require.Less(t, time.Since(start), time.Second)

	select {
	case err := <-errCh:
		require.Error(t, err)
		require.True(t, IsTransportError(err), "got %T: %v", err, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pending call was not released by CloseAllConnections")
	}

	res, err := cli.Fast(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, int64(2), res)
	require.True(t, registered(gcli.cm))

	// CloseAllConnections drops every host.
	go func() {
		_, err := cli.Slow(ctx, lcl.SlowArg{X: 1, Wait: proto.ExportDurationMilli(time.Hour)})
		errCh <- err
	}()
	fc.BlockUntil(2) // the first Slow's server handler is still parked too
	CloseAllConnections()
	select {
	case err := <-errCh:
		require.True(t, IsTransportError(err), "got %T: %v", err, err)
	case <-time.After(5 * time.Second):
		t.Fatal("pending call was not released by CloseAllConnections")
	}
}

func TestConnRegistryClearedOnShutdownAndIdle(t *testing.T) {
	srv := testServer{}
	srv.start(t)
	defer srv.stop(t)
	m := newRpcLogMetaContext()
	clock := clockwork.NewFakeClock()
	opts := NewRpcClientOpts()
	opts.Clock = clock
	idleDisconnectCh := make(chan struct{}, 10)
	refcountCh := make(chan int, 10)
	exitCh := make(chan int, 10)
	opts.testIdleDisconnectCh = idleDisconnectCh
	opts.testRefcountUpdateCh = refcountCh
	opts.testExitCh = exitCh

	gcli := NewRpcClient(m, proto.TCPAddr(srv.connectTo), srv.tlsConfig.RootCAs, nil, opts)
	cli := lcl.TestLibsClient{Cli: gcli, ErrorUnwrapper: StatusToError}
	ctx := context.Background()
	drainRefcounts := func() {
		for i := range refcountCh {
			if i == 0 {
				return
			}
		}
	}

	_, err := cli.Fast(ctx, 1)
	require.NoError(t, err)
	drainRefcounts()
	require.True(t, registered(gcli.cm))

	clock.Advance(opts.IdleTimeout * 2)
	clock.Advance(opts.PollInterval * 2)
	<-idleDisconnectCh
	require.False(t, registered(gcli.cm))

	_, err = cli.Fast(ctx, 1)
	require.NoError(t, err)
	drainRefcounts()
	require.True(t, registered(gcli.cm))

	gcli.Shutdown()
	<-exitCh
	<-exitCh
	require.False(t, registered(gcli.cm))
}
