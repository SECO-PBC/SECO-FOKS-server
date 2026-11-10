// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"errors"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/integration-tests/common"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// The system store (fork-only; docs/kv-system-store.md). The first half drives
// the KV RPCs directly: what these pin is what the SERVER accepts, and a
// Leader holds the keys and need not go through libkv at all. The fixtures
// carry well-formed but meaningless crypto, since the server stores a seed
// box and a dirent's MACs without interpreting them. The second half drives
// libkv, with real keys, for the client's half of the contract.

type sysScene struct {
	*kvChannelScene
	sage  *TestUser // an ADMIN of the team, on the Sage list
	sage2 *TestUser // a second ADMIN, on the list only where a test asks
}

// setupSysScene: alice is OWNER and dara ADMIN, both Leaders and not Sages;
// bob is a plain member. sage and sage2 are fresh admins.
func setupSysScene(t *testing.T) *sysScene {
	sc := setupPrivScene(t, true)
	sage := sc.tew.NewTestUser(t)
	sage2 := sc.tew.NewTestUser(t)
	sc.tew.DirectDoubleMerklePokeInTest(t)
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u,
		[]proto.MemberRole{
			sage.toMemberRole(t, proto.AdminRole, sc.tm.hepks),
			sage2.toMemberRole(t, proto.AdminRole, sc.tm.hepks),
		}, nil,
	)
	s := &sysScene{
		kvChannelScene: &kvChannelScene{sc: sc, pid: sc.teamID.ToPartyID()},
		sage:           sage,
		sage2:          sage2,
	}
	s.listSage(t, sage)
	return s
}

// listSage is what a signup with a flagged code or the operator does.
func (s *sysScene) listSage(t *testing.T, u *TestUser) {
	m := s.sc.tew.MetaContext()
	db, err := m.Db(shared.DbTypeUsers)
	require.NoError(t, err)
	defer db.Release()
	_, err = db.Exec(m.Ctx(),
		`INSERT INTO daemon_accounts(short_host_id, uid, ctime) VALUES($1,$2,NOW())`,
		int(m.ShortHostID()), u.uid.ExportToDB())
	require.NoError(t, err)
}

func requireKVPerm(t *testing.T, err error, msg string) {
	t.Helper()
	require.Error(t, err, msg)
	require.IsType(t, core.KVPermssionError{}, err, msg)
}

func randDirID(t *testing.T) proto.DirID {
	var did proto.DirID
	require.NoError(t, core.RandomFill(did[:]))
	return did
}

type rawDirOpts struct {
	locked bool
	write  proto.Role
	ch     *proto.RTChannelID
}

// rawKVDir is a directory row as a client would send it, readable at the
// default role.
func rawKVDir(t *testing.T, id proto.DirID, opts rawDirOpts) proto.KVDir {
	ctext := make([]byte, 48)
	require.NoError(t, core.RandomFill(ctext))
	write := opts.write
	if write == (proto.Role{}) {
		write = proto.AdminRole
	}
	return proto.KVDir{
		Id:      id,
		Version: proto.KVVersion(1),
		Box: proto.SeedBoxExternalNonce{
			Rg:    proto.RoleAndGen{Role: proto.DefaultRole, Gen: proto.FirstGeneration},
			Ctext: proto.NaclCiphertext(ctext),
		},
		WriteRole:  write,
		Status:     proto.KVDirStatus_Active,
		DaemonLock: opts.locked,
	}
}

func (s *sysScene) mkdir(t *testing.T, u *TestUser, opts rawDirOpts) (proto.DirID, error) {
	m, cli, auth := s.rawFor(t, u)
	did := randDirID(t)
	_, err := cli.KvMkdir(m.Ctx(), rem.KvMkdirArg{
		Hdr:       rem.KVReqHeader{Auth: auth},
		Dir:       rawKVDir(t, did, opts),
		ChannelID: opts.ch,
	})
	return did, err
}

func (s *sysScene) rawRoot(t *testing.T, dir proto.DirID) proto.KVRoot {
	var mac proto.HMAC
	require.NoError(t, core.RandomFill(mac[:]))
	return proto.KVRoot{
		Root:       dir,
		Vers:       proto.KVVersion(1),
		Rg:         proto.RoleAndGen{Role: proto.DefaultRole, Gen: proto.FirstGeneration},
		BindingMac: mac,
	}
}

func (s *sysScene) putSystemRoot(t *testing.T, u *TestUser, root proto.KVRoot) error {
	m, cli, auth := s.rawFor(t, u)
	return cli.KvPutSystemRoot(m.Ctx(), rem.KvPutSystemRootArg{Auth: auth, Root: root})
}

func (s *sysScene) smallFile(t *testing.T, u *TestUser) proto.KVNodeID {
	nid, err := putSmallRaw(t, s.kvChannelScene, u, nil)
	require.NoError(t, err)
	return nid
}

