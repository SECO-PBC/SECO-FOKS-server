// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
)

// CertChainID identifies a client certificate by the chain it presents: two
// certificates with the same ID present the same chain (and so, since the
// leaf binds the public key, prove the same key). It is the one notion of
// "the same certificate" shared by everything that caches a client built
// with one, so a rebuilt *tls.Certificate with an unchanged chain matches,
// and a replaced chain does not.
type CertChainID [sha256.Size]byte

// NoClientCert is the CertChainID of a nil certificate, for clients that
// present none.
var NoClientCert CertChainID

// ClientCertID is the CertChainID of c; NoClientCert for nil.
func ClientCertID(c *tls.Certificate) CertChainID {
	if c == nil {
		return NoClientCert
	}
	return ChainID(c.Certificate)
}

// ChainID hashes a DER chain, each certificate length-prefixed so that
// different splits of the same bytes differ.
func ChainID(chain [][]byte) CertChainID {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(chain)))
	h.Write(n[:])
	for _, der := range chain {
		binary.BigEndian.PutUint64(n[:], uint64(len(der)))
		h.Write(n[:])
		h.Write(der)
	}
	var ret CertChainID
	copy(ret[:], h.Sum(nil))
	return ret
}
