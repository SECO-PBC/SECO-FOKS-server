// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// TestColdStartUsesSavedCertChain: a cold start presents the client
// certificate chain an earlier run saved, instead of asking the reg server
// for one first (four round trips: a reg connection, its vhost select and
// the call). The reg server stamps a new serial on every chain it issues, so
// the leaf in use tells a saved chain from a fresh one.
func TestColdStartUsesSavedCertChain(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	home := t.TempDir()
	warm := tew.NewClientMetaContextAtHome(t, u, home)
	warmCert, err := warm.G().ActiveUser().ClientCert(warm)
	require.NoError(t, err)

	cold := tew.NewClientMetaContextAtHome(t, u, home)
	au := cold.G().ActiveUser()
	coldCert, err := au.ClientCert(cold)
	require.NoError(t, err)
	require.Equal(t, warmCert.Certificate, coldCert.Certificate,
		"the cold start should present the saved chain, not a freshly issued one")

	// And the saved chain still authenticates.
	ucli, err := au.UserClient(cold)
	require.NoError(t, err)
	uid, err := ucli.Ping(cold.Ctx())
	require.NoError(t, err)
	require.Equal(t, u.uid, uid)

	// In the background the reg server confirms the key. The saved chain is
	// still good, so nothing is rewritten: the row stays the chain in use, and
	// a lock/unlock or the next start presents the same chain (no client is
	// rebuilt for a reissued leaf).
	require.Eventually(t, func() bool { return !au.SavedCertUnconfirmedForTest() },
		10*time.Second, 50*time.Millisecond)
	eid, err := au.ClientCertKeyForTest(cold)
	require.NoError(t, err)
	var row lcl.UserCertChain
	_, err = cold.DbGet(&row, libclient.DbTypeSoft, &u.uid, lcl.DataType_UserCertChain, eid)
	require.NoError(t, err)
	require.Equal(t, core.ChainID(warmCert.Certificate), core.ChainID(row.Certs),
		"a confirmed saved chain is not replaced by the reissued one")

	// So a third start presents it again.
	third := tew.NewClientMetaContextAtHome(t, u, home)
	thirdCert, err := third.G().ActiveUser().ClientCert(third)
	require.NoError(t, err)
	require.Equal(t, core.ClientCertID(warmCert), core.ClientCertID(thirdCert))
}

// TestSavedCertRefusedAfterRevoke: a cold start on a device revoked since
// the last run presents its saved chain at once, but the background
// confirmation's refusal drops it, so the next use asks the reg server and
// sees the refusal, rather than retrying the confirmation forever.
func TestSavedCertRefusedAfterRevoke(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUserFakeRoot(t)
	tew.DirectMerklePokeForLeafCheck(t)
	other := u.ProvisionNewDevice(t, u.eldest, "other", proto.DeviceType_Computer,
		proto.NewRoleDefault(proto.RoleType_OWNER))
	tew.DirectMerklePokeForLeafCheck(t)

	home := t.TempDir()
	warm := tew.NewClientMetaContextAtHome(t, u, home)
	_, err := warm.G().ActiveUser().ClientCert(warm)
	require.NoError(t, err)

	cold := tew.NewClientMetaContextAtHome(t, u, home)
	au := cold.G().ActiveUser()
	eid, err := au.ClientCertKeyForTest(cold)
	require.NoError(t, err)

	tew.DirectMerklePokeForLeafCheck(t)
	u.RevokeDevice(t, other, u.eldest)
	tew.DirectMerklePokeForLeafCheck(t)

	_, err = au.ClientCert(cold)
	require.NoError(t, err, "the saved chain is used without asking the reg server")

	require.Eventually(t, func() bool { return !au.SavedCertUnconfirmedForTest() },
		10*time.Second, 50*time.Millisecond)
	var row lcl.UserCertChain
	_, err = cold.DbGet(&row, libclient.DbTypeSoft, &u.uid, lcl.DataType_UserCertChain, eid)
	require.Error(t, err, "the refused chain is no longer saved")

	_, err = au.ClientCert(cold)
	require.Error(t, err)
	require.True(t, core.IsAuthError(err), "got %T: %v", err, err)
}

// TestReplacedCertReachesClients: when the certificate in use is replaced (a
// saved chain giving way to a fresh one after a CA change), the user client
// is rebuilt with it rather than presenting the old one on every re-dial.
func TestReplacedCertReachesClients(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	m := tew.NewClientMetaContext(t, u)
	au := m.G().ActiveUser()
	g1, err := au.UserGCli(m)
	require.NoError(t, err)
	cur, err := au.ClientCert(m)
	require.NoError(t, err)

	eid, err := au.ClientCertKeyForTest(m)
	require.NoError(t, err)
	reg, err := au.RegClient(m)
	require.NoError(t, err)
	fresh, err := reg.GetClientCertChain(m.Ctx(), rem.GetClientCertChainArg{Uid: u.uid, Key: eid})
	require.NoError(t, err)
	require.True(t, au.PrivKeys.ReplaceCert(cur, &tls.Certificate{PrivateKey: cur.PrivateKey, Certificate: fresh}))

	g2, err := au.UserGCli(m)
	require.NoError(t, err)
	require.NotSame(t, g1, g2, "the user client follows the certificate in use")
	ucli, err := au.UserClient(m)
	require.NoError(t, err)
	uid, err := ucli.Ping(m.Ctx())
	require.NoError(t, err)
	require.Equal(t, u.uid, uid)
}
