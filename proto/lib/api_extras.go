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

// String encodes the token as it travels in an Authorization header.
func (a ApiToken) String() string {
	return ApiTokenPrefix + B62Encode(a[:])
}

// ParseApiToken decodes a token produced by ApiToken.String.
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

// Hash is what the server stores and looks tokens up by; the token itself is
// never written down.
func (a ApiToken) Hash() []byte {
	h := sha256.Sum256(a[:])
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
