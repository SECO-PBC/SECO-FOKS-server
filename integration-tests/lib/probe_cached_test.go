// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"sync/atomic"
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
func coldDiscovery(t *testing.T, di chains.DiscoveryInterface) (libclient.MetaContext, *chains.DiscoveryMgr, proto.HostID) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	home := t.TempDir()
	m := tew.NewClientMetaContextAtHome(t, u, home)
	if di == nil {
		di = &libclient.DiscoveryEngine{}
	}
	dm := chains.NewDiscoveryMgr(di)
	t.Cleanup(dm.Shutdown)
	return m, dm, u.host
}

func probeCalls(st *core.RpcStats) int {
	for _, ms := range st.ByMethod() {
		if ms.Method == "Probe.probe" {
			return ms.Calls
		}
	}
	return 0
}

// staleZone makes what this device stored for host no longer match it (a
// service moved), and returns the host's real zone.
func staleZone(t *testing.T, m libclient.MetaContext, host proto.HostID) (real, stale proto.PublicZone) {
	var di libclient.DiscoveryEngine
	pz, err := di.LoadPublicZoneFromDB(m, host)
	require.NoError(t, err)
	real = *pz
	stale = *pz
	stale.Services.Reg = proto.TCPAddr("stale.example:4432")
	stale.Services.User = proto.TCPAddr("stale.example:4433")
	require.NoError(t, di.StorePublicZoneToDB(m, host, &stale))
	return real, stale
}

// TestProbeCachedOKStartsAtOnce: a host verified before is usable at once
// from its stored identity, and a full probe re-verifies it in the
// background; the same identity keeps the probe connected.
func TestProbeCachedOKStartsAtOnce(t *testing.T) {
	m, dm, host := coldDiscovery(t, nil)
	m, stats := m.WithRpcStats()

	pr, err := dm.ProbeCachedOK(m, host, time.Minute)
	require.NoError(t, err)
	require.True(t, pr.IsConnected())
	require.NotNil(t, pr.PublicZone())
	require.Zero(t, probeCalls(stats), "no probe round trip before use")

	require.Eventually(t, func() bool { return probeCalls(stats) == 1 }, 10*time.Second, 20*time.Millisecond,
		"re-verified in the background")
	time.Sleep(200 * time.Millisecond)
	require.True(t, pr.IsConnected(), "same identity: carries on")
}

// TestProbeCachedOKAfterShutdown: a manager that can no longer re-verify in
// the background does not start from the cached identity.
func TestProbeCachedOKAfterShutdown(t *testing.T) {
	m, dm, host := coldDiscovery(t, nil)
	m, stats := m.WithRpcStats()
	dm.Shutdown()

	pr, err := dm.ProbeCachedOK(m, host, time.Minute)
	require.NoError(t, err)
	require.True(t, pr.IsConnected())
	require.Equal(t, 1, probeCalls(stats), "probed before use")
}

// TestProbeCachedOKIdentityChanged: if the host's identity is not what was
// stored, the background probe marks the cached probe failed, and its next
// use re-probes and takes the host's real identity.
func TestProbeCachedOKIdentityChanged(t *testing.T) {
	m, dm, host := coldDiscovery(t, nil)
	real, stale := staleZone(t, m, host)

	pr, err := dm.ProbeCachedOK(m, host, time.Minute)
	require.NoError(t, err)
	require.Equal(t, stale.Services.Reg, pr.PublicZone().Services.Reg, "used at once, as stored")

	require.Eventually(t, func() bool { return !pr.IsConnected() }, 10*time.Second, 20*time.Millisecond,
		"the background probe finds the identity changed")
	require.NoError(t, pr.Reconnect(m))
	require.True(t, pr.IsConnected())
	require.Equal(t, real.Services.Reg, pr.PublicZone().Services.Reg, "re-probed: the host's real zone")
}

// TestProbeCachedOKPrewarmedAfterChange: a client prewarmed from the cached
// identity is not handed out once the background probe has found the
// identity changed; the probe re-probes first.
func TestProbeCachedOKPrewarmedAfterChange(t *testing.T) {
	m, dm, host := coldDiscovery(t, nil)
	real, stale := staleZone(t, m, host)

	pr, err := dm.ProbeCachedOK(m, host, time.Minute)
	require.NoError(t, err)
	pr.Prewarm(m, []proto.ServerType{proto.ServerType_User}, nil)
	require.Equal(t, stale.Services.User, pr.PublicZone().Services.User)

	require.Eventually(t, func() bool { return !pr.IsConnected() }, 10*time.Second, 20*time.Millisecond,
		"the background probe finds the identity changed")
	cli, err := pr.RPCClient(m, proto.ServerType_User, nil)
	require.NoError(t, err)
	defer cli.Shutdown()
	require.True(t, pr.IsConnected())
	require.Equal(t, real.Services.User, pr.PublicZone().Services.User,
		"re-probed before the client was made, not handed the prewarmed one")
}

// failStoreDiscovery fails the last step of a probe (saving the verified
// hostchain) while fail is set: a probe that fetched everything from the
// server and then did not complete.
type failStoreDiscovery struct {
	*libclient.DiscoveryEngine
	fail atomic.Bool
}

func (f *failStoreDiscovery) StoreHostchainToDB(m chains.MetaContext, hc *core.Hostchain) error {
	if f.fail.Load() {
		return core.InternalError("test: store refused")
	}
	return f.DiscoveryEngine.StoreHostchainToDB(m, hc)
}

// TestProbeCachedOKVerifyFailureStaysFailed: a probe whose re-probe fails
// on something other than transport keeps failing -- every later use
// re-probes -- and keeps none of the failed run's server data.
func TestProbeCachedOKVerifyFailureStaysFailed(t *testing.T) {
	di := &failStoreDiscovery{DiscoveryEngine: &libclient.DiscoveryEngine{}}
	m, dm, host := coldDiscovery(t, di)
	real, stale := staleZone(t, m, host)
	di.fail.Store(true)

	pr, err := dm.ProbeCachedOK(m, host, time.Minute)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !pr.IsConnected() }, 10*time.Second, 20*time.Millisecond,
		"the background probe fails")

	for range 2 {
		require.Error(t, pr.Reconnect(m))
		require.False(t, pr.IsConnected(), "still failed: the next use probes again")
		require.Equal(t, stale.Services.Reg, pr.PublicZone().Services.Reg,
			"nothing the failed run fetched is kept")
	}

	di.fail.Store(false)
	require.NoError(t, pr.Reconnect(m))
	require.True(t, pr.IsConnected())
	require.Equal(t, real.Services.Reg, pr.PublicZone().Services.Reg)
}
