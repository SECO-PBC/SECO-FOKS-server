// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// The K3 tests drive the KV RPCs directly. libkv does not know about channel
// storage yet -- the client surface is a later spec -- so the wire is the
// only way to build a tagged tree, and it is the right way regardless: these
// pin what the SERVER accepts, which is what the guarantee rests on.
//
// The server stores a directory's seed box and a dirent's MACs without
// interpreting them (their integrity is checked by clients holding the keys),
// so the fixtures below carry well-formed but meaningless crypto. That keeps
// the tests about the ACL.

// mkdirRaw issues kvMkdir with an optional channel tag, returning the new
// directory's ID.
func mkdirRaw(
	t *testing.T, k *kvChannelScene, u *TestUser, ch *proto.RTChannelID,
) (proto.DirID, error) {
	m, cli, auth := k.rawFor(t, u)
	var did proto.DirID
	require.NoError(t, core.RandomFill(did[:]))
	ctext := make([]byte, 48)
	require.NoError(t, core.RandomFill(ctext))
	_, err := cli.KvMkdir(m.Ctx(), rem.KvMkdirArg{
		Hdr: rem.KVReqHeader{Auth: auth},
		Dir: proto.KVDir{
			Id:      did,
			Version: proto.KVVersion(1),
			Box: proto.SeedBoxExternalNonce{
				Rg:    proto.RoleAndGen{Role: proto.DefaultRole, Gen: proto.FirstGeneration},
				Ctext: proto.NaclCiphertext(ctext),
			},
			WriteRole: proto.DefaultRole,
			Status:    proto.KVDirStatus_Active,
		},
		ChannelID: ch,
	})
	return did, err
}

// putSmallRaw issues kvPutSmallFileOrSymlink with an optional channel tag.
func putSmallRaw(
	t *testing.T, k *kvChannelScene, u *TestUser, ch *proto.RTChannelID,
) (proto.KVNodeID, error) {
	m, cli, auth := k.rawFor(t, u)
	var nid proto.KVNodeID
	nid[0] = byte(proto.KVNodeType_SmallFile)
	require.NoError(t, core.RandomFill(nid[1:]))
	_, err := cli.KvPutSmallFileOrSymlink(m.Ctx(), rem.KvPutSmallFileOrSymlinkArg{
		Auth: auth,
		Id:   nid,
		Sfb: proto.SmallFileBox{
			Rg:      proto.RoleAndGen{Role: proto.DefaultRole, Gen: proto.FirstGeneration},
			DataBox: proto.NaclCiphertext("some ciphertext for a small file"),
		},
		ChannelID: ch,
	})
	return nid, err
}

// linkRaw issues kvPut, linking value into parent under a fresh name.
func linkRaw(
	t *testing.T, k *kvChannelScene, u *TestUser,
	parent proto.DirID, value proto.KVNodeID,
) error {
	m, cli, auth := k.rawFor(t, u)
	var deid proto.DirentID
	require.NoError(t, core.RandomFill(deid[:]))
	var nameMac, bindMac proto.HMAC
	require.NoError(t, core.RandomFill(nameMac[:]))
	require.NoError(t, core.RandomFill(bindMac[:]))
	nameBox := make([]byte, 40)
	require.NoError(t, core.RandomFill(nameBox))
	var nonce proto.NaclNonce
	require.NoError(t, core.RandomFill(nonce[:]))
	sb := proto.NewSecretBoxWithNacl(proto.NaclSecretBox{
		Nonce:      nonce,
		Ciphertext: proto.NaclCiphertext(nameBox),
	})
	return cli.KvPut(m.Ctx(), rem.KvPutArg{
		Hdr: rem.KVReqHeader{Auth: auth},
		Dirents: []proto.KVDirent{{
			ParentDir:  parent,
			Id:         deid,
			Value:      value,
			Version:    proto.KVVersion(1),
			DirVersion: proto.KVVersion(1),
			WriteRole:  proto.DefaultRole,
			NameMac:    nameMac,
			NameBox:    sb,
			DirStatus:  proto.KVDirStatus_Active,
			BindingMac: bindMac,
		}},
	})
}

