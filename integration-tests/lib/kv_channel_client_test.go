// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// TestKvChannelClientEndToEnd is the K6 deliverable stated as a test: a
// client can set a channel's storage up and use it through libkv's ordinary
// API, carrying nothing but lcl.KVConfig.ChannelID, and the ACL holds against
// another member of the same community throughout.
//
// The earlier K2/K3 tests drive raw RPCs, because the client could not
// express a tag at all. This one exists to prove that is no longer true --
// and to pin the part a raw-RPC test cannot reach: mkdirP creates the
// intermediate directories of a path, and if those were created untagged the
// server would refuse to link them under a tagged parent, so the whole flow
// would fail for a reason no server-side test would show.
func TestKvChannelClientEndToEnd(t *testing.T) {
	sc := setupPrivScene(t, false)

	teamCfg := sc.teamCfg()
	kvFor := func(u *TestUser) (libkv.MetaContext, *libkv.Minder) {
		m := libkv.NewMetaContext(sc.tew.NewClientMetaContextWithEracer(t, u))
		return m, libkv.NewMinderWithCacheSettings(
			m.G().ActiveUser(), libclient.CacheSettings{})
	}

	// Alice owns the channel's ACL, so she may create its storage.
	chanCfg := lcl.KVConfig{
		ActingAs:  teamCfg,
		Roles:     proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		MkdirP:    true,
		ChannelID: &sc.chid,
	}

	root, err := core.RandomDomain()
	require.NoError(t, err)
	rootPath := proto.KVPath("/" + root)

	mA, kvA := kvFor(sc.alice.u)
	_, err = kvA.ChannelMkRoot(mA, chanCfg, sc.chid, rootPath)
	require.NoError(t, err)

	// One root per channel, and a caller can tell: a second attempt, even at
	// another path, fails with a recognisable error rather than silently
	// making a second tree. (Callers such as the SECO daemon treat it as
	// "the root exists".)
	againRoot, err := core.RandomDomain()
	require.NoError(t, err)
	_, err = kvA.ChannelMkRoot(mA, chanCfg, sc.chid, proto.KVPath("/"+againRoot))
	require.Error(t, err)
	require.Contains(t, err.Error(), "channel already has a storage root")

	// Mkdir cannot make the root: it creates and links in one walk, so the
	// link is attempted before the directory is registered and containment
	// refuses it. That ordering is why ChannelMkRoot exists.
	otherRoot, err := core.RandomDomain()
	require.NoError(t, err)
	_, err = kvA.Mkdir(mA, chanCfg, proto.KVPath("/"+otherRoot))
	require.Error(t, err,
		"a tagged directory may not be linked under the community tree unless registered")

	// Write through the ordinary API, several components deep, so mkdirP has
	// intermediate directories to create. They must be tagged too, or the
	// server refuses to link them.
	deep := rootPath + "/plans/q4/notes.txt"
	body := []byte("what the channel is actually for")
	_, err = kvA.PutFileFirst(mA, chanCfg, deep, body, true)
	require.NoError(t, err)

	// A large file in the same subtree, exercising the upload path's tag.
	bigPath := rootPath + "/plans/q4/big.bin"
	big := writeRandomFileWithConfig(t, kvA, mA, bigPath, 3*testChunkSize, chanCfg, 0)

	// Alice reads both back.
	got, err := kvA.GetFile(mA, chanCfg, deep)
	require.NoError(t, err)
	require.Equal(t, body, []byte(got.Chunk.Chunk))
	readFileWithConfig(t, kvA, mA, bigPath, big, chanCfg, 0)

	// Cleo is in the community and not in the channel. She is refused, all
	// the way down -- including at the subtree root, so she cannot even
	// enumerate it.
	readCfg := lcl.KVConfig{ActingAs: teamCfg}
	mC, kvC := kvFor(sc.cleo.u)
	_, err = kvC.GetFile(mC, readCfg, deep)
	require.Error(t, err, "a non-member must not read the channel's files")
	_, err = kvC.GetFile(mC, readCfg, bigPath)
	require.Error(t, err)
	_, err = kvC.List(mC, readCfg, rootPath, nil, rem.KVListOpts{})
	require.Error(t, err, "a non-member must not list the channel's subtree")

	// And she cannot create storage for a channel she is not in, which is
	// what stops her planting a node to be linked later.
	_, err = kvC.Mkdir(mC, chanCfg, proto.KVPath("/"+root+"-cleo"))
	require.Error(t, err, "a non-member must not create channel-tagged storage")

	// A grant is the whole difference, through the client API as well.
	sc.grant(t, sc.alice, sc.cleo, false)
	mC2, kvC2 := kvFor(sc.cleo.u)
	got, err = kvC2.GetFile(mC2, readCfg, deep)
	require.NoError(t, err)
	require.Equal(t, body, []byte(got.Chunk.Chunk))

	// Untagged storage in the same team is unaffected: the community's own
	// tree still works for everyone, which is the behaviour this must not
	// have disturbed.
	plainCfg := lcl.KVConfig{
		ActingAs: teamCfg,
		Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		MkdirP:   true,
	}
	other, err := core.RandomDomain()
	require.NoError(t, err)
	plainPath := proto.KVPath("/" + other + "/readme.txt")
	plain := []byte("ordinary community storage")

	// Alice makes the top-level directory: a team's KV root is created with
	// Write: AdminRole (KVParty.DefaultRootPerms), so an ordinary member
	// cannot create at the top level. That is upstream behaviour and nothing
	// to do with channels -- but it means the member half of this check has
	// to happen one level down, where the write role is DefaultRole.
	_, err = kvA.Mkdir(mA, plainCfg, proto.KVPath("/"+other))
	require.NoError(t, err)

	mB, kvB := kvFor(sc.bob.u)
	_, err = kvB.PutFileFirst(mB, plainCfg, plainPath, plain, true)
	require.NoError(t, err, "ordinary community storage must be untouched")
	got, err = kvB.GetFile(mB, plainCfg, plainPath)
	require.NoError(t, err)
	require.Equal(t, plain, []byte(got.Chunk.Chunk))

	// And the channel's member cannot read it any differently than anyone
	// else: tagging is not a second permission system layered on the team's.
	got, err = kvC2.GetFile(mC2, plainCfg, plainPath)
	require.NoError(t, err)
	require.Equal(t, plain, []byte(got.Chunk.Chunk))
}