// rawEntry names one dirent across its versions: the ID and name MAC stay
// fixed, which is what makes a write a replacement.
type rawEntry struct {
	id   proto.DirentID
	mac  proto.HMAC
	dir  proto.DirID
	vers proto.KVVersion
}

func newRawEntry(t *testing.T, dir proto.DirID) rawEntry {
	var ret rawEntry
	require.NoError(t, core.RandomFill(ret.id[:]))
	require.NoError(t, core.RandomFill(ret.mac[:]))
	ret.dir = dir
	ret.vers = proto.KVVersion(1)
	return ret
}

func (s *sysScene) put(t *testing.T, u *TestUser, e rawEntry, value proto.KVNodeID) error {
	m, cli, auth := s.rawFor(t, u)
	var bindMac proto.HMAC
	require.NoError(t, core.RandomFill(bindMac[:]))
	nameBox := make([]byte, 40)
	require.NoError(t, core.RandomFill(nameBox))
	var nonce proto.NaclNonce
	require.NoError(t, core.RandomFill(nonce[:]))
	return cli.KvPut(m.Ctx(), rem.KvPutArg{
		Hdr: rem.KVReqHeader{Auth: auth},
		Dirents: []proto.KVDirent{{
			ParentDir:  e.dir,
			Id:         e.id,
			Value:      value,
			Version:    e.vers,
			DirVersion: proto.KVVersion(1),
			WriteRole:  proto.AdminRole,
			NameMac:    e.mac,
			NameBox: proto.NewSecretBoxWithNacl(proto.NaclSecretBox{
				Nonce:      nonce,
				Ciphertext: proto.NaclCiphertext(nameBox),
			}),
			DirStatus:  proto.KVDirStatus_Active,
			BindingMac: bindMac,
		}},
	})
}

// read returns the current dirent at the entry's name, the way any reader
// resolves a name: the highest version, across IDs.
func (s *sysScene) read(t *testing.T, u *TestUser, e rawEntry) proto.KVDirent {
	m, cli, auth := s.rawFor(t, u)
	res, err := cli.KvGet(m.Ctx(), rem.KvGetArg{
		Hdr: rem.KVReqHeader{Auth: auth},
		Path: rem.KVNodePathMultiple{
			ParentDir: e.dir,
			Names:     []rem.KVNameMACAtDirVersion{{DirVers: proto.KVVersion(1), Mac: e.mac}},
		},
		Follow: rem.FollowBehavior_None,
	})
	require.NoError(t, err)
	return res.De
}

// makeStore does what a Sage's first write does, over the wire.
func (s *sysScene) makeStore(t *testing.T) (proto.DirID, rawEntry, proto.KVNodeID) {
	dir, err := s.mkdir(t, s.sage, rawDirOpts{locked: true})
	require.NoError(t, err)
	require.NoError(t, s.putSystemRoot(t, s.sage, s.rawRoot(t, dir)))
	e := newRawEntry(t, dir)
	val := s.smallFile(t, s.sage)
	require.NoError(t, s.put(t, s.sage, e, val))
	return dir, e, val
}

func tombstone() proto.KVNodeID { return proto.KVNodeID{} }

func TestKvSystemStoreLeaderCannotChange(t *testing.T) {
	s := setupSysScene(t)
	dir, entry, val := s.makeStore(t)
	sc := s.sc

	for _, leader := range []*TestUser{sc.alice.u, sc.dara.u} {
		// A new entry.
		requireKVPerm(t, s.put(t, leader, newRawEntry(t, dir), s.smallFile(t, leader)),
			"a Leader must not add an entry")

		// A replacement: same ID and name, next version.
		next := entry
		next.vers = entry.vers + 1
		requireKVPerm(t, s.put(t, leader, next, s.smallFile(t, leader)),
			"a Leader must not replace an entry")

		// A removal is a replacement by a tombstone.
		requireKVPerm(t, s.put(t, leader, next, tombstone()),
			"a Leader must not remove an entry")
	}

	got := s.read(t, s.sage, entry)
	require.Equal(t, val, got.Value, "the entry must be unchanged")
	require.Equal(t, entry.vers, got.Version)
}

