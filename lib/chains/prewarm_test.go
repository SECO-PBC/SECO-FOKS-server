// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package chains

import (
	"crypto/tls"
	"sync/atomic"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/keybase/clockwork"
	"github.com/stretchr/testify/require"
)

type fakeClient struct{ down atomic.Bool }

func (f *fakeClient) Shutdown() { f.down.Store(true) }

func newTestPool() (*prewarmPool[*fakeClient], clockwork.FakeClock) {
	clk := clockwork.NewFakeClock()
	return &prewarmPool[*fakeClient]{clock: clk}, clk
}

func certWith(chain ...[]byte) *tls.Certificate { return &tls.Certificate{Certificate: chain} }

func TestPrewarmPoolHandsOverOnce(t *testing.T) {
	p, _ := newTestPool()
	k := prewarmKey{st: proto.ServerType_KVStore, cert: core.ClientCertID(certWith([]byte{1}))}
	c := &fakeClient{}

	require.True(t, p.put(k, c))
	require.False(t, p.put(k, &fakeClient{}), "one client per service and cert")
	require.True(t, p.has(k))

	got, ok := p.take(k)
	require.True(t, ok)
	require.Same(t, c, got)

	_, ok = p.take(k)
	require.False(t, ok, "a pre-warmed client is handed out once")

	// Taken clients belong to their taker: expiry leaves them alone.
	p.expire(k, c)
	require.False(t, c.down.Load())
}

// The pool keys on the chain presented, the identity every client cache
// uses: a certificate rebuilt with the same chain finds the client.
func TestPrewarmPoolKeysOnServiceAndChain(t *testing.T) {
	p, _ := newTestPool()
	a := core.ClientCertID(certWith([]byte{1}))
	require.True(t, p.put(prewarmKey{st: proto.ServerType_KVStore, cert: a}, &fakeClient{}))

	_, ok := p.take(prewarmKey{st: proto.ServerType_RealTime, cert: a})
	require.False(t, ok, "another service")
	_, ok = p.take(prewarmKey{st: proto.ServerType_KVStore, cert: core.ClientCertID(certWith([]byte{2}))})
	require.False(t, ok, "another chain")
	_, ok = p.take(prewarmKey{st: proto.ServerType_KVStore, cert: core.ClientCertID(certWith([]byte{1}))})
	require.True(t, ok, "the same chain in another *tls.Certificate")
}

func TestPrewarmPoolExpiresUnclaimed(t *testing.T) {
	p, clk := newTestPool()
	k := prewarmKey{st: proto.ServerType_User}
	c := &fakeClient{}
	require.True(t, p.put(k, c))

	clk.BlockUntil(1)
	clk.Advance(prewarmUnclaimedAfter - time.Second)
	require.True(t, p.has(k), "still waiting to be taken")
	clk.Advance(time.Second)
	require.Eventually(t, c.down.Load, time.Second, time.Millisecond, "an unclaimed client is shut down")
	require.False(t, p.has(k))

	// A newer client held under the same key is not the expired one.
	c2 := &fakeClient{}
	require.True(t, p.put(k, c2))
	p.expire(k, c)
	require.False(t, c2.down.Load())
	require.True(t, p.has(k))
}

// A client taken before its expiry is never shut down by it.
func TestPrewarmPoolLateExpiryLeavesTakenClient(t *testing.T) {
	p, clk := newTestPool()
	k := prewarmKey{st: proto.ServerType_RealTime}
	c := &fakeClient{}
	require.True(t, p.put(k, c))
	clk.BlockUntil(1)
	_, ok := p.take(k)
	require.True(t, ok)
	clk.Advance(prewarmUnclaimedAfter)
	time.Sleep(20 * time.Millisecond)
	require.False(t, c.down.Load())
}
