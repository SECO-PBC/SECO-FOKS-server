// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

func TestApiTokenRoundTrip(t *testing.T) {
	var zero proto.ApiToken
	var lead proto.ApiToken
	lead[31] = 0xff
	var rnd proto.ApiToken
	require.NoError(t, core.RandomFill(rnd[:]))
	for _, tok := range []proto.ApiToken{zero, lead, rnd} {
		s := tok.Bearer()
		got, err := proto.ParseApiToken(s)
		require.NoError(t, err, s)
		require.Equal(t, tok, *got)
	}
	for _, bad := range []string{"", "foksapi_", "foksapi_!!", rnd.Bearer()[len(proto.ApiTokenPrefix):], rnd.Bearer() + "A"} {
		_, err := proto.ParseApiToken(bad)
		require.Error(t, err, bad)
	}
}

// Formatting a token -- or a session holding one -- never yields the secret;
// only Bearer does.
func TestApiTokenRedacted(t *testing.T) {
	var tok proto.ApiToken
	require.NoError(t, core.RandomFill(tok[:]))
	secret := tok.Bearer()[len(proto.ApiTokenPrefix):]
	sess := proto.ApiSession{Token: tok, BaseURL: "https://h"}
	for _, out := range []string{
		tok.String(),
		fmt.Sprintf("%v", tok),
		fmt.Sprint(tok),
		fmt.Sprintf("%#v", tok),
		fmt.Sprintf("%v", sess),
		fmt.Sprintf("%+v", sess),
		fmt.Sprintf("%#v", sess),
	} {
		require.NotContains(t, out, secret, out)
	}
}

func TestBearerToken(t *testing.T) {
	var tok proto.ApiToken
	require.NoError(t, core.RandomFill(tok[:]))
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	for hdr, ok := range map[string]bool{
		"Bearer " + tok.Bearer():     true,
		"BEARER " + tok.Bearer():     true,
		"Bearer  " + tok.Bearer():    true,
		"Basic " + tok.Bearer():      false,
		"Bearer":                     false,
		tok.Bearer():                 false,
		"Bearer " + tok.Bearer()[1:]: false,
	} {
		r.Header.Set("Authorization", hdr)
		got, gotOK := bearerToken(r)
		require.Equal(t, ok, gotOK, hdr)
		if ok {
			require.Equal(t, tok, *got)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newRateLimiter[rateKey](2, 3, func() time.Time { return now })
	a := rateKey{shid: 1, uid: proto.UID{1}}
	b := rateKey{shid: 1, uid: proto.UID{2}}

	for range 3 {
		_, ok := l.allow(a)
		require.True(t, ok)
	}
	wait, ok := l.allow(a)
	require.False(t, ok)
	require.Equal(t, 500*time.Millisecond, wait)

	// Buckets are per user.
	_, ok = l.allow(b)
	require.True(t, ok)

	now = now.Add(500 * time.Millisecond)
	_, ok = l.allow(a)
	require.True(t, ok)
	_, ok = l.allow(a)
	require.False(t, ok)

	// Idle buckets are swept; a swept key starts full again.
	now = now.Add(limiterIdle + time.Minute)
	_, ok = l.allow(b)
	require.True(t, ok)
	require.Len(t, l.entries, 1)
}

func TestFailGate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newRateLimiter[clientAddr](1, 2, func() time.Time { return now })

	// check never spends; only refusals (charge) do.
	for range 5 {
		_, ok := l.check("1.2.3.4")
		require.True(t, ok)
	}
	l.charge("1.2.3.4")
	l.charge("1.2.3.4")
	wait, ok := l.check("1.2.3.4")
	require.False(t, ok)
	require.Equal(t, time.Second, wait)
	_, ok = l.check("5.6.7.8")
	require.True(t, ok)

	// Failing while blocked digs deeper, down to -burst (-2 here): it takes
	// three seconds, not one, to earn a token back.
	for range 10 {
		l.charge("1.2.3.4")
	}
	now = now.Add(2 * time.Second)
	_, ok = l.check("1.2.3.4")
	require.False(t, ok)
	now = now.Add(time.Second)
	_, ok = l.check("1.2.3.4")
	require.True(t, ok)
}

func TestAuthCache(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newAuthCache(func() time.Time { return now })
	auth := shared.ApiSessionAuth{ShortHostID: 1, UID: proto.UID{9}, Expires: now.Add(time.Hour)}
	var tok proto.ApiToken
	tok[5] = 7
	k := authCacheKey{hash: tok.Hash(), scope: proto.ApiScope_Ping}

	_, ok := c.get(k)
	require.False(t, ok)
	c.put(k, auth)
	got, ok := c.get(k)
	require.True(t, ok)
	require.Equal(t, auth.UID, got.UID)

	// The same token checked for another scope is another key.
	_, ok = c.get(authCacheKey{hash: tok.Hash(), scope: proto.ApiScope_ClientAnalytics})
	require.False(t, ok)

	// Trusted for authCacheTTL at most...
	now = now.Add(authCacheTTL)
	_, ok = c.get(k)
	require.False(t, ok)

	// ...and never past the token's own expiry.
	auth.Expires = now.Add(time.Second)
	c.put(k, auth)
	now = now.Add(time.Second)
	_, ok = c.get(k)
	require.False(t, ok)
}

func TestNormMethod(t *testing.T) {
	require.Equal(t, "GET", normMethod("GET"))
	require.Equal(t, "OTHER", normMethod("BREW"))
}
