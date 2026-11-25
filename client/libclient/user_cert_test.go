// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, keyID byte) testCA {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
		SubjectKeyId:          []byte{keyID},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return testCA{cert: cert, key: key}
}

// leaf issues a client certificate valid from notBefore to notAfter, the way
// the reg server does (client_ca.go): a new serial each time.
func (ca testCA) leaf(t *testing.T, serial int64, notBefore, notAfter time.Time) [][]byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	return [][]byte{der}
}

func TestCertChainValidFor(t *testing.T) {
	ca := newTestCA(t, 1)
	now := time.Now()
	day := 24 * time.Hour

	require.True(t, certChainValidFor(ca.leaf(t, 2, now.Add(-day), now.AddDate(10, 0, 0)), now, day),
		"a ten-year chain is usable")
	require.False(t, certChainValidFor(ca.leaf(t, 3, now.Add(-day), now.Add(time.Hour)), now, day),
		"one expiring within the margin is fetched afresh instead")
	require.True(t, certChainValidFor(ca.leaf(t, 4, now.Add(time.Hour), now.AddDate(1, 0, 0)), now, day),
		"NotBefore is the server's clock; a device running behind still uses the chain")
	require.False(t, certChainValidFor(nil, now, day))
	require.False(t, certChainValidFor([][]byte{[]byte("not a certificate")}, now, day))
}

// The reg server reissues on every fetch, so a refreshed chain always differs
// from the saved one; only a different issuing CA means the saved one stopped
// working.
func TestSameCertIssuer(t *testing.T) {
	now := time.Now()
	ca1, ca2 := newTestCA(t, 1), newTestCA(t, 2)
	saved := ca1.leaf(t, 10, now, now.AddDate(10, 0, 0))
	reissued := ca1.leaf(t, 11, now, now.AddDate(10, 0, 0))
	rotated := ca2.leaf(t, 12, now, now.AddDate(10, 0, 0))

	require.NotEqual(t, saved, reissued)
	require.True(t, sameCertIssuer(saved, reissued))
	require.False(t, sameCertIssuer(saved, rotated))
	require.False(t, sameCertIssuer(saved, nil))
	require.False(t, sameCertIssuer(saved, [][]byte{[]byte("junk")}))
}

func TestReplaceSavedCert(t *testing.T) {
	now := time.Now()
	ca1, ca2 := newTestCA(t, 1), newTestCA(t, 2)
	saved := ca1.leaf(t, 20, now, now.AddDate(10, 0, 0))

	require.False(t, replaceSavedCert(saved, ca1.leaf(t, 21, now, now.AddDate(10, 0, 0)), now),
		"a reissue from the same CA leaves the saved chain in use")
	require.True(t, replaceSavedCert(saved, ca2.leaf(t, 22, now, now.AddDate(10, 0, 0)), now),
		"a new issuing CA means the saved chain stopped working")
	expiring := ca1.leaf(t, 23, now, now.Add(time.Hour))
	require.True(t, replaceSavedCert(expiring, ca1.leaf(t, 24, now, now.AddDate(10, 0, 0)), now),
		"one about to expire gives way, same CA or not")
}

func TestSameCertChain(t *testing.T) {
	require.True(t, SameCertChain([][]byte{{1, 2}, {3}}, [][]byte{{1, 2}, {3}}))
	require.False(t, SameCertChain([][]byte{{1, 2}}, [][]byte{{1, 3}}))
	require.False(t, SameCertChain([][]byte{{1}}, [][]byte{{1}, {2}}))
	require.True(t, SameCertChain(nil, nil))
}

// ReplaceCert is a compare-and-swap: a Clear (UserLock) or another
// replacement in between is not undone.
func TestUserPrivateKeysReplaceCert(t *testing.T) {
	var k UserPrivateKeys
	a, b, c := &tls.Certificate{}, &tls.Certificate{}, &tls.Certificate{}
	k.SetCert(a)
	require.True(t, k.ReplaceCert(a, b))
	require.Same(t, b, k.GetCert())
	require.False(t, k.ReplaceCert(a, c), "a is no longer held")
	k.Clear()
	require.False(t, k.ReplaceCert(b, c), "a cleared cert stays cleared")
	require.Nil(t, k.GetCert())
}