func TestKvSystemStoreForgedEntriesRefused(t *testing.T) {
	s := setupSysScene(t)
	dir, entry, val := s.makeStore(t)
	leader := s.sc.alice.u

	// A new dirent ID at the existing name: readers take the highest version
	// per name across IDs, so this would shadow the entry if it were written.
	forged := newRawEntry(t, dir)
	forged.mac = entry.mac
	forged.vers = proto.KVVersion(50)
	requireKVPerm(t, s.put(t, leader, forged, s.smallFile(t, leader)), "a new ID must not shadow the entry")

	// A skipped version of the same ID: loadExistingDirent only looks at v-1.
	skipped := entry
	skipped.vers = proto.KVVersion(50)
	requireKVPerm(t, s.put(t, leader, skipped, s.smallFile(t, leader)), "a skipped version must be refused")

	// A symlink value in the same spot.
	var sym proto.KVNodeID
	sym[0] = byte(proto.KVNodeType_Symlink)
	require.NoError(t, core.RandomFill(sym[1:]))
	m, cli, auth := s.rawFor(t, leader)
	_, err := cli.KvPutSmallFileOrSymlink(m.Ctx(), rem.KvPutSmallFileOrSymlinkArg{
		Auth: auth,
		Id:   sym,
		Sfb: proto.SmallFileBox{
			Rg:      proto.RoleAndGen{Role: proto.DefaultRole, Gen: proto.FirstGeneration},
			DataBox: proto.NaclCiphertext("a symlink pointing somewhere else"),
		},
	})
	require.NoError(t, err)
	next := entry
	next.vers = entry.vers + 1
	requireKVPerm(t, s.put(t, leader, next, sym), "a symlink value must be refused")

	got := s.read(t, s.sage, entry)
	require.Equal(t, val, got.Value, "readers must still see the existing entry")
	require.Equal(t, entry.vers, got.Version)
}

func TestKvSystemStoreLinksIntoStoreRefused(t *testing.T) {
	s := setupSysScene(t)
	dir, _, _ := s.makeStore(t)
	sc := s.sc

	ordinary, err := s.mkdir(t, sc.alice.u, rawDirOpts{})
	require.NoError(t, err)

	// From the ordinary tree, by anyone, Sage included: Sage must not be a
	// confused deputy for a Leader who points an ordinary name at the store.
	for _, u := range []*TestUser{sc.alice.u, sc.dara.u, s.sage} {
		requireKVPerm(t, s.put(t, u, newRawEntry(t, ordinary), *dir.KVNodeID()),
			"an ordinary dirent must not name a locked directory")
	}

	// putRoot naming a locked directory, likewise for everyone.
	for _, u := range []*TestUser{sc.alice.u, s.sage} {
		m, cli, auth := s.rawFor(t, u)
		err := cli.KvPutRoot(m.Ctx(), rem.KvPutRootArg{Auth: auth, Root: s.rawRoot(t, dir)})
		requireKVPerm(t, err, "an ordinary root must not be a locked directory")
	}

	// The other direction: an unlocked directory inside the store would be a
	// hole any Leader could write.
	unlocked, err := s.mkdir(t, s.sage, rawDirOpts{})
	require.NoError(t, err)
	requireKVPerm(t, s.put(t, s.sage, newRawEntry(t, dir), *unlocked.KVNodeID()),
		"a locked directory must not name an unlocked one")

	// Inside the store, a locked child is fine.
	child, err := s.mkdir(t, s.sage, rawDirOpts{locked: true})
	require.NoError(t, err)
	require.NoError(t, s.put(t, s.sage, newRawEntry(t, dir), *child.KVNodeID()))

	// Control: ordinary links and an ordinary root are unaffected.
	require.NoError(t, s.put(t, sc.alice.u, newRawEntry(t, ordinary), s.smallFile(t, sc.alice.u)))
	m, cli, auth := s.rawFor(t, sc.alice.u)
	require.NoError(t, cli.KvPutRoot(m.Ctx(), rem.KvPutRootArg{Auth: auth, Root: s.rawRoot(t, ordinary)}))
}

func TestKvSystemStoreLocks(t *testing.T) {
	s := setupSysScene(t)
	dir, entry, _ := s.makeStore(t)
	sc := s.sc

	lock := func(d proto.DirID, id proto.DirentID) rem.KVLock {
		var lid rem.LockID
		require.NoError(t, core.RandomFill(lid[:]))
		return rem.KVLock{
			Idp:    proto.KVDirentIDPair{ParentDirID: d, DirentID: id},
			LockID: lid,
		}
	}
	acquire := func(u *TestUser, l rem.KVLock) error {
		m, cli, auth := s.rawFor(t, u)
		return cli.KvLockAcquire(m.Ctx(), rem.KvLockAcquireArg{
			Auth: auth, Lock: l, Timeout: proto.ExportDurationMilli(60_000_000_000),
		})
	}
	release := func(u *TestUser, l rem.KVLock) error {
		m, cli, auth := s.rawFor(t, u)
		return cli.KvLockRelease(m.Ctx(), rem.KvLockReleaseArg{Auth: auth, Lock: l})
	}

	l := lock(dir, entry.id)
	requireKVPerm(t, acquire(sc.alice.u, l), "a Leader must not take a lock on the store")
	require.NoError(t, acquire(s.sage, l))
	requireKVPerm(t, release(sc.alice.u, l), "a Leader must not release a Sage's lock")
	require.NoError(t, release(s.sage, l))

	// Control: an ordinary directory's locks are unchanged.
	ordinary, err := s.mkdir(t, sc.alice.u, rawDirOpts{})
	require.NoError(t, err)
	o := lock(ordinary, entry.id)
	require.NoError(t, acquire(sc.alice.u, o))
	require.NoError(t, release(sc.alice.u, o))
}

