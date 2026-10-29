// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"context"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/keybase/clockwork"
	"github.com/stretchr/testify/require"
)

// newNameCacheTestContext returns a MetaContext with a working soft DB and a
// fake clock, and no active user, so any user-chain load fails at once -- the
// way it does on a device that can't reach its server.
func newNameCacheTestContext(t *testing.T) (MetaContext, clockwork.FakeClock) {
	ctx := context.Background()
	g := NewGlobalContext()
	g.Cfg().TestSetHomeCLIFlag(t.TempDir())
	g.Cfg().TestSetLogTargets("stdout", "stderr")
	g.Cfg().TestSetTestingMode()
	require.NoError(t, g.Configure(ctx))
	clock := clockwork.NewFakeClockAt(time.Now())
	g.SetClock(clock)
	return NewMetaContextTODO(g), clock
}

// pastNameTTL is beyond the longest jittered TTL, so every entry has expired.
const pastNameTTL = nameCacheTTLBase + nameCacheTTLJitter

// TestUsernameLoaderStaleOnError: once a cached name has expired, a load that
// fails returns the stale name rather than the error, from the memory tier
// and, after an agent restart, from the soft DB.
func TestUsernameLoaderStaleOnError(t *testing.T) {
	m, clock := newNameCacheTestContext(t)
	fqu := proto.FQUser{Uid: core.RandomUID(), HostID: core.RandomHostID()}
	arg := LoadUserArg{LoadMode: LoadModeOthers}

	u := NewUsernameLoader()
	require.NoError(t, u.Set(m, fqu, "alice"))
	clock.Advance(pastNameTTL)

	_, ok := u.Get(m, fqu)
	require.False(t, ok, "precondition: the entry has expired")
	_, err := LoadUser(m, LoadUserArg{Uid: fqu.Uid, LoadMode: LoadModeOthers})
	require.Error(t, err, "precondition: a user load fails")

	nm, uw, err := u.Load(m, fqu, arg)
	require.NoError(t, err)
	require.Equal(t, proto.NameUtf8("alice"), nm)
	require.Nil(t, uw)

	// A fresh loader has an empty memory tier, like a restarted agent; the
	// expired soft-DB row still answers.
	nm, _, err = NewUsernameLoader().Load(m, fqu, arg)
	require.NoError(t, err)
	require.Equal(t, proto.NameUtf8("alice"), nm)

	// The fallback doesn't extend the entry: it is still due a refresh.
	_, ok = u.Get(m, fqu)
	require.False(t, ok)
}

// TestUsernameLoaderErrorWithoutCachedName: with nothing cached, a failed load
// still reports its error.
func TestUsernameLoaderErrorWithoutCachedName(t *testing.T) {
	m, _ := newNameCacheTestContext(t)
	fqu := proto.FQUser{Uid: core.RandomUID(), HostID: core.RandomHostID()}
	_, _, err := NewUsernameLoader().Load(m, fqu, LoadUserArg{LoadMode: LoadModeOthers})
	require.Error(t, err)
}
