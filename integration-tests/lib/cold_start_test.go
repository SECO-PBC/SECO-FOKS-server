// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/proto/lcl"
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

	// In the background the chain is fetched from the reg server and saved
	// for the next start; wait for that write before the home is removed.
	eid, err := au.ClientCertKeyForTest(cold)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var row lcl.UserCertChain
		_, err := cold.DbGet(&row, libclient.DbTypeSoft, &u.uid, lcl.DataType_UserCertChain, eid)
		return err == nil && !libclient.SameCertChain(row.Certs, warmCert.Certificate)
	}, 10*time.Second, 50*time.Millisecond)
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
