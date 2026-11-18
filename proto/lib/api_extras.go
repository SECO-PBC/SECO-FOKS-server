// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

// Fork-only (docs/api-sessions.md): helpers for /api/v1 bearer tokens.

// String is REDACTED. fmt, zap and error wrapping call String implicitly, so
// it must never yield the credential: printing a token, or an ApiSession
// with %+v, shows only that a token was there. Use Reveal for the real value.
func (a ApiToken) String() string {
	return "<redacted API token>"
}

// GoString redacts %#v too.
func (a ApiToken) GoString() string {
	return a.String()
}

// Reveal is the token as it travels in an Authorization header. This is the
// secret: call it only to hand the token to its owner or to verify it.
func (a ApiToken) Reveal() string {
	return string(a)
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
