// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/chains"
	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// coldDiscovery is a fresh discovery manager over a home where an earlier
// run probed the user's host -- what a cold start finds.
func coldDiscovery(t *testing.T) (*TestEnvWrapper, libclient.MetaContext, *chains.DiscoveryMgr, proto.HostID) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()
	m := tew.NewClientMetaContextAtHome(t, u, home)
	return tew, m, chains.NewDiscoveryMgr(&libclient.DiscoveryEngine{}), u.host
}

func probeCalls(st *core.RpcStats) int {
	for _, ms := range st.ByMethod() {
		if ms.Method == "Probe.probe" {
			return ms.Calls
		}
	}
	return 0
}

// TestProbeCachedOKStartsAtOnce: a host verified before is usable at once
// from its stored identity, and a full probe re-verifies it in the
// background; the same identity keeps the probe connected.
func TestProbeCachedOKStartsAtOnce(t *testing.T) {
	_, m, dm, host := coldDiscovery(t)
	m, stats := m.WithRpcStats()

	pr, err := dm.Probe(m, chains.ProbeArg{HostID: host, Timeout: time.Minute, CachedOK: true})
	require.NoError(t, err)
	require.True(t, pr.IsConnected())
	require.NotNil(t, pr.PublicZone())
	require.Zero(t, probeCalls(stats), "no probe round trip before use")

	require.Eventually(t, func() bool { return probeCalls(stats) == 1 }, 10*time.Second, 20*time.Millisecond,
		"re-verified in the background")
	time.Sleep(200 * time.Millisecond)
	require.True(t, pr.IsConnected(), "same identity: carries on")
}

// TestProbeCachedOKIdentityChanged: if the host's identity is not what was
// stored, the background probe marks the cached probe failed, and its next
// use re-probes and takes the host's real identity.
func TestProbeCachedOKIdentityChanged(t *testing.T) {
	_, m, dm, host := coldDiscovery(t)

	// What this device stored no longer matches the host: a service moved.
	var di libclient.DiscoveryEngine
	pz, err := di.LoadPublicZoneFromDB(m, host)
	require.NoError(t, err)
	real := *pz
	stale := *pz
	stale.Services.Reg = proto.TCPAddr("stale.example:4432")
	require.NoError(t, di.StorePublicZoneToDB(m, host, &stale))

	pr, err := dm.Probe(m, chains.ProbeArg{HostID: host, Timeout: time.Minute, CachedOK: true})
	require.NoError(t, err)
	require.Equal(t, stale.Services.Reg, pr.PublicZone().Services.Reg, "used at once, as stored")

	require.Eventually(t, func() bool { return !pr.IsConnected() }, 10*time.Second, 20*time.Millisecond,
		"the background probe finds the identity changed")
	require.NoError(t, pr.Reconnect(m))
	require.True(t, pr.IsConnected())
	require.Equal(t, real.Services.Reg, pr.PublicZone().Services.Reg, "re-probed: the host's real zone")
}