// TestKvChannelCreationRequiresMembership pins §7 H3: a node is tagged at the
// instant it exists, and only a member can create one. That is what closes
// the window in which a directory could hold channel data untagged, waiting
// to be linked -- there is no such window, because a non-member cannot mint
// a tagged node at all.
func TestKvChannelCreationRequiresMembership(t *testing.T) {
	k := setupKvChannelScene(t)
	sc := k.sc

	// Cleo is in the community, not in the channel.
	_, err := mkdirRaw(t, k, sc.cleo.u, &sc.chid)
	require.Error(t, err, "a non-member must not create channel-tagged storage")
	_, err = putSmallRaw(t, k, sc.cleo.u, &sc.chid)
	require.Error(t, err)

	// A channel that does not exist answers the same way, to a member.
	var ghostCh proto.RTChannelID
	require.NoError(t, core.RandomFill(ghostCh[:]))
	_, errGhost := mkdirRaw(t, k, sc.alice.u, &ghostCh)
	_, errDenied := mkdirRaw(t, k, sc.cleo.u, &sc.chid)
	require.Error(t, errGhost)
	require.Equal(t, errGhost, errDenied,
		"no membership and no channel must be indistinguishable")

	// Alice, the ACL owner, may. And untagged creation is unaffected for
	// everyone, which is the upstream behaviour this must not disturb.
	_, err = mkdirRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	_, err = mkdirRaw(t, k, sc.cleo.u, nil)
	require.NoError(t, err, "untagged creation must be untouched")
}

// TestKvChannelOrphanIsProtected pins the other half of H3: a tagged node
// that has been created but never linked is already invisible to
// non-members. Protection does not wait for the link.
func TestKvChannelOrphanIsProtected(t *testing.T) {
	k := setupKvChannelScene(t)
	sc := k.sc

	orphan, err := putSmallRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)

	mC, cliC, authC := k.rawFor(t, sc.cleo.u)
	var ghost proto.KVNodeID
	ghost[0] = byte(proto.KVNodeType_SmallFile)
	require.NoError(t, core.RandomFill(ghost[1:]))

	_, errReal := cliC.KvGetNode(mC.Ctx(), rem.KvGetNodeArg{Auth: authC, Id: orphan})
	_, errGhost := cliC.KvGetNode(mC.Ctx(), rem.KvGetNodeArg{Auth: authC, Id: ghost})
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal,
		"an unlinked tagged node is masked from the moment it exists")

	// And its creator can still see it.
	mA, cliA, authA := k.rawFor(t, sc.alice.u)
	_, err = cliA.KvGetNode(mA.Ctx(), rem.KvGetNodeArg{Auth: authA, Id: orphan})
	require.NoError(t, err)
}

// TestKvChannelContainment pins the invariant that keeps a channel's storage
// one subtree instead of a scattering of tagged rows, in both directions,
// plus the single legal crossing.
func TestKvChannelContainment(t *testing.T) {
	k := setupKvChannelScene(t)
	sc := k.sc

	// A second private channel, with alice in both, so a cross-channel link
	// is refused on containment rather than on membership.
	otherID, _ := sc.makePrivateAt(t, sc.alice, proto.DefaultRole)

	untaggedDir, err := mkdirRaw(t, k, sc.alice.u, nil)
	require.NoError(t, err)
	taggedDir, err := mkdirRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	taggedFile, err := putSmallRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	untaggedFile, err := putSmallRaw(t, k, sc.alice.u, nil)
	require.NoError(t, err)
	otherFile, err := putSmallRaw(t, k, sc.alice.u, &otherID)
	require.NoError(t, err)

	// Baselines: like under like, both untagged and both tagged.
	require.NoError(t, linkRaw(t, k, sc.alice.u, untaggedDir, untaggedFile))
	require.NoError(t, linkRaw(t, k, sc.alice.u, taggedDir, taggedFile))

	// Tagged child under an untagged parent: channel data outside the ACL's
	// reach. Refused unless it is the registered root.
	strayFile, err := putSmallRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	require.Error(t, linkRaw(t, k, sc.alice.u, untaggedDir, strayFile),
		"a tagged node may not hang outside its channel's subtree")

	// Untagged child under a tagged parent: a hole in the other direction,
	// content inside a channel's tree that the whole community may read.
	require.Error(t, linkRaw(t, k, sc.alice.u, taggedDir, untaggedFile),
		"untagged content may not be planted inside a channel's subtree")

	// Another channel's node, by a member of both: refused on containment.
	require.Error(t, linkRaw(t, k, sc.alice.u, taggedDir, otherFile),
		"channels must not be able to share a subtree")

	// The one legal crossing: a registered root under the untagged tree.
	rootDir, err := mkdirRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	mA, cliA, authA := k.rawFor(t, sc.alice.u)
	require.NoError(t, cliA.KvChannelMkRoot(mA.Ctx(), rem.KvChannelMkRootArg{
		Auth: authA, ChannelID: sc.chid, DirID: rootDir,
	}))
	require.NoError(t, linkRaw(t, k, sc.alice.u, untaggedDir, *rootDir.KVNodeID()),
		"the registered root is the one tagged node allowed under the community tree")

	// Registration blesses exactly one directory: a sibling tagged for the
	// same channel is still refused, so a member cannot mint a second
	// entrance to the subtree.
	require.Error(t, linkRaw(t, k, sc.alice.u, untaggedDir, *taggedDir.KVNodeID()),
		"only the registered root may cross")
}

