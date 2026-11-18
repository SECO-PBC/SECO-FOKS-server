// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package api

import (
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
		s := tok.String()
		got, err := proto.ParseApiToken(s)
		require.NoError(t, err, s)
		require.Equal(t, tok, *got)
	}
	for _, bad := range []string{"", "foksapi_", "foksapi_!!", rnd.String()[len(proto.ApiTokenPrefix):], rnd.String() + "A"} {
		_, err := proto.ParseApiToken(bad)
		require.Error(t, err, bad)
	}
}

func TestBearerToken(t *testing.T) {
	var tok proto.ApiToken
	require.NoError(t, core.RandomFill(tok[:]))
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	for hdr, ok := range map[string]bool{
		"Bearer " + tok.String():     true,
		"BEARER " + tok.String():     true,
		"Bearer  " + tok.String():    true,
		"Basic " + tok.String():      false,
		"Bearer":                     false,
		tok.String():                 false,
		"Bearer " + tok.String()[1:]: false,
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
	require.Len(t, l.buckets, 1)
}

func TestFailGate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newRateLimiter[string](1, 2, func() time.Time { return now })

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
	auth := shared.ApiSessionAuth{ShortHostID: 1, UID: proto.UID{9}}
	var h authCacheKey
	h[5] = 7

	_, ok := c.get(h)
	require.False(t, ok)
	c.put(h, auth, now.Add(time.Hour))
	got, ok := c.get(h)
	require.True(t, ok)
	require.Equal(t, auth.UID, got.UID)

	// Trusted for authCacheTTL at most...
	now = now.Add(authCacheTTL)
	_, ok = c.get(h)
	require.False(t, ok)

	// ...and never past the token's own expiry.
	c.put(h, auth, now.Add(time.Second))
	now = now.Add(time.Second)
	_, ok = c.get(h)
	require.False(t, ok)

	// A token cached for one scope is a different key for another.
	require.NotEqual(t, cacheKeyFor(h, proto.ApiScope_Ping), cacheKeyFor(h, proto.ApiScope_ClientAnalytics))
}

func TestNormMethod(t *testing.T) {
	require.Equal(t, "GET", normMethod("GET"))
	require.Equal(t, "OTHER", normMethod("BREW"))
}
