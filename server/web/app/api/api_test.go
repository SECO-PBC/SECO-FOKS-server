// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// Formatting a token -- or a session holding one -- never yields the secret;
// only Bearer does.
func TestApiTokenRedacted(t *testing.T) {
	tok := proto.ApiToken("eyJhbGciOiJIUzI1NiJ9.e30.c2VjcmV0LXNpZ25hdHVyZQ")
	sess := proto.ApiSession{Token: tok, BaseURL: "https://h"}
	for _, out := range []string{
		tok.String(),
		fmt.Sprint(tok),
		fmt.Sprintf("%#v", tok),
		fmt.Sprintf("%v", sess),
		fmt.Sprintf("%+v", sess),
		fmt.Sprintf("%#v", sess),
	} {
		require.NotContains(t, out, "c2VjcmV0", out)
	}
	require.Equal(t, string(tok), tok.Reveal())
}

func TestBearerToken(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	for hdr, want := range map[string]string{
		"Bearer abc.def.ghi":  "abc.def.ghi",
		"BEARER abc.def.ghi":  "abc.def.ghi",
		"Bearer  abc.def.ghi": "abc.def.ghi",
		"Basic abc.def.ghi":   "",
		"Bearer":              "",
		"Bearer   ":           "",
		"abc.def.ghi":         "",
	} {
		r.Header.Set("Authorization", hdr)
		got, ok := bearerToken(r)
		require.Equal(t, want != "", ok, hdr)
		require.Equal(t, want, got, hdr)
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

func TestNormMethod(t *testing.T) {
	require.Equal(t, "GET", normMethod("GET"))
	require.Equal(t, "OTHER", normMethod("BREW"))
}
