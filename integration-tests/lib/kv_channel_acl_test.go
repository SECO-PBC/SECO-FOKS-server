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

// kvChannelScene is a privScene (team, private channel, alice as ACL owner)
// with files planted in the team's KV store and every node row tagged as the
// channel's storage.
//
// The tagging is done with direct SQL against the KV shard, deliberately:
// creation-time tagging is the next slice (K3 in docs/kv-channel-acl.md), and
// these tests pin the READ gate on its own, so a K3 regression cannot hide a
// K2 one. Once creation tagging exists, its tests build tagged trees over the
// wire; these stay as the gate's own fixture.
type kvChannelScene struct {
	sc     *privScene
	pid    proto.PartyID
	chid   int64
	cfg    lcl.KVConfig
	small  proto.KVPath
	large  proto.KVPath
	sbytes []byte
	lbytes []byte
	fileID proto.FileID
	rootID proto.DirID
	smalID proto.KVNodeID
}

func (k *kvChannelScene) kvFor(t *testing.T, u *TestUser) (libkv.MetaContext, *libkv.Minder) {
	m := libkv.NewMetaContext(k.sc.tew.NewClientMetaContextWithEracer(t, u))
	return m, libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})
}

func setupKvChannelScene(t *testing.T) *kvChannelScene {
	sc := setupPrivScene(t, false)

	pid := sc.teamID.ToPartyID()

	k := &kvChannelScene{
		sc:   sc,
		pid:  pid,
		chid: sc.chid.Short().Int64(),
		cfg: lcl.KVConfig{
			ActingAs: sc.teamCfg(),
			Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
			MkdirP:   true,
		},
	}

	root, err := core.RandomDomain()
	require.NoError(t, err)
	k.small = proto.KVPath("/" + root + "/notes.txt")
	k.large = proto.KVPath("/" + root + "/big.bin")

	mA, kvA := k.kvFor(t, sc.alice.u)

	dirID, err := kvA.Mkdir(mA, k.cfg, proto.KVPath("/"+root))
	require.NoError(t, err)
	k.rootID = *dirID

	k.sbytes = []byte("channel secret")
	sres, err := kvA.PutFileFirst(mA, k.cfg, k.small, k.sbytes, true)
	require.NoError(t, err)
	require.NotNil(t, sres)
	k.smalID = sres.NodeID

	k.lbytes = writeRandomFileWithConfig(t, kvA, mA, k.large, 3*testChunkSize, k.cfg, 0)
	gfr, err := kvA.GetFile(mA, k.cfg, k.large)
	require.NoError(t, err)
	require.NotNil(t, gfr.Id)
	k.fileID = *gfr.Id

	// Tag every node in the team's store as the channel's. The store holds
	// only what this test wrote, so the blanket UPDATE is exact.
	m := sc.tew.MetaContext()
	kvdb, err := m.KVShard(pid)
	require.NoError(t, err)
	defer kvdb.Release()
	for _, tbl := range []string{"dir", "large_file", "small_file_or_symlink"} {
		_, err := kvdb.Exec(m.Ctx(),
			`UPDATE `+tbl+` SET channel_id=$1 WHERE short_host_id=$2 AND short_party_id=$3`,
			k.chid, int(m.ShortHostID()), pid.Shorten().ExportToDB(),
		)
		require.NoError(t, err)
	}
	return k
}

func (k *kvChannelScene) rawFor(t *testing.T, u *TestUser) (libclient.MetaContext, rem.KVStoreClient, rem.KVAuth) {
	m := k.sc.tew.NewClientMetaContext(t, u)
	cli := kvStoreClientForUser(t, k.sc.tew, u)
	tok := makeVOBearerTokenForUser(t, k.sc.tm, u, nil)
	return m, cli, rem.NewKVAuthWithTeam(tok)
}

