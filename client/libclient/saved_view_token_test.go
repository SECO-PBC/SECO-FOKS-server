// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"context"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// The key must come from the values, not their addresses: a member role
// holds its viz level behind a pointer, and formatting it with %v printed the
// pointer, so every process built a different key and never found its own
// saved token. The key generation is not part of the key, so a mint at a
// newer generation replaces the row; the host is.
func TestSavedViewTokenKeyIsByValue(t *testing.T) {
	var tm proto.FQTeam
	member := proto.FQParty{}
	a := proto.NewRoleWithMember(proto.VizLevel(5))
	b := proto.NewRoleWithMember(proto.VizLevel(5))
	c := proto.NewRoleWithMember(proto.VizLevel(6))
	otherHost := tm
	otherHost.Host[0] = 1

	ka, err := savedViewTokenKey(tm, member, a)
	require.NoError(t, err)
	kb, err := savedViewTokenKey(tm, member, b)
	require.NoError(t, err)
	kc, err := savedViewTokenKey(tm, member, c)
	require.NoError(t, err)
	kh, err := savedViewTokenKey(otherHost, member, a)
	require.NoError(t, err)

	require.Equal(t, ka, kb, "equal roles, equal keys")
	require.NotEqual(t, ka, kc, "another viz level")
	require.NotEqual(t, ka, kh, "the same team ID on another host")
}

// usableFor checks every field the token was minted for, and the mint time
// against the window in both directions.
func TestMintedViewTokenUsableFor(t *testing.T) {
	now := time.Now()
	role := proto.NewRoleWithMember(proto.VizLevel(5))
	v := &mintedViewToken{gen: 3, role: role, at: now.Add(-time.Minute)}
	ok := func(v *mintedViewToken, team proto.FQTeam, gen proto.Generation, member proto.FQParty, r proto.Role) bool {
		return v.usableFor(team, gen, member, r, now)
	}
	var team proto.FQTeam
	team.Team[1] = 7
	var fqu proto.FQUser
	fqu.Uid[1] = 9
	member := fqu.FQParty()
	v.team, v.member = team, member
	require.True(t, ok(v, team, 3, member, role))
	require.False(t, ok(nil, team, 3, member, role), "no token")
	require.False(t, ok(v, team, 4, member, role), "another generation")
	require.False(t, ok(v, team, 3, member, proto.NewRoleWithMember(proto.VizLevel(6))), "another role")
	otherTeam := team
	otherTeam.Host[0] = 1
	require.False(t, ok(v, otherTeam, 3, member, role), "another team host")
	otherMember := member
	otherMember.Host[0] = 1
	require.False(t, ok(v, team, 3, otherMember, role), "another member")

	old := *v
	old.at = now.Add(-voTokReuseWindow)
	require.False(t, ok(&old, team, 3, member, role), "at the window")
	future := *v
	future.at = now.Add(time.Minute)
	require.False(t, ok(&future, team, 3, member, role), "minted in the future (the clock moved back)")
}

type savedTokFixture struct {
	g     *GlobalContext
	m     MetaContext
	alice *UserContext
	bob   *UserContext
}

func newSavedTokFixture(t *testing.T) *savedTokFixture {
	ctx := context.Background()
	g := NewGlobalContext()
	g.Cfg().TestSetHomeCLIFlag(t.TempDir())
	g.Cfg().TestSetLogTargets("stdout", "stderr")
	g.Cfg().TestSetTestingMode()
	require.NoError(t, g.Configure(ctx))

	user := func(b byte) *UserContext {
		var fqu proto.FQUser
		fqu.Uid[1] = b
		u := &UserContext{Info: proto.UserInfo{Fqu: fqu}}
		var seed proto.SecretSeed32
		seed[0] = b
		puk, err := core.NewSharedPrivateSuite25519(proto.EntityType_User, proto.OwnerRole, seed, 1, fqu.HostID)
		require.NoError(t, err)
		u.PrivKeys.SetPUKs(NewPUKSet([]core.SharedPrivateSuiter{puk}, fqu.HostID))
		return u
	}
	return &savedTokFixture{g: g, m: NewMetaContextTODO(g), alice: user(1), bob: user(2)}
}

