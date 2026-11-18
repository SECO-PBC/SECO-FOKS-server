// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

// Fork-only (docs/api-sessions.md): /api/v1 bearer tokens, end to end -- mint
// over the User RPC, then call the web server with them.

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
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

func TestApiSessionPing(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	sess, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	require.NotEmpty(t, sess.BaseURL)
	require.Equal(t, 24*time.Hour, sess.Lifetime.Import())

	bearer := "Bearer " + sess.Token.Bearer()

	res := apiPing(t, sess.BaseURL, bearer)
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, "*", res.Header.Get("Access-Control-Allow-Origin"))

	// The scheme is case-insensitive.
	res = apiPing(t, sess.BaseURL, "bearer "+sess.Token.Bearer())
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	// Every refusal is the same 401.
	for _, bad := range []string{
		"",
		"Bearer",
		"Bearer foksapi_",
		"Bearer garbage",
		"Basic " + sess.Token.Bearer(),
	} {
		res = apiPing(t, sess.BaseURL, bad)
		require.Equal(t, http.StatusUnauthorized, res.StatusCode, "auth %q", bad)
		require.Contains(t, res.Header.Get("WWW-Authenticate"), "Bearer")
	}

	// A well-formed token the server never minted.
	var stranger proto.ApiToken
	stranger[0] = 1
	res = apiPing(t, sess.BaseURL, "Bearer "+stranger.Bearer())
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// An admin-panel web session is not an API token.
	adminURL, err := ucli.NewWebAdminPanelURL(m.Ctx())
	require.NoError(t, err)
	u, err := url.Parse(string(adminURL))
	require.NoError(t, err)
	webSess := u.Query().Get("s")
	require.NotEmpty(t, webSess)
	res = apiPing(t, sess.BaseURL, "Bearer "+webSess)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	res = apiPing(t, sess.BaseURL, "Bearer foksapi_"+webSess)
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
}

// backdateApiSessions moves the user's tokens a couple of minutes into the
// past, so the mint throttle (5 per scope per minute) doesn't trip while a
// test mints a batch; their relative order is kept.
func backdateApiSessions(t *testing.T, m shared.MetaContext, uid proto.UID) {
	db, err := m.G().Db(m.Ctx(), shared.DbTypeUsers)
	require.NoError(t, err)
	defer db.Release()
	_, err = db.Exec(m.Ctx(),
		`UPDATE user_api_sessions SET ctime = ctime - INTERVAL '2 minutes'
		 WHERE short_host_id = $1 AND uid = $2`,
		int(m.G().ShortHostID()), uid.ExportToDB(),
	)
	require.NoError(t, err)
}