func TestKvSystemStoreLockedNotChannelStorage(t *testing.T) {
	sc := setupPrivScene(t, false) // alice owns the channel's ACL
	s := &sysScene{kvChannelScene: &kvChannelScene{sc: sc, pid: sc.teamID.ToPartyID()}}
	s.listSage(t, sc.alice.u)

	_, err := s.mkdir(t, sc.alice.u, rawDirOpts{locked: true, ch: &sc.chid})
	require.Error(t, err, "a directory must not be both locked and channel storage")

	// Control: each alone is allowed.
	_, err = s.mkdir(t, sc.alice.u, rawDirOpts{ch: &sc.chid})
	require.NoError(t, err)
	_, err = s.mkdir(t, sc.alice.u, rawDirOpts{locked: true})
	require.NoError(t, err)
}

func (s *sysScene) refcount(t *testing.T, dir proto.DirID) int {
	m := s.sc.tew.MetaContext()
	db, err := m.KVShard(s.pid)
	require.NoError(t, err)
	defer db.Release()
	var n int
	require.NoError(t, db.QueryRow(m.Ctx(),
		`SELECT refcount FROM dir_refcount
		 WHERE short_host_id=$1 AND short_party_id=$2 AND dir_id=$3`,
		int(m.ShortHostID()), s.pid.Shorten().ExportToDB(), dir.ExportToDB(),
	).Scan(&n))
	return n
}

func TestKvSystemRootSubmission(t *testing.T) {
	s := setupSysScene(t)
	sc := s.sc

	// A Leader who is not a Sage can make neither half of the store.
	_, err := s.mkdir(t, sc.alice.u, rawDirOpts{locked: true})
	requireKVPerm(t, err, "a non-Sage must not create a locked directory")

	dir, err := s.mkdir(t, s.sage, rawDirOpts{locked: true})
	require.NoError(t, err)
	root := s.rawRoot(t, dir)
	requireKVPerm(t, s.putSystemRoot(t, sc.alice.u, root), "a non-Sage must not submit a system root")

	// The directory must exist and be locked.
	require.Error(t, s.putSystemRoot(t, s.sage, s.rawRoot(t, randDirID(t))),
		"a directory that does not exist is not a system root")
	unlocked, err := s.mkdir(t, s.sage, rawDirOpts{})
	require.NoError(t, err)
	require.Error(t, s.putSystemRoot(t, s.sage, s.rawRoot(t, unlocked)),
		"an unlocked directory is not a system root")

	// A user store has no system root.
	mU := sc.tew.NewClientMetaContext(t, s.sage)
	userCli := kvStoreClientForUser(t, sc.tew, s.sage)
	requireKVPerm(t, userCli.KvPutSystemRoot(mU.Ctx(), rem.KvPutSystemRootArg{Root: root}),
		"a user party must not have a system root")

	require.NoError(t, s.putSystemRoot(t, s.sage, root))
	require.Equal(t, 1, s.refcount(t, dir), "the root holds a reference, so nothing collects it")

	// An identical replay succeeds and takes no second reference.
	require.NoError(t, s.putSystemRoot(t, s.sage, root))
	require.Equal(t, 1, s.refcount(t, dir), "a replay must not take a second reference")

	// A different directory, or the same one with different contents, loses.
	other, err := s.mkdir(t, s.sage, rawDirOpts{locked: true})
	require.NoError(t, err)
	err = s.putSystemRoot(t, s.sage, s.rawRoot(t, other))
	require.IsType(t, core.KVRaceError(""), err, "the root must never be replaced")
	err = s.putSystemRoot(t, s.sage, s.rawRoot(t, dir))
	require.IsType(t, core.KVRaceError(""), err, "a different binding is not a replay")

	m, cli, auth := s.rawFor(t, s.sage)
	got, err := cli.KvGetSystemRoot(m.Ctx(), auth)
	require.NoError(t, err)
	require.Equal(t, dir, got.Root, "the root must be unchanged")
	require.Equal(t, root.BindingMac, got.BindingMac)
	require.Equal(t, 0, s.refcount(t, other), "a losing directory takes no reference")
}

