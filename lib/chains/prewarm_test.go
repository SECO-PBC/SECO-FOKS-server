// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package chains

import (
	"crypto/tls"
	"testing"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

type fakeClient struct{ down bool }

func (f *fakeClient) Shutdown() { f.down = true }

func TestPrewarmPoolHandsOverOnce(t *testing.T) {
	var p prewarmPool[*fakeClient]
	cert := &tls.Certificate{}
	k := prewarmKey{st: proto.ServerType_KVStore, cert: cert}
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
	require.False(t, c.down)
}

func TestPrewarmPoolKeysOnServiceAndCert(t *testing.T) {
	var p prewarmPool[*fakeClient]
	a, b := &tls.Certificate{}, &tls.Certificate{}
	require.True(t, p.put(prewarmKey{st: proto.ServerType_KVStore, cert: a}, &fakeClient{}))

	_, ok := p.take(prewarmKey{st: proto.ServerType_RealTime, cert: a})
	require.False(t, ok, "another service")
	_, ok = p.take(prewarmKey{st: proto.ServerType_KVStore, cert: b})
	require.False(t, ok, "another certificate")
}

func TestPrewarmPoolExpiresUnclaimed(t *testing.T) {
	var p prewarmPool[*fakeClient]
	k := prewarmKey{st: proto.ServerType_User}
	c := &fakeClient{}
	require.True(t, p.put(k, c))

	p.expire(k, c)
	require.True(t, c.down, "an unclaimed client is shut down")
	require.False(t, p.has(k))

	// A newer client held under the same key is not the expired one.
	c2 := &fakeClient{}
	require.True(t, p.put(k, c2))
	p.expire(k, c)
	require.False(t, c2.down)
	require.True(t, p.has(k))
}
