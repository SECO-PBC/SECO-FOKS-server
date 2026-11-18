// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
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
	l := newRateLimiter(2, 3, func() time.Time { return now })
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

func TestNormMethod(t *testing.T) {
	require.Equal(t, "GET", normMethod("GET"))
	require.Equal(t, "OTHER", normMethod("BREW"))
}