func TestKvSystemStoreMemberRead(t *testing.T) {
	s := setupSysScene(t)
	dir, entry, val := s.makeStore(t)
	bob := s.sc.bob.u

	m, cli, auth := s.rawFor(t, bob)
	root, err := cli.KvGetSystemRoot(m.Ctx(), auth)
	require.NoError(t, err)
	require.Equal(t, dir, root.Root)

	got, err := cli.KvGetDir(m.Ctx(), rem.KvGetDirArg{Auth: auth, Id: dir})
	require.NoError(t, err)
	require.True(t, got.Active.DaemonLock, "the lock travels on read")

	require.Equal(t, val, s.read(t, bob, entry).Value)
	_, err = cli.KvGetNode(m.Ctx(), rem.KvGetNodeArg{Auth: auth, Id: val})
	require.NoError(t, err)

	// And a member cannot write it, whatever the file's read role.
	requireKVPerm(t, s.put(t, bob, newRawEntry(t, dir), s.smallFile(t, bob)),
		"a member must not write to the store")
}

func TestKvSystemStoreAbsentRootIsNoent(t *testing.T) {
	s := setupSysScene(t)
	m, cli, auth := s.rawFor(t, s.sc.bob.u)
	_, err := cli.KvGetSystemRoot(m.Ctx(), auth)
	require.IsType(t, core.KVNoentError{}, err)
}

// TestKvSystemStoreSageBelowWriteRole: the list is not a role. A Sage demoted
// below the write role is refused by the role check, not by the list.
func TestKvSystemStoreSageBelowWriteRole(t *testing.T) {
	s := setupSysScene(t)
	dir, entry, val := s.makeStore(t)
	bob := s.sc.bob.u
	s.listSage(t, bob)

	_, err := s.mkdir(t, bob, rawDirOpts{locked: true})
	requireKVPerm(t, err, "a member Sage must not create a locked directory at admin write")

	requireKVPerm(t, s.put(t, bob, newRawEntry(t, dir), s.smallFile(t, bob)),
		"a member Sage must not write to an admin-write directory")

	next := entry
	next.vers = entry.vers + 1
	requireKVPerm(t, s.put(t, bob, next, s.smallFile(t, bob)),
		"a member Sage must not replace an entry")
	require.Equal(t, val, s.read(t, s.sage, entry).Value)

	require.Error(t, s.putSystemRoot(t, bob, s.rawRoot(t, randDirID(t))),
		"submitting a root needs admin as well as the list")
}

func TestKvSystemStoreSecondSage(t *testing.T) {
	s := setupSysScene(t)
	dir, entry, _ := s.makeStore(t)
	s.listSage(t, s.sage2)

	// A re-minted Sage needs no hand-over: it writes the store its
	// predecessor made, by the same rules.
	next := entry
	next.vers = entry.vers + 1
	val := s.smallFile(t, s.sage2)
	require.NoError(t, s.put(t, s.sage2, next, val))
	require.Equal(t, val, s.read(t, s.sage, entry).Value)
	require.NoError(t, s.put(t, s.sage2, newRawEntry(t, dir), s.smallFile(t, s.sage2)))

	// Removal is a Sage's to do as well, and a tombstone links nothing.
	gone := next
	gone.vers = next.vers + 1
	require.NoError(t, s.put(t, s.sage2, gone, tombstone()))
	require.True(t, s.read(t, s.sage, entry).Value.IsTombstone())

	// And the list is what mattered: an admin not on it is still refused.
	requireKVPerm(t, s.put(t, s.sc.dara.u, newRawEntry(t, dir), s.smallFile(t, s.sc.dara.u)),
		"an admin who is not a Sage must still be refused")
}

// TestKvSystemStoreNoActor drives the real trust boundary: a team acting as a
// member carries no verified actor, even when a Sage holds that team's keys.
func TestKvSystemStoreNoActor(t *testing.T) {
	s := setupSysScene(t)
	dir, _, _ := s.makeStore(t)
	sc := s.sc

	inner := sc.tew.makeTeamForOwner(t, s.sage)
	sc.tm.setIndexRange(t, sc.tew.MetaContext(), sc.alice.u, index3)
	inner.setIndexRange(t, sc.tew.MetaContext(), s.sage, index0)
	member := inner.toMemberRole(t, proto.OwnerRole, proto.AdminRole)
	sc.tm.absorb(inner.hepks)
	sc.tm.makeChanges(t, sc.tew.MetaContext(), sc.alice.u, []proto.MemberRole{member}, nil)
	sc.tew.DirectDoubleMerklePokeInTest(t)

	ctx := sc.tew.MetaContext().Ctx()
	tcli, closer := s.sage.newTeamLoaderClient(t, ctx, true)
	defer closer()
	req := rem.TeamVOBearerTokenReq{
		Team:    sc.tm.FQTeam(t).ToFQTeamIDOrName(),
		Member:  inner.FQTeam(t).FQParty(),
		Gen:     proto.FirstGeneration,
		SrcRole: proto.OwnerRole,
	}
	ch, err := tcli.GetTeamVOBearerTokenChallenge(ctx, req)
	require.NoError(t, err)
	ownerKey, err := core.ImportRole(proto.OwnerRole)
	require.NoError(t, err)
	ptk, ok := inner.ptks[*ownerKey]
	require.True(t, ok)
	sig, err := ptk.Sign(&ch)
	require.NoError(t, err)
	tok, err := tcli.ActivateTeamVOBearerToken(ctx, rem.ActivateTeamVOBearerTokenArg{Ch: ch, Sig: *sig})
	require.NoError(t, err)

	m, cli, _ := s.rawFor(t, s.sage)
	auth := rem.NewKVAuthWithTeam(tok.Tok)

	// The token works: the read path takes it.
	_, err = cli.KvGetDir(m.Ctx(), rem.KvGetDirArg{Auth: auth, Id: dir})
	require.NoError(t, err)

	// And every locked write refuses it.
	did := randDirID(t)
	_, err = cli.KvMkdir(m.Ctx(), rem.KvMkdirArg{
		Hdr: rem.KVReqHeader{Auth: auth},
		Dir: rawKVDir(t, did, rawDirOpts{locked: true}),
	})
	requireKVPerm(t, err, "no actor must not create a locked directory")
	err = cli.KvPutSystemRoot(m.Ctx(), rem.KvPutSystemRootArg{Auth: auth, Root: s.rawRoot(t, did)})
	requireKVPerm(t, err, "no actor must not submit a root")
}