func (f *savedTokFixture) loaderFor(u *UserContext) *TeamLoader {
	var team proto.FQTeam
	team.Team[1] = 7
	return NewTeamLoader(u, LoadTeamArg{
		Team:    team,
		As:      u.Info.Fqu.FQParty(),
		SrcRole: proto.OwnerRole,
	})
}

func (f *savedTokFixture) mint(l *TeamLoader) *mintedViewToken {
	return &mintedViewToken{
		tok:    rem.TeamVOBearerToken{1, 2, 3},
		team:   l.Arg.Team,
		gen:    1,
		member: l.Arg.As,
		role:   l.Arg.SrcRole,
		at:     f.m.G().Now(),
	}
}

func (f *savedTokFixture) savedUnder(t *testing.T, u *UserContext, l *TeamLoader) bool {
	key, err := savedViewTokenKey(l.Arg.Team, l.Arg.As, l.Arg.SrcRole)
	require.NoError(t, err)
	fqu := u.FQU()
	var saved lcl.SavedVOBearerToken
	_, err = f.m.DbGet(&saved, DbTypeSoft, &fqu, lcl.DataType_SavedVOBearerToken, key)
	if err != nil {
		require.ErrorIs(t, err, core.RowNotFoundError{})
		return false
	}
	return true
}

// A loader saves under its own user, not whoever is active: after a switch
// to bob, alice's still-running loader must not put her token in bob's scope,
// where forgetting alice's tokens (her revoke or lock) would miss it.
func TestSavedViewTokenScopedToLoaderUser(t *testing.T) {
	f := newSavedTokFixture(t)
	f.g.userMu.Lock()
	f.g.curr = f.bob
	f.g.userMu.Unlock()

	l := f.loaderFor(f.alice)
	l.saveViewToken(f.m, f.mint(l))
	require.True(t, f.savedUnder(t, f.alice, l), "under the loader's user")
	require.False(t, f.savedUnder(t, f.bob, l), "not under the active user")

	require.NoError(t, ForgetSavedViewTokens(f.m, f.alice.FQU()))
	require.False(t, f.savedUnder(t, f.alice, l), "forgetting alice's tokens drops it")
}

// A mint that completes after the user locked (ClearSecrets, then
// ForgetSavedViewTokens) must not write its token back to disk.
func TestSavedViewTokenNotSavedWhileLocked(t *testing.T) {
	f := newSavedTokFixture(t)
	l := f.loaderFor(f.alice)
	v := f.mint(l)

	l.saveViewToken(f.m, v)
	require.True(t, f.savedUnder(t, f.alice, l))

	require.NoError(t, f.alice.ClearSecrets())
	require.NoError(t, ForgetSavedViewTokens(f.m, f.alice.FQU()))
	l.saveViewToken(f.m, v)
	require.False(t, f.savedUnder(t, f.alice, l), "a locked user's mint is not saved")
}

// Adoption takes a saved token that matches; a refused one is forgotten;
// rows past the window are pruned on the next save.
func TestSavedViewTokenAdoptForgetPrune(t *testing.T) {
	f := newSavedTokFixture(t)
	l := f.loaderFor(f.alice)
	v := f.mint(l)
	l.saveViewToken(f.m, v)

	fresh := f.loaderFor(f.alice)
	fresh.adoptSavedViewToken(f.m, 1)
	require.NotNil(t, fresh.voTok)
	require.Equal(t, v.tok, fresh.voTok.tok)

	other := f.loaderFor(f.alice)
	other.adoptSavedViewToken(f.m, 2)
	require.Nil(t, other.voTok, "minted at another generation")

	fresh.forgetSavedViewToken(f.m)
	require.False(t, f.savedUnder(t, f.alice, l), "forgotten after a refusal")

	// Pruning is by the row's write time (wall clock).
	l.saveViewToken(f.m, v)
	fqu := f.alice.FQU()
	require.NoError(t, f.m.DbDeleteOfTypeModifiedBefore(DbTypeSoft, &fqu, lcl.DataType_SavedVOBearerToken,
		proto.ExportTime(time.Now().Add(-time.Minute))))
	require.True(t, f.savedUnder(t, f.alice, l), "a fresh row survives")
	require.NoError(t, f.m.DbDeleteOfTypeModifiedBefore(DbTypeSoft, &fqu, lcl.DataType_SavedVOBearerToken,
		proto.ExportTime(time.Now().Add(time.Minute))))
	require.False(t, f.savedUnder(t, f.alice, l), "an old row is pruned")
}