// TestKvChannelHardlinkAcrossChannelsRefused covers the multi-parent case
// the schema allows (dir_refcount exists because a directory can have more
// than one parent). Each link is checked on its own, so a hardlink that
// would straddle two trees fails at the second link rather than quietly
// creating a bridge between them.
func TestKvChannelHardlinkAcrossChannelsRefused(t *testing.T) {
	k := setupKvChannelScene(t)
	sc := k.sc

	taggedDir, err := mkdirRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	taggedFile, err := putSmallRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	untaggedDir, err := mkdirRaw(t, k, sc.alice.u, nil)
	require.NoError(t, err)

	// First link, inside the channel: fine.
	require.NoError(t, linkRaw(t, k, sc.alice.u, taggedDir, taggedFile))
	// Second link, out into the community tree: refused.
	require.Error(t, linkRaw(t, k, sc.alice.u, untaggedDir, taggedFile),
		"a hardlink may not bridge a channel's subtree and the community's")
}

// TestKvChannelTagIsImmutableOnReplay pins H1 at the wire: re-sending a
// creation with a different tag is not a replay. The replay comparison
// covers the tag along with every other persisted field, so an attempt to
// relabel a node by resending it is refused rather than silently keeping
// whichever tag landed first.
func TestKvChannelTagIsImmutableOnReplay(t *testing.T) {
	k := setupKvChannelScene(t)
	sc := k.sc

	m, cli, auth := k.rawFor(t, sc.alice.u)
	var nid proto.KVNodeID
	nid[0] = byte(proto.KVNodeType_SmallFile)
	require.NoError(t, core.RandomFill(nid[1:]))
	mk := func(ch *proto.RTChannelID) (rem.KVPutSmallFileOrSymlinkRes, error) {
		return cli.KvPutSmallFileOrSymlink(m.Ctx(), rem.KvPutSmallFileOrSymlinkArg{
			Auth: auth,
			Id:   nid,
			Sfb: proto.SmallFileBox{
				Rg:      proto.RoleAndGen{Role: proto.DefaultRole, Gen: proto.FirstGeneration},
				DataBox: proto.NaclCiphertext("identical bytes every time"),
			},
			ChannelID: ch,
		})
	}

	res, err := mk(&sc.chid)
	require.NoError(t, err)
	require.False(t, res.WasReplay)

	// The same write again is the genuine retry case, and stays a replay.
	res, err = mk(&sc.chid)
	require.NoError(t, err)
	require.True(t, res.WasReplay)

	// Identical bytes, different tag: not a replay, and not allowed to
	// quietly keep the stored tag.
	_, err = mk(nil)
	require.Error(t, err, "dropping the tag by resending must be refused")
	other, _ := sc.makePrivateAt(t, sc.alice, proto.DefaultRole)
	_, err = mk(&other)
	require.Error(t, err, "moving the tag by resending must be refused")

	// The same, for directories: putDir has its own replay comparison, and
	// it must weigh the tag too. Covered separately because a regression in
	// one path says nothing about the other.
	var did proto.DirID
	require.NoError(t, core.RandomFill(did[:]))
	ctext := make([]byte, 48)
	require.NoError(t, core.RandomFill(ctext))
	mkdir := func(ch *proto.RTChannelID) (rem.KVMkdirRes, error) {
		return cli.KvMkdir(m.Ctx(), rem.KvMkdirArg{
			Hdr: rem.KVReqHeader{Auth: auth},
			Dir: proto.KVDir{
				Id:      did,
				Version: proto.KVVersion(1),
				Box: proto.SeedBoxExternalNonce{
					Rg:    proto.RoleAndGen{Role: proto.DefaultRole, Gen: proto.FirstGeneration},
					Ctext: proto.NaclCiphertext(ctext),
				},
				WriteRole: proto.DefaultRole,
				Status:    proto.KVDirStatus_Active,
			},
			ChannelID: ch,
		})
	}

	dres, err := mkdir(&sc.chid)
	require.NoError(t, err)
	require.False(t, dres.WasReplay)

	dres, err = mkdir(&sc.chid)
	require.NoError(t, err)
	require.True(t, dres.WasReplay, "an identical mkdir stays a replay")

	_, err = mkdir(nil)
	require.Error(t, err, "dropping a directory's tag by resending must be refused")
	_, err = mkdir(&other)
	require.Error(t, err, "moving a directory's tag by resending must be refused")
}