// --- the client ---

func (s *sysScene) kvMinder(t *testing.T, u *TestUser) (libkv.MetaContext, *libkv.Minder) {
	m := libkv.NewMetaContext(s.sc.tew.NewClientMetaContextWithEracer(t, u))
	return m, libkv.NewMinderWithCacheSettings(m.G().ActiveUser(),
		libclient.CacheSettings{UseMem: true})
}

func (s *sysScene) sysCfg() lcl.KVConfig {
	return lcl.KVConfig{
		ActingAs:    s.sc.teamCfg(),
		Roles:       proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.AdminRole},
		OverwriteOk: true,
		SystemStore: true,
	}
}

// ordinaryCfg is the same request with the system store unselected.
func (s *sysScene) ordinaryCfg() lcl.KVConfig {
	cfg := s.sysCfg()
	cfg.SystemStore = false
	return cfg
}

func (s *sysScene) ordinaryRootExists(t *testing.T) bool {
	m := s.sc.tew.MetaContext()
	db, err := m.KVShard(s.pid)
	require.NoError(t, err)
	defer db.Release()
	var n int
	require.NoError(t, db.QueryRow(m.Ctx(),
		`SELECT COUNT(*) FROM root WHERE short_host_id=$1 AND party_id=$2`,
		int(m.ShortHostID()), s.pid.ExportToDB(),
	).Scan(&n))
	return n > 0
}

func requireSysRead(t *testing.T, kv *libkv.Minder, m libkv.MetaContext, cfg lcl.KVConfig, want []byte) {
	t.Helper()
	got, err := kv.GetFile(m, cfg, "/usage")
	require.NoError(t, err)
	require.Equal(t, want, []byte(got.Chunk.Chunk))
}

// TestKvSystemStoreClient is the client's contract end to end, starting from
// a team that has never had an ordinary root.
func TestKvSystemStoreClient(t *testing.T) {
	s := setupSysScene(t)
	sc := s.sc
	cfg := s.sysCfg()
	sage, sageKV := s.kvMinder(t, s.sage)

	// A store that does not exist yet reads as absent, and a read must not
	// create it.
	_, err := sageKV.GetFile(sage, cfg, "/usage")
	require.True(t, core.IsKVNoentError(err), "a missing store reads as absent, got %v", err)

	// First write creates it. No ordinary root is involved anywhere.
	require.False(t, s.ordinaryRootExists(t))
	body := []byte(`{"day":"2026-11-09","used":1}`)
	_, err = sageKV.PutFileFirst(sage, cfg, "/usage", body, true)
	require.NoError(t, err)
	requireSysRead(t, sageKV, sage, cfg, body)
	require.False(t, s.ordinaryRootExists(t),
		"the system store must not create an ordinary root")

	// A second read goes through the cache and the closing version check,
	// which is what used to need an ordinary root to exist.
	requireSysRead(t, sageKV, sage, cfg, body)

	// A member reads it through the same flag.
	bob, bobKV := s.kvMinder(t, sc.bob.u)
	requireSysRead(t, bobKV, bob, cfg, body)

	// A Leader writing through the same API is refused and changes nothing.
	alice, aliceKV := s.kvMinder(t, sc.alice.u)
	_, err = aliceKV.PutFileFirst(alice, cfg, "/usage", []byte("forged"), true)
	requireKVPerm(t, err, "a Leader must not write to the store")
	_, err = aliceKV.PutFileFirst(alice, cfg, "/other", []byte("forged"), true)
	requireKVPerm(t, err, "a Leader must not add to the store")
	requireSysRead(t, sageKV, sage, cfg, body)

	// A forged ordinary /usage is ignored. The name means nothing across the
	// two trees: the store's name MACs come from a different directory seed.
	ordinary := s.ordinaryCfg()
	_, err = aliceKV.PutFileFirst(alice, ordinary, "/usage", []byte("forged"), true)
	require.NoError(t, err)
	got, err := aliceKV.GetFile(alice, ordinary, "/usage")
	require.NoError(t, err)
	require.Equal(t, []byte("forged"), []byte(got.Chunk.Chunk))
	requireSysRead(t, bobKV, bob, cfg, body)
	bob2, bobKV2 := s.kvMinder(t, sc.bob.u)
	requireSysRead(t, bobKV2, bob2, cfg, body)

	// The ordinary tree's own churn cannot reach the store either.
	_, err = aliceKV.Mkdir(alice, ordinary, "/system")
	require.NoError(t, err)
	require.NoError(t, aliceKV.Unlink(alice, ordinary, "/usage"))
	requireSysRead(t, sageKV, sage, cfg, body)

	// A second Sage continues the same store.
	s.listSage(t, s.sage2)
	sage2, sage2KV := s.kvMinder(t, s.sage2)
	body2 := []byte(`{"day":"2026-11-09","used":2}`)
	_, err = sage2KV.PutFileFirst(sage2, cfg, "/usage", body2, true)
	require.NoError(t, err)
	requireSysRead(t, sageKV, sage, cfg, body2)
}