// TestKvChannelAclReadGate walks the whole §1 arc of docs/kv-channel-acl.md:
// two members of one community, and the only thing separating reader from
// refused is a channel_acl row. Denials must be indistinguishable from the
// node not existing, which is asserted as error EQUALITY against probes for
// genuinely absent nodes, not as a not-nil check.
func TestKvChannelAclReadGate(t *testing.T) {
	k := setupKvChannelScene(t)
	sc := k.sc
	readCfg := lcl.KVConfig{ActingAs: sc.teamCfg()}

	// Alice, the ACL owner who wrote the files, still reads them: the gate
	// wiring must pass members, not merely pass when the tag is NULL.
	mA, kvA := k.kvFor(t, sc.alice.u)
	readFileWithConfig(t, kvA, mA, k.large, k.lbytes, k.cfg, 0)
	sgot, err := kvA.GetFile(mA, readCfg, k.small)
	require.NoError(t, err)
	require.Equal(t, k.sbytes, []byte(sgot.Chunk.Chunk))

	// Cleo is in the team but not in the channel. Path reads die at the
	// tagged root dir with the same answer a nonexistent tree gets.
	mC, kvC := k.kvFor(t, sc.cleo.u)
	_, errReal := kvC.GetFile(mC, readCfg, k.large)
	require.Error(t, errReal)
	ghostRoot, err2 := core.RandomDomain()
	require.NoError(t, err2)
	_, errGhost := kvC.GetFile(mC, readCfg, proto.KVPath("/"+ghostRoot+"/nope.bin"))
	require.Error(t, errGhost)
	require.IsType(t, errGhost, errReal,
		"a denial must look exactly like absence, not like a permission error")

	// Raw probes by ID: the metadata and the ciphertext answer exactly as
	// they do for a file that was never uploaded.
	mCr, cliC, authC := k.rawFor(t, sc.cleo.u)
	var ghost proto.FileID
	require.NoError(t, core.RandomFill(ghost[:]))

	_, errReal = cliC.KvGetNode(mCr.Ctx(), rem.KvGetNodeArg{Auth: authC, Id: k.fileID.KVNodeID()})
	_, errGhost = cliC.KvGetNode(mCr.Ctx(), rem.KvGetNodeArg{Auth: authC, Id: ghost.KVNodeID()})
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal, "kvGetNode: masked exactly as absent")

	// A tombstone node ID is a legal value that names no node; it must
	// answer rather than crash the server (upstream #374).
	var tombstone proto.KVNodeID
	require.NoError(t, core.RandomFill(tombstone[1:]))
	_, err = cliC.KvGetNode(mCr.Ctx(), rem.KvGetNodeArg{Auth: authC, Id: tombstone})
	require.Error(t, err, "a tombstone node ID must answer, not crash")

	_, errReal = cliC.KvGetEncryptedChunk(mCr.Ctx(), rem.KvGetEncryptedChunkArg{
		Auth: authC, Id: k.fileID, Offset: proto.Offset(testChunkSize)})
	_, errGhost = cliC.KvGetEncryptedChunk(mCr.Ctx(), rem.KvGetEncryptedChunkArg{
		Auth: authC, Id: ghost, Offset: proto.Offset(testChunkSize)})
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal, "kvGetEncryptedChunk: masked exactly as absent")

	// Every loader is probed at its own RPC, because the path walk above
	// dies at the first gate it meets and would keep passing if a deeper
	// loader lost its check: the dir itself (kvGetDir and kvList reach
	// loadDir with no dirent in front of it), the small file's box, and the
	// dir's version number, which is the probing side channel of
	// docs/kv-channel-acl.md §6 row 11. Each must equal its ghost.
	var ghostDir proto.DirID
	require.NoError(t, core.RandomFill(ghostDir[:]))
	var ghostSmall proto.KVNodeID
	ghostSmall[0] = byte(proto.KVNodeType_SmallFile)
	require.NoError(t, core.RandomFill(ghostSmall[1:]))

	_, errReal = cliC.KvGetDir(mCr.Ctx(), rem.KvGetDirArg{Auth: authC, Id: k.rootID})
	_, errGhost = cliC.KvGetDir(mCr.Ctx(), rem.KvGetDirArg{Auth: authC, Id: ghostDir})
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal, "kvGetDir: masked exactly as absent")

	_, errReal = cliC.KvList(mCr.Ctx(), rem.KvListArg{Auth: authC, Dir: k.rootID})
	_, errGhost = cliC.KvList(mCr.Ctx(), rem.KvListArg{Auth: authC, Dir: ghostDir})
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal, "kvList: masked exactly as absent")

	_, errReal = cliC.KvGetNode(mCr.Ctx(), rem.KvGetNodeArg{Auth: authC, Id: k.smalID})
	_, errGhost = cliC.KvGetNode(mCr.Ctx(), rem.KvGetNodeArg{Auth: authC, Id: ghostSmall})
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal, "small file: masked exactly as absent")

	// kvGet resolves one (parentDir, nameMAC) hop, and its dirent loader is
	// the ONLY dir gate on that RPC -- it never calls loadDir. Its denial is
	// nil-row, which the client API renders identically to a plain miss, so
	// the pin is that the probe with a REAL name MAC (lifted from the dirent
	// table, since cleo cannot compute one without reading the dir she is
	// denied) must ERROR like a miss rather than answer the dirent.
	var realMac []byte
	var realVers int
	mSrv := sc.tew.MetaContext()
	kvdbP, err3 := mSrv.KVShard(k.pid)
	require.NoError(t, err3)
	err3 = kvdbP.QueryRow(mSrv.Ctx(),
		`SELECT name_mac, dir_version FROM dirent
		 WHERE short_host_id=$1 AND short_party_id=$2 AND dir_id=$3 AND active=true
		 LIMIT 1`,
		int(mSrv.ShortHostID()), k.pid.Shorten().ExportToDB(), k.rootID.ExportToDB(),
	).Scan(&realMac, &realVers)
	kvdbP.Release()
	require.NoError(t, err3)

	probeGet := func(mac []byte, vers int) error {
		var hm proto.HMAC
		require.Equal(t, len(hm), copy(hm[:], mac))
		_, err := cliC.KvGet(mCr.Ctx(), rem.KvGetArg{
			Hdr: rem.KVReqHeader{Auth: authC},
			Path: rem.KVNodePathMultiple{
				ParentDir: k.rootID,
				Names: []rem.KVNameMACAtDirVersion{{
					DirVers: proto.KVVersion(vers),
					Mac:     hm,
				}},
			},
			Follow: rem.FollowBehavior_None,
		})
		return err
	}
	ghostMac := make([]byte, len(proto.HMAC{}))
	require.NoError(t, core.RandomFill(ghostMac))
	errReal = probeGet(realMac, realVers)
	errGhost = probeGet(ghostMac, realVers)
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal, "kvGet dirent: masked exactly as absent")

	probeVV := func(dir proto.DirID) error {
		return cliC.KvCacheCheck(mCr.Ctx(), rem.KVReqHeader{
			Auth: authC,
			Precondition: &proto.PathVersionVector{
				Path: []proto.DirVersion{{Id: dir, Vers: proto.KVVersion(1)}},
			},
		})
	}
	errReal = probeVV(k.rootID)
	errGhost = probeVV(ghostDir)
	require.Error(t, errReal)
	require.Equal(t, errGhost, errReal, "version probe: masked exactly as absent")

	// A channel_acl row is the whole difference: grant cleo and the same
	// reads succeed with the same bytes.
	sc.grant(t, sc.alice, sc.cleo, false)
	mC2, kvC2 := k.kvFor(t, sc.cleo.u)
	readFileWithConfig(t, kvC2, mC2, k.large, k.lbytes, readCfg, 0)
	sgot, err = kvC2.GetFile(mC2, readCfg, k.small)
	require.NoError(t, err)
	require.Equal(t, k.sbytes, []byte(sgot.Chunk.Chunk))

	// And revocation closes the door again, at the next read, with no rekey
	// and no grace: the policy IS the guarantee.
	sc.revoke(t, sc.alice, sc.cleo)
	mC3, kvC3 := k.kvFor(t, sc.cleo.u)
	_, errReal = kvC3.GetFile(mC3, readCfg, k.large)
	require.Error(t, errReal)
}