func TestApiSessionScopeExpiryAndCap(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	db, err := m.G().Db(m.Ctx(), shared.DbTypeUsers)
	require.NoError(t, err)
	defer db.Release()
	rowExists := func(tok proto.ApiToken) bool {
		var n int
		err := db.QueryRow(m.Ctx(),
			`SELECT COUNT(*) FROM user_api_sessions WHERE token_hash = $1`,
			tok.Hash().ExportToDB(),
		).Scan(&n)
		require.NoError(t, err)
		return n == 1
	}

	// A token is good only for the scope it was minted with.
	analytics, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_ClientAnalytics)
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, analytics.Lifetime.Import())
	res := apiPing(t, analytics.BaseURL, "Bearer "+analytics.Token.Bearer())
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// Unknown scopes are refused at mint time.
	_, err = ucli.NewApiSession(m.Ctx(), proto.ApiScope(99))
	require.Error(t, err)

	// Expired tokens are refused. (Not pinged before expiring: a verified
	// token is cached for up to 30s, by design.)
	exp, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	_, err = db.Exec(m.Ctx(),
		`UPDATE user_api_sessions SET etime = NOW() - INTERVAL '1 minute' WHERE token_hash = $1`,
		exp.Token.Hash().ExportToDB(),
	)
	require.NoError(t, err)
	res = apiPing(t, exp.BaseURL, "Bearer "+exp.Token.Bearer())
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// Minting past the per-scope cap deletes the oldest others; the other
	// scope's token is untouched.
	backdateApiSessions(t, m, tu.uid)
	first, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	var last proto.ApiSession
	for i := range 10 {
		if i%4 == 0 {
			backdateApiSessions(t, m, tu.uid)
		}
		time.Sleep(2 * time.Millisecond)
		last, err = ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
		require.NoError(t, err)
	}
	require.False(t, rowExists(first.Token))
	require.False(t, rowExists(exp.Token))
	require.True(t, rowExists(analytics.Token))
	res = apiPing(t, first.BaseURL, "Bearer "+first.Token.Bearer())
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	res = apiPing(t, last.BaseURL, "Bearer "+last.Token.Bearer())
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	var nPing int
	err = db.QueryRow(m.Ctx(),
		`SELECT COUNT(*) FROM user_api_sessions
		 WHERE short_host_id = $1 AND uid = $2 AND scope = $3`,
		int(m.G().ShortHostID()), tu.uid.ExportToDB(), int(proto.ApiScope_Ping),
	).Scan(&nPing)
	require.NoError(t, err)
	require.Equal(t, 10, nPing)

	// GC removes rows past etime + margin, and leaves the rest.
	_, err = db.Exec(m.Ctx(),
		`UPDATE user_api_sessions SET etime = NOW() - INTERVAL '1 minute' WHERE token_hash = $1`,
		analytics.Token.Hash().ExportToDB(),
	)
	require.NoError(t, err)
	_, err = shared.DeleteExpiredApiSessions(m, time.Hour, 1000000)
	require.NoError(t, err)
	require.True(t, rowExists(analytics.Token), "expired a minute ago is inside the GC margin")
	_, err = db.Exec(m.Ctx(),
		`UPDATE user_api_sessions SET etime = NOW() - INTERVAL '2 hours' WHERE token_hash = $1`,
		analytics.Token.Hash().ExportToDB(),
	)
	require.NoError(t, err)
	_, err = shared.DeleteExpiredApiSessions(m, time.Hour, 1000000)
	require.NoError(t, err)
	require.False(t, rowExists(analytics.Token))
	require.True(t, rowExists(last.Token))
}

func TestApiSessionMintThrottle(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	for range 5 {
		_, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
		require.NoError(t, err)
	}
	_, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.Error(t, err)
	require.Equal(t, core.RateLimitError{}, err)

	// The throttle is per scope.
	_, err = ucli.NewApiSession(m.Ctx(), proto.ApiScope_ClientAnalytics)
	require.NoError(t, err)

	// And it lifts once the minute has passed.
	backdateApiSessions(t, m, tu.uid)
	_, err = ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
}

// Runs last among the API tests: it exhausts 127.0.0.1's budget of refused
// tokens, which would turn later uncached lookups from this IP into 429s.
func TestApiSessionZFailGate(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	good, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	bearer := "Bearer " + good.Token.Bearer()
	res := apiPing(t, good.BaseURL, bearer)
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	var stranger proto.ApiToken
	stranger[1] = 2
	got429 := false
	for range 40 {
		res = apiPing(t, good.BaseURL, "Bearer "+stranger.Bearer())
		if res.StatusCode == http.StatusTooManyRequests {
			got429 = true
			require.NotEmpty(t, res.Header.Get("Retry-After"))
			require.Contains(t, res.Header.Get("Access-Control-Expose-Headers"), "Retry-After")
			break
		}
		require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	}
	require.True(t, got429, "a flood of refused tokens is throttled before the DB")

	// A token verified moments ago is served from the cache, so it still
	// works while its IP is blocked for unknown tokens.
	res = apiPing(t, good.BaseURL, bearer)
	require.Equal(t, http.StatusNoContent, res.StatusCode)
}