// TestKvSystemStoreClientRefusesOtherOps: every other local KV operation
// refuses the selection. One that silently ignored it would read a Leader's
// ordinary file as the real one.
func TestKvSystemStoreClientRefusesOtherOps(t *testing.T) {
	s := setupSysScene(t)
	cfg := s.sysCfg()
	m, kv := s.kvMinder(t, s.sage)
	var nid proto.FileID

	ops := map[string]func() error{
		"Mkdir":    func() error { _, err := kv.Mkdir(m, cfg, "/x"); return err },
		"Symlink":  func() error { _, err := kv.Symlink(m, cfg, "/x", "/y"); return err },
		"Readlink": func() error { _, err := kv.Readlink(m, cfg, "/x"); return err },
		"Stat":     func() error { _, err := kv.Stat(m, cfg, "/x"); return err },
		"Unlink":   func() error { return kv.Unlink(m, cfg, "/x") },
		"Mv":       func() error { return kv.Mv(m, cfg, "/x", "/y") },
		"List": func() error {
			_, err := kv.List(m, cfg, "/x", nil, rem.KVListOpts{})
			return err
		},
		"GetUsage":     func() error { _, err := kv.GetUsage(m, cfg); return err },
		"HostID":       func() error { _, err := kv.HostID(m, cfg); return err },
		"GetFileChunk": func() error { _, err := kv.GetFileChunk(m, cfg, nid, 0); return err },
		"PutFileChunk": func() error { return kv.PutFileChunk(m, cfg, nid, nil, 0, true) },
		"AcquireLock": func() error {
			_, err := kv.AcquireLock(m, cfg, proto.KVDirentIDPair{}, 0)
			return err
		},
		"ChannelMkRoot": func() error {
			_, err := kv.ChannelMkRoot(m, cfg, proto.RTChannelID{}, "/x")
			return err
		},
		"ListOutbox":  func() error { _, err := kv.ListOutbox(m, cfg); return err },
		"DrainOutbox": func() error { _, err := kv.DrainOutbox(m, cfg); return err },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			err := op()
			require.Error(t, err)
			require.Contains(t, err.Error(), "system store", "%s must refuse the selection", name)
		})
	}

	// The two that honour it still refuse what the store cannot hold.
	mkdirP := cfg
	mkdirP.MkdirP = true
	_, err := kv.PutFileFirst(m, mkdirP, "/a/b", []byte("x"), true)
	require.Error(t, err)
	big := make([]byte, 8192)
	_, err = kv.PutFileFirst(m, cfg, "/big", big, true)
	require.Error(t, err, "large files stay out of the system store")
}

// TestKvSystemStoreClientVerifiesRootBinding: the root record is the one
// thing a reader takes on the server's word to find the store, so its binding
// MAC is checked the same as an ordinary root's.
func TestKvSystemStoreClientVerifiesRootBinding(t *testing.T) {
	s := setupSysScene(t)
	cfg := s.sysCfg()
	m, kv := s.kvMinder(t, s.sage)
	_, err := kv.PutFileFirst(m, cfg, "/usage", []byte("real"), true)
	require.NoError(t, err)

	srv := s.sc.tew.MetaContext()
	db, err := srv.KVShard(s.pid)
	require.NoError(t, err)
	var mac proto.HMAC
	require.NoError(t, core.RandomFill(mac[:]))
	_, err = db.Exec(srv.Ctx(),
		`UPDATE system_root SET binding_mac=$1 WHERE short_host_id=$2 AND party_id=$3`,
		mac.ExportToDB(), int(srv.ShortHostID()), s.pid.ExportToDB())
	db.Release()
	require.NoError(t, err)

	fresh, freshKV := s.kvMinder(t, s.sage)
	_, err = freshKV.GetFile(fresh, cfg, "/usage")
	require.IsType(t, core.VerifyError(""), err)
}