// TestKvChannelMkRootAuthorization pins kvChannelMkRoot's standing rules:
// ACL owner or team admin, channel real and private and of THIS team, dir
// real and already tagged; a replay succeeds, a second root is refused, and
// every authorization failure answers as if the channel did not exist.
func TestKvChannelMkRootAuthorization(t *testing.T) {
	k := setupKvChannelScene(t)
	sc := k.sc

	mkroot := func(u *TestUser, chid proto.RTChannelID, dir proto.DirID) error {
		m, cli, auth := k.rawFor(t, u)
		return cli.KvChannelMkRoot(m.Ctx(), rem.KvChannelMkRootArg{
			Auth: auth, ChannelID: chid, DirID: dir,
		})
	}

	// Bob: team member, not in the channel, not admin. Masked.
	errBob := mkroot(sc.bob.u, sc.chid, k.rootID)
	require.Error(t, errBob)

	// A channel id that does not exist answers identically.
	var ghostCh proto.RTChannelID
	require.NoError(t, core.RandomFill(ghostCh[:]))
	errGhost := mkroot(sc.alice.u, ghostCh, k.rootID)
	require.Error(t, errGhost)
	require.Equal(t, errGhost, errBob, "no standing and no channel must be indistinguishable")

	// Alice, the ACL owner: registers, and a replay of the same root is
	// idempotent.
	require.NoError(t, mkroot(sc.alice.u, sc.chid, k.rootID))
	require.NoError(t, mkroot(sc.alice.u, sc.chid, k.rootID))

	// Dara, team admin outside the channel, has management standing (the
	// same standing rtChannelGrant gives admins); re-registering the same
	// root is a replay for her too.
	require.NoError(t, mkroot(sc.dara.u, sc.chid, k.rootID))

	// A different directory for a channel that has a root is refused. It is
	// created through the tagging path rather than planted, and left
	// unlinked: registration is about the directory, not about where it
	// hangs.
	otherID, err := mkdirRaw(t, k, sc.alice.u, &sc.chid)
	require.NoError(t, err)
	require.Error(t, mkroot(sc.alice.u, sc.chid, otherID))

	// An untagged directory is refused even for the owner: creation is the
	// membership-gated act, registration only blesses what members made.
	untaggedID, err := mkdirRaw(t, k, sc.alice.u, nil)
	require.NoError(t, err)
	require.Error(t, mkroot(sc.alice.u, sc.chid, untaggedID))
}
