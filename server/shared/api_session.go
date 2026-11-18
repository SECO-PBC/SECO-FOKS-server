// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

// Fork-only (docs/api-sessions.md): bearer tokens for the web server's
// /api/v1 HTTPS surface.
//
// A logged-in client mints a token over the authenticated User RPC
// (newApiSession) and sends it as `Authorization: Bearer <token>`. The token
// is a JWT (HS256) signed with one of the host's challenge HMAC keys; the web
// server verifies it with golang-jwt and keeps no per-token state.
//
// FOR NON-SENSITIVE ENDPOINTS ONLY (analytics, health). By design a token
// cannot be revoked: it stays valid until it expires, even if the device that
// minted it is revoked or the account is deleted. See the doc for the full
// list of what this does not protect against.

const (
	// HmacKeyApiSession signs /api/v1 tokens. Created on a host's first mint
	// (ensureApiSessionKey), so existing deployments need no migration.
	HmacKeyApiSession HmacKeyType = "api_session"

	apiTokenIssuer   = "foks"
	apiTokenAudience = "foks-api"

	// apiTokenLeeway tolerates clock skew between the user server that
	// signed a token and the web server that checks it.
	apiTokenLeeway = 30 * time.Second
)

// ApiSessionNotFoundError is returned for every reason a bearer token is
// refused (malformed, forged, expired, wrong scope, unknown key), so the HTTP
// layer cannot leak which.
type ApiSessionNotFoundError struct{}

func (ApiSessionNotFoundError) Error() string { return "API session not found" }

// ApiSessionAuth is who a valid bearer token speaks for.
type ApiSessionAuth struct {
	ShortHostID core.ShortHostID
	UID         proto.UID
}

type apiClaims struct {
	Scope string `json:"scope"`
	Host  int    `json:"hst"`
	jwt.RegisteredClaims
}

// NewApiSession mints a bearer token for the user and host in m, good for
// scope until the configured api_session_duration elapses.
func NewApiSession(
	m MetaContext,
	scope proto.ApiScope,
) (
	*proto.ApiSession,
	error,
) {
	if !scope.IsValid() {
		return nil, core.BadArgsError("unknown API scope")
	}
	uid := m.UID()
	if uid.IsZero() {
		return nil, core.AuthError{}
	}
	wcfg, err := m.G().Config().WebConfig(m.Ctx())
	if err != nil {
		return nil, err
	}
	baseURL, err := adminBaseURL(m, wcfg)
	if err != nil {
		return nil, err
	}
	dur, err := wcfg.ApiSessionDuration()
	if err != nil {
		return nil, err
	}
	sub, err := uid.StringErr()
	if err != nil {
		return nil, err
	}
	keyID, key, err := ensureApiSessionKey(m)
	if err != nil {
		return nil, err
	}
	now := m.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, apiClaims{
		Scope: scope.String(),
		Host:  int(m.ShortHostID()),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    apiTokenIssuer,
			Subject:   sub,
			Audience:  jwt.ClaimStrings{apiTokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(dur)),
		},
	})
	tok.Header["kid"] = hex.EncodeToString(keyID[:])
	signed, err := tok.SignedString(key[:])
	if err != nil {
		return nil, err
	}
	return &proto.ApiSession{
		Token:    proto.ApiToken(signed),
		BaseURL:  baseURL,
		Lifetime: proto.ExportDurationMilli(dur),
	}, nil
}

// ensureApiSessionKey returns the host's newest api_session key, creating the
// first one if the host has none. Two first mints racing each create a key;
// both stay valid, since tokens name their key.
func ensureApiSessionKey(m MetaContext) (*proto.HMACKeyID, *proto.HMACKey, error) {
	id, key, err := LookupLatestChallengeKey(m, HmacKeyApiSession)
	if errors.As(err, new(core.BadServerDataError)) {
		err = GenerateSomeNewChallengeHMACKeys(m, []HmacKeyType{HmacKeyApiSession})
		if err != nil {
			return nil, nil, err
		}
		id, key, err = LookupLatestChallengeKey(m, HmacKeyApiSession)
	}
	return id, key, err
}

type apiKeyRef struct {
	host core.ShortHostID
	id   proto.HMACKeyID
}

// ApiTokenVerifier checks /api/v1 tokens. Keys are immutable once created, so
// each one is read from the database once and then cached for the life of
// the process. Lookups of keys not yet cached share one small global budget:
// key IDs come from the (unverified) token, so without it a flood of tokens
// naming made-up keys would cost a query each.
type ApiTokenVerifier struct {
	sync.Mutex
	g       *GlobalContext
	keys    map[apiKeyRef]proto.HMACKey
	lookups *rate.Limiter
}

func NewApiTokenVerifier(g *GlobalContext) *ApiTokenVerifier {
	return &ApiTokenVerifier{
		g:       g,
		keys:    make(map[apiKeyRef]proto.HMACKey),
		lookups: rate.NewLimiter(1, 10),
	}
}

func (v *ApiTokenVerifier) key(m MetaContext, ref apiKeyRef) (*proto.HMACKey, error) {
	v.Lock()
	key, ok := v.keys[ref]
	v.Unlock()
	if ok {
		return &key, nil
	}
	if !v.lookups.Allow() {
		return nil, ApiSessionNotFoundError{}
	}
	mh, err := m.WithShortHostID(ref.host)
	if err != nil {
		return nil, ApiSessionNotFoundError{}
	}
	db, err := mh.Db(DbTypeUsers)
	if err != nil {
		return nil, err
	}
	defer db.Release()
	k, err := LookupHMACKeyByID(mh, db, ref.id, HmacKeyApiSession)
	if errors.As(err, new(core.KeyNotFoundError)) {
		return nil, ApiSessionNotFoundError{}
	}
	if err != nil {
		return nil, err
	}
	v.Lock()
	v.keys[ref] = *k
	v.Unlock()
	return k, nil
}

// Verify resolves a bearer token to its user, provided it is correctly signed
// by a key of the host it names, unexpired, and minted for scope. Any failure
// to match is ApiSessionNotFoundError; other errors are server-side (e.g. the
// database) and the caller should answer 5xx.
func (v *ApiTokenVerifier) Verify(
	m MetaContext,
	raw string,
	scope proto.ApiScope,
) (
	*ApiSessionAuth,
	error,
) {
	var serverErr error
	var claims apiClaims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			b, err := hex.DecodeString(kid)
			var ref apiKeyRef
			if err != nil || len(b) != len(ref.id) || claims.Host <= 0 {
				return nil, ApiSessionNotFoundError{}
			}
			copy(ref.id[:], b)
			ref.host = core.ShortHostID(claims.Host)
			key, err := v.key(m, ref)
			if err != nil {
				if !errors.As(err, new(ApiSessionNotFoundError)) {
					serverErr = err
				}
				return nil, err
			}
			return key[:], nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(apiTokenIssuer),
		jwt.WithAudience(apiTokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(apiTokenLeeway),
		jwt.WithTimeFunc(m.Now),
	)
	if serverErr != nil {
		return nil, serverErr
	}
	if err != nil || claims.Scope != scope.String() {
		return nil, ApiSessionNotFoundError{}
	}
	eid, err := proto.ImportEntityIDFromString(claims.Subject)
	if err != nil {
		return nil, ApiSessionNotFoundError{}
	}
	uid, err := eid.ToUID()
	if err != nil {
		return nil, ApiSessionNotFoundError{}
	}
	return &ApiSessionAuth{
		ShortHostID: core.ShortHostID(claims.Host),
		UID:         uid,
	}, nil
}