func TestKvSystemStoreNoOutbox(t *testing.T) {
	s := setupSysScene(t)
	cfg := s.sysCfg()
	m, kv := s.kvMinder(t, s.sage)

	body := []byte("the saved count")
	_, err := kv.PutFileFirst(m, cfg, "/usage", body, true)
	require.NoError(t, err)

	down := core.NewConnectError("test: server unreachable", errors.New("down"))
	for _, op := range []string{"uploadNode", "putDirent"} {
		kv.SetTestHooks(&libkv.MinderTestHooks{PreRPC: func(got string) error {
			if got == op {
				return down
			}
			return nil
		}})
		_, err = kv.PutFileFirst(m, cfg, "/usage", []byte("newer count"), true)
		require.Error(t, err)
		require.True(t, core.IsTransportError(err), "%s: got %v", op, err)
		var queued core.KVWriteQueuedError
		require.False(t, errors.As(err, &queued),
			"%s: a counter write replayed later would overwrite newer counts", op)
	}
	kv.SetTestHooks(nil)

	plain := s.ordinaryCfg()
	rows, err := kv.ListOutbox(m, plain)
	require.NoError(t, err)
	require.Empty(t, rows, "nothing may be queued for later delivery")

	requireSysRead(t, kv, m, cfg, body)
}

// TestKvSystemStoreLostCreationRace: two Sages create a team's store at once.
// The loser uses the winner's and the write lands.
func TestKvSystemStoreLostCreationRace(t *testing.T) {
	s := setupSysScene(t)
	s.listSage(t, s.sage2)
	cfg := s.sysCfg()
	loser, loserKV := s.kvMinder(t, s.sage)
	winner, winnerKV := s.kvMinder(t, s.sage2)

	once := true
	loserKV.SetTestHooks(&libkv.MinderTestHooks{PreRPC: func(op string) error {
		if op == "uploadDir" && once {
			once = false
			_, err := winnerKV.PutFileFirst(winner, cfg, "/usage", []byte("winner"), true)
			require.NoError(t, err)
		}
		return nil
	}})

	_, err := loserKV.PutFileFirst(loser, cfg, "/usage", []byte("loser"), true)
	require.NoError(t, err)
	require.False(t, once, "the hook must have run")
	loserKV.SetTestHooks(nil)

	m := loser
	requireSysRead(t, winnerKV, winner, cfg, []byte("loser"))
	requireSysRead(t, loserKV, m, cfg, []byte("loser"))
}

func TestSignupWithDaemonCode(t *testing.T) {
	// Its own environment: the shared one's vhost setup copies that host's
	// single multi-use code and fails if it finds more.
	tew := ForkNewTestEnvWrapper(t)
	defer tew.Shutdown()
	m := tew.MetaContext()
	db, err := m.Db(shared.DbTypeUsers)
	require.NoError(t, err)
	defer db.Release()

	listed := func(u *TestUser) bool {
		var n int
		require.NoError(t, db.QueryRow(m.Ctx(),
			`SELECT COUNT(*) FROM daemon_accounts WHERE short_host_id=$1 AND uid=$2`,
			int(m.ShortHostID()), u.uid.ExportToDB(),
		).Scan(&n))
		return n == 1
	}

	flagged, err := common.InsertNewMutliuseInviteCode(m)
	require.NoError(t, err)
	_, err = db.Exec(m.Ctx(),
		`UPDATE multiuse_invite_codes SET daemon_signup=TRUE WHERE short_host_id=$1 AND code=$2`,
		int(m.ShortHostID()), flagged.Multiuse())
	require.NoError(t, err)
	ordinary, err := common.InsertNewMutliuseInviteCode(m)
	require.NoError(t, err)

	sage := tew.NewTestUserOpts(t, &TestUserOpts{InviteCode: flagged})
	require.True(t, listed(sage), "a signup with a flagged code is a Sage")

	// A code is reusable, and each signup lists its own account.
	sage2 := tew.NewTestUserOpts(t, &TestUserOpts{InviteCode: flagged})
	require.True(t, listed(sage2))

	person := tew.NewTestUserOpts(t, &TestUserOpts{InviteCode: ordinary})
	require.False(t, listed(person), "an ordinary signup is not a Sage")

	regular := tew.NewTestUserFakeRoot(t)
	require.False(t, listed(regular), "a signup with the default code is not a Sage")
}
