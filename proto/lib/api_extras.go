// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"crypto/sha256"
	"errors"
	"strings"
)

// Fork-only (docs/api-sessions.md): helpers for /api/v1 bearer tokens.

// ApiTokenPrefix marks an encoded ApiToken, so a token is recognisable in a
// leaked config or a secret scanner, and a WebSession string is never
// mistaken for one.
const ApiTokenPrefix = "foksapi_"

var ErrBadApiToken = errors.New("malformed API token")

// String is REDACTED. fmt, zap and error wrapping call String implicitly, so
// it must never yield the credential: printing a token, or an ApiSession
// with %+v, shows only that a token was there. Use Bearer for the real value.
func (a ApiToken) String() string {
	return ApiTokenPrefix + "<redacted>"
}

// GoString redacts %#v too.
func (a ApiToken) GoString() string {
	return a.String()
}

// Bearer encodes the token as it travels in an Authorization header. This
// is the secret: call it only to hand the token to its owner.
func (a ApiToken) Bearer() string {
	return ApiTokenPrefix + B62Encode(a[:])
}

// ParseApiToken decodes a token produced by ApiToken.Bearer.
func ParseApiToken(s string) (*ApiToken, error) {
	rest, ok := strings.CutPrefix(s, ApiTokenPrefix)
	if !ok {
		return nil, ErrBadApiToken
	}
	raw, err := B62Decode(rest)
	if err != nil {
		return nil, ErrBadApiToken
	}
	var ret ApiToken
	if len(raw) != len(ret) {
		return nil, ErrBadApiToken
	}
	copy(ret[:], raw)
	return &ret, nil
}

// ApiTokenHash is the SHA-256 of an ApiToken: what the server stores and looks
// tokens up by. A distinct type, so a token can't be passed where its hash is
// expected (cache keys, the helpers that take a hash). SQL parameters are
// `any` and can't be protected this way: always pass Hash().ExportToDB().
type ApiTokenHash [sha256.Size]byte

func (a ApiToken) Hash() ApiTokenHash {
	return sha256.Sum256(a[:])
}

func (h ApiTokenHash) ExportToDB() []byte {
	return h[:]
}

func (s ApiScope) String() string {
	if r, ok := ApiScopeRevMap[s]; ok {
		return r
	}
	return "unknown"
}

// IsValid reports whether s is a scope this build knows.
func (s ApiScope) IsValid() bool {
	_, ok := ApiScopeRevMap[s]
	return ok
}
