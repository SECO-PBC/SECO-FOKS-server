// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

// Fork-only (docs/api-sessions.md): /api/v1 bearer tokens, end to end -- mint
// over the User RPC, then call the web server with them.

import (
	"encoding/hex"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// testBase rewrites a minted baseURL's test hostname (e.g. base-xyz.foks) to
// the address it CNAMEs to, as the CLI web tests do.
func testBase(t *testing.T, base proto.URLString) string {
	u, err := url.Parse(string(base))
	require.NoError(t, err)
	host := G.CnameResolver().Resolve(proto.Hostname(u.Hostname()))
	u.Host = string(host) + ":" + u.Port()
	return u.String()
}

func apiPing(t *testing.T, base proto.URLString, auth string) *http.Response {
	req, err := http.NewRequest(http.MethodGet, testBase(t, base)+"/api/v1/ping", nil)
	require.NoError(t, err)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	res.Body.Close()
	return res
}

// forge signs claims the way the server does, with a key the test picks, so
// the test can probe every way a token can be wrong.
type forgeArgs struct {
	method jwt.SigningMethod
	key    any
	kid    string
	claims jwt.MapClaims
}

func forge(t *testing.T, a forgeArgs) string {
	tok := jwt.NewWithClaims(a.method, a.claims)
	if a.kid != "" {
		tok.Header["kid"] = a.kid
	}
	s, err := tok.SignedString(a.key)
	require.NoError(t, err)
	return s
}

func TestApiSessionPing(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	sess, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	require.NotEmpty(t, sess.BaseURL)
	require.Equal(t, time.Hour, sess.Lifetime.Import())

	bearer := "Bearer " + sess.Token.Reveal()

	res := apiPing(t, sess.BaseURL, bearer)
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, "*", res.Header.Get("Access-Control-Allow-Origin"))

	// Every refusal is the same 401.
	for _, bad := range []string{
		"",
		"Bearer",
		"Bearer garbage",
		"Bearer a.b.c",
		"Basic " + sess.Token.Reveal(),
	} {
		res = apiPing(t, sess.BaseURL, bad)
		require.Equal(t, http.StatusUnauthorized, res.StatusCode, "auth %q", bad)
		require.Contains(t, res.Header.Get("WWW-Authenticate"), "Bearer")
	}

	// An admin-panel web session is not an API token.
	adminURL, err := ucli.NewWebAdminPanelURL(m.Ctx())
	require.NoError(t, err)
	u, err := url.Parse(string(adminURL))
	require.NoError(t, err)
	webSess := u.Query().Get("s")
	require.NotEmpty(t, webSess)
	res = apiPing(t, sess.BaseURL, "Bearer "+webSess)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// Unknown routes are JSON 404s, not the HTML app.
	res, err = http.Get(testBase(t, sess.BaseURL) + "/api/v1/nope")
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	require.Equal(t, "application/json", res.Header.Get("Content-Type"))

	// CORS preflight needs no auth.
	req, err := http.NewRequest(http.MethodOptions, testBase(t, sess.BaseURL)+"/api/v1/ping", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	res, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, "*", res.Header.Get("Access-Control-Allow-Origin"))
	require.Contains(t, res.Header.Get("Access-Control-Allow-Headers"), "Authorization")
	require.Contains(t, res.Header.Get("Access-Control-Expose-Headers"), "Retry-After")

	// Unknown scopes are refused at mint time.
	_, err = ucli.NewApiSession(m.Ctx(), proto.ApiScope(99))
	require.Error(t, err)
}

func TestApiSessionRejectsBadTokens(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	// A real mint first, so the host has an api_session key.
	sess, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	res := apiPing(t, sess.BaseURL, "Bearer "+sess.Token.Reveal())
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	keyID, key, err := shared.LookupLatestChallengeKey(m, shared.HmacKeyApiSession)
	require.NoError(t, err)
	kid := hex.EncodeToString(keyID[:])
	sub, err := tu.uid.StringErr()
	require.NoError(t, err)
	now := time.Now()
	good := func() jwt.MapClaims {
		return jwt.MapClaims{
			"iss":   "foks",
			"aud":   []string{"foks-api"},
			"sub":   sub,
			"scope": "Ping",
			"hst":   int(m.G().ShortHostID()),
			"iat":   now.Unix(),
			"exp":   now.Add(time.Hour).Unix(),
		}
	}
	with := func(k string, v any) jwt.MapClaims {
		c := good()
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	ping := func(tok string) int {
		return apiPing(t, sess.BaseURL, "Bearer "+tok).StatusCode
	}

	// The test's own signing matches the server's: a correct forgery passes.
	require.Equal(t, http.StatusNoContent, ping(forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, good()})))

	var otherKey [32]byte
	require.NoError(t, core.RandomFill(otherKey[:]))
	var otherKid [16]byte
	require.NoError(t, core.RandomFill(otherKid[:]))
	real := sess.Token.Reveal()

	for name, tok := range map[string]string{
		"wrong key":           forge(t, forgeArgs{jwt.SigningMethodHS256, otherKey[:], kid, good()}),
		"unknown kid":         forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], hex.EncodeToString(otherKid[:]), good()}),
		"no kid":              forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], "", good()}),
		"alg none":            forge(t, forgeArgs{jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, kid, good()}),
		"HS512":               forge(t, forgeArgs{jwt.SigningMethodHS512, key[:], kid, good()}),
		"expired":             forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("exp", now.Add(-time.Minute).Unix())}),
		"no expiry":           forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("exp", nil)}),
		"other scope":         forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("scope", "ClientAnalytics")}),
		"other audience":      forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("aud", []string{"elsewhere"})}),
		"other issuer":        forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("iss", "elsewhere")}),
		"host not its key's":  forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("hst", int(m.G().ShortHostID())+1000)}),
		"subject not a uid":   forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("sub", "nobody")}),
		"tampered after sign": real[:len(real)-4] + "AAAA",
	} {
		require.Equal(t, http.StatusUnauthorized, ping(tok), name)
	}

	// Inside the 30s leeway for clock skew between servers, still good.
	require.Equal(t, http.StatusNoContent, ping(forge(t, forgeArgs{jwt.SigningMethodHS256, key[:], kid, with("exp", now.Add(-10*time.Second).Unix())})))
}
