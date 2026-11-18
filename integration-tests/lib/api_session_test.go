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
	require.Equal(t, proto.ApiScope_Ping, sess.Scope)
	require.NotEmpty(t, sess.BaseURL)
	ttl := time.Until(sess.Expires.Import())
	require.Greater(t, ttl, 23*time.Hour)
	require.Less(t, ttl, 25*time.Hour)

	bearer := "Bearer " + sess.Token.String()

	res := apiPing(t, sess.BaseURL, bearer)
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, "*", res.Header.Get("Access-Control-Allow-Origin"))

	// The scheme is case-insensitive.
	res = apiPing(t, sess.BaseURL, "bearer "+sess.Token.String())
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	// Every refusal is the same 401.
	for _, bad := range []string{
		"",
		"Bearer",
		"Bearer foksapi_",
		"Bearer garbage",
		"Basic " + sess.Token.String(),
	} {
		res = apiPing(t, sess.BaseURL, bad)
		require.Equal(t, http.StatusUnauthorized, res.StatusCode, "auth %q", bad)
		require.Contains(t, res.Header.Get("WWW-Authenticate"), "Bearer")
	}

	// A well-formed token the server never minted.
	var stranger proto.ApiToken
	stranger[0] = 1
	res = apiPing(t, sess.BaseURL, "Bearer "+stranger.String())
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

func TestApiSessionScopeExpiryAndCap(t *testing.T) {
	m := testMetaContext()
	tu := GenerateNewTestUser(t)
	ucli, closeFn := tu.newUserCertAndClient(t, m.Ctx())
	defer closeFn()

	// A token is good only for the scope it was minted with.
	analytics, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_ClientAnalytics)
	require.NoError(t, err)
	res := apiPing(t, analytics.BaseURL, "Bearer "+analytics.Token.String())
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// Unknown scopes are refused at mint time.
	_, err = ucli.NewApiSession(m.Ctx(), proto.ApiScope(99))
	require.Error(t, err)

	db, err := m.G().Db(m.Ctx(), shared.DbTypeUsers)
	require.NoError(t, err)
	defer db.Release()

	// Expired tokens are refused.
	exp, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	res = apiPing(t, exp.BaseURL, "Bearer "+exp.Token.String())
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	_, err = db.Exec(m.Ctx(),
		`UPDATE user_api_sessions SET etime = NOW() - INTERVAL '1 minute' WHERE token_hash = $1`,
		exp.Token.Hash(),
	)
	require.NoError(t, err)
	res = apiPing(t, exp.BaseURL, "Bearer "+exp.Token.String())
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)

	// Minting past the cap retires the oldest live token.
	first, err := ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
	require.NoError(t, err)
	res = apiPing(t, first.BaseURL, "Bearer "+first.Token.String())
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	var last proto.ApiSession
	for range 10 {
		// ctime is NOW() per transaction; keep them strictly ordered.
		time.Sleep(2 * time.Millisecond)
		last, err = ucli.NewApiSession(m.Ctx(), proto.ApiScope_Ping)
		require.NoError(t, err)
	}
	res = apiPing(t, first.BaseURL, "Bearer "+first.Token.String())
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	res = apiPing(t, last.BaseURL, "Bearer "+last.Token.String())
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	var nActive int
	err = db.QueryRow(m.Ctx(),
		`SELECT COUNT(*) FROM user_api_sessions
		 WHERE short_host_id = $1 AND uid = $2 AND active = true`,
		int(m.G().ShortHostID()), tu.uid.ExportToDB(),
	).Scan(&nActive)
	require.NoError(t, err)
	require.Equal(t, 10, nActive)

	// GC removes expired rows and leaves live ones.
	_, err = shared.DeleteExpiredApiSessions(m, time.Hour, 1000000)
	require.NoError(t, err)
	var n int
	err = db.QueryRow(m.Ctx(),
		`SELECT COUNT(*) FROM user_api_sessions WHERE token_hash = $1`,
		exp.Token.Hash(),
	).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 1, n, "expired a minute ago is inside the GC margin")
	_, err = db.Exec(m.Ctx(),
		`UPDATE user_api_sessions SET etime = NOW() - INTERVAL '2 hours' WHERE token_hash = $1`,
		exp.Token.Hash(),
	)
	require.NoError(t, err)
	_, err = shared.DeleteExpiredApiSessions(m, time.Hour, 1000000)
	require.NoError(t, err)
	err = db.QueryRow(m.Ctx(),
		`SELECT COUNT(*) FROM user_api_sessions WHERE token_hash = $1`,
		exp.Token.Hash(),
	).Scan(&n)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	res = apiPing(t, last.BaseURL, "Bearer "+last.Token.String())
	require.Equal(t, http.StatusNoContent, res.StatusCode)
}
