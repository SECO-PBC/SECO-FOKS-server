// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

// Channel-scoped storage access control (fork-only; docs/kv-channel-acl.md).
//
// A node whose row carries a non-NULL channel_id belongs to a realtime
// private channel of the team that owns this KV store. The guarantee is the
// same one the realtime message ACL gives, and no stronger: the data is
// sealed with the TEAM's key at the node's read role, so every team member
// at that role holds the key, and what stands between a non-member and the
// plaintext is this file refusing to serve the bytes. A gap here exposes a
// channel's entire storage, silently and retroactively, to anyone who kept
// their keys.
//
// Hence one chokepoint. Every read of a dir, dirent, small file, large file
// or version number funnels through one of the loaders in this package, and
// each loader calls authorizeKVNodeRead immediately after its row loads --
// before its role check, so a non-member below the read role learns nothing
// a non-member above it would not -- and maps errKVNodeMasked to exactly the
// shape it answers for a row that does not exist. Existence is never
// disclosed.
//
// The acting user is the one the KV auth proved, not the connection's: team
// auth reaches this server over possibly-anonymous connections (RequireAuth
// is AuthTypeNone), and the verified identity there is the member named in
// the bearer token's signed challenge. auth() stashes it on the context;
// a team-as-member or remote-member token yields no actor, and tagged nodes
// then fail closed.

import (
	"bytes"
	"context"
	"errors"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// eqChannelTag compares two nullable channel tags.
func eqChannelTag(a *int64, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// errKVNodeMasked says the caller may not know this node exists. It never
// leaves the package: every call site maps it to its own missing-row answer.
// Infrastructure errors from the check pass through unchanged.
var errKVNodeMasked = errors.New("kv node masked by channel acl")

// Values stored in channel_acl.acl_role (server/sql/foks_realtime.sql).
const kvAclRoleOwner = 1

type kvActorKey struct{}

// withActor records the acting user for channel-ACL checks downstream.
func withActor(m shared.MetaContext, uid *proto.UID) shared.MetaContext {
	return m.WithContext(context.WithValue(m.Ctx(), kvActorKey{}, uid))
}

func actorUID(m shared.MetaContext) *proto.UID {
	uid, _ := m.Ctx().Value(kvActorKey{}).(*proto.UID)
	return uid
}

// authorizeKVNodeRead gates a read of a node whose row carried channelID.
// nil channelID is ordinary storage and always passes. Per Q1
// (docs/kv-channel-acl.md §10) there is no admin bypass: membership in
// channel_acl, full stop. Denials are errKVNodeMasked.
func authorizeKVNodeRead(
	m shared.MetaContext,
	pid proto.PartyID,
	channelID *int64,
) error {
	if channelID == nil {
		return nil
	}
	return kvChannelMembership(m, pid, *channelID)
}

// kvChannelMembership is the membership probe every non-management check
// shares: the acting user holds a channel_acl row for a channel of the team
// whose store this is. Absence is errKVNodeMasked; the caller decides what
// that looks like from outside.
func kvChannelMembership(
	m shared.MetaContext,
	pid proto.PartyID,
	channelID int64,
) error {
	uid, tid, err := kvChannelCheckSetup(m, pid)
	if err != nil {
		return err
	}
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	var one int
	err = rtdb.QueryRow(
		m.Ctx(),
		`SELECT 1
		 FROM channel_acl AS a
		 JOIN channels AS c USING (short_host_id, channel_id)
		 WHERE a.short_host_id=$1 AND a.channel_id=$2 AND a.uid=$3
		   AND c.parent_team_id=$4`,
		int(m.ShortHostID()), channelID, uid.ExportToDB(), tid.ExportToDB(),
	).Scan(&one)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return errKVNodeMasked
	}
	return err
}

// authorizeKVNodeCreate gates tagging a NEW node as a channel's storage, and
// returns the value to write into its channel_id column: nil for ordinary
// storage, which is every call that names no channel and the whole of
// upstream's behavior.
//
// Creation is membership-gated, and that is what makes §7 H3 of
// docs/kv-channel-acl.md hold: a node is tagged at the instant it exists, so
// there is no window in which a directory or file holds channel data
// untagged, waiting to be linked. A caller who is not a member cannot create
// a tagged node at all, so they cannot manufacture one to smuggle in later.
func authorizeKVNodeCreate(
	m shared.MetaContext,
	pid proto.PartyID,
	channelID *proto.RTChannelID,
) (*int64, error) {
	if channelID == nil {
		return nil, nil
	}
	chid := channelID.Short().Int64()
	err := kvChannelMembership(m, pid, chid)
	if errors.Is(err, errKVNodeMasked) {
		// Same answer as a channel that does not exist.
		return nil, core.NotFoundError("channel")
	}
	if err != nil {
		return nil, err
	}
	return &chid, nil
}

// loadNodeChannelTag reads the channel tag of an existing node of any kind.
// A node with no row yet -- the client may link a value it is about to
// write, or that a sibling transaction is writing -- reads as untagged, and
// containment then holds it to the untagged tree, which is where a node
// nobody has claimed belongs.
func loadNodeChannelTag(
	m shared.MetaContext,
	rq shared.Querier,
	pid proto.PartyID,
	id proto.KVNodeID,
) (*int64, error) {
	typ, err := id.Type()
	if err != nil {
		return nil, err
	}
	var q string
	var key []byte
	switch typ {
	case proto.KVNodeType_Dir:
		did, err := id.ToDirID()
		if err != nil {
			return nil, err
		}
		q = `SELECT channel_id FROM dir
		     WHERE short_host_id=$1 AND short_party_id=$2 AND dir_id=$3
		     ORDER BY version DESC LIMIT 1`
		key = did.ExportToDB()
	case proto.KVNodeType_File:
		fid, err := id.ToFileID()
		if err != nil {
			return nil, err
		}
		q = `SELECT channel_id FROM large_file
		     WHERE short_host_id=$1 AND short_party_id=$2 AND file_id=$3`
		key = fid.ExportToDB()
	case proto.KVNodeType_SmallFile, proto.KVNodeType_Symlink:
		q = `SELECT channel_id FROM small_file_or_symlink
		     WHERE short_host_id=$1 AND short_party_id=$2 AND node_id=$3`
		key = id.ExportToDB()
	default:
		return nil, nil
	}
	var chid *int64
	err = rq.QueryRow(
		m.Ctx(), q,
		int(m.ShortHostID()), pid.Shorten().ExportToDB(), key,
	).Scan(&chid)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return chid, nil
}

// checkChannelContainment enforces the invariant that makes a channel's
// storage a subtree rather than a scattering of tagged rows: a node may only
// be linked under a parent carrying the same tag.
//
// The single exception is a channel's registered storage root, which by
// definition hangs beneath the untagged community tree -- and only the
// directory kvChannelMkRoot recorded qualifies, so a member cannot mint a
// second entrance to the channel's subtree from anywhere else.
//
// Both directions are refused. A tagged child under an untagged parent would
// put channel data where the ACL does not reach, and an untagged child under
// a tagged parent would be a hole in the other direction: content inside a
// channel's tree that anyone in the community may read. A tombstone links
// nothing and is exempt.
func checkChannelContainment(
	m shared.MetaContext,
	rq shared.Querier,
	pid proto.PartyID,
	parent *int64,
	child proto.KVNodeID,
) error {
	if child.IsTombstone() {
		return nil
	}
	childTag, err := loadNodeChannelTag(m, rq, pid, child)
	if err != nil {
		return err
	}
	switch {
	case parent == nil && childTag == nil:
		return nil
	case parent != nil && childTag != nil && *parent == *childTag:
		return nil
	case parent == nil && childTag != nil:
		// The one legal crossing: the channel's registered root.
		return assertIsChannelRoot(m, rq, pid, *childTag, child)
	}
	return core.KVPermssionError{
		KVPermError: proto.KVPermError{
			Op:       proto.KVOp_Write,
			Resource: proto.KVNodeType_Dir,
		},
	}
}

// assertIsChannelRoot passes only for the exact directory registered as this
// channel's storage root.
func assertIsChannelRoot(
	m shared.MetaContext,
	rq shared.Querier,
	pid proto.PartyID,
	channelID int64,
	child proto.KVNodeID,
) error {
	denied := core.KVPermssionError{
		KVPermError: proto.KVPermError{
			Op:       proto.KVOp_Write,
			Resource: proto.KVNodeType_Dir,
		},
	}
	typ, err := child.Type()
	if err != nil {
		return err
	}
	if typ != proto.KVNodeType_Dir {
		return denied
	}
	did, err := child.ToDirID()
	if err != nil {
		return err
	}
	var stored []byte
	err = rq.QueryRow(
		m.Ctx(),
		`SELECT dir_id FROM channel_kv_root
		 WHERE short_host_id=$1 AND short_party_id=$2 AND channel_id=$3`,
		int(m.ShortHostID()), pid.Shorten().ExportToDB(), channelID,
	).Scan(&stored)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return denied
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, did.ExportToDB()) {
		return denied
	}
	return nil
}

// authorizeKVNodeManage gates registration of a channel's storage root: an
// ACL owner of the channel, or a team admin-or-above -- the same standing
// rtChannelGrant demands, so storage management is never quieter than
// channel management. The channel must belong to the team whose store this
// is, and must be private; anything else answers as if it did not exist.
func authorizeKVNodeManage(
	m shared.MetaContext,
	pid proto.PartyID,
	channelID int64,
	role proto.Role,
) error {
	uid, tid, err := kvChannelCheckSetup(m, pid)
	if err != nil {
		return err
	}
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	var private bool
	var aclRole *int
	err = rtdb.QueryRow(
		m.Ctx(),
		`SELECT c.private, a.acl_role
		 FROM channels AS c
		 LEFT JOIN channel_acl AS a ON (
			a.short_host_id=c.short_host_id AND
			a.channel_id=c.channel_id AND
			a.uid=$3
		 )
		 WHERE c.short_host_id=$1 AND c.channel_id=$2 AND c.parent_team_id=$4`,
		int(m.ShortHostID()), channelID, uid.ExportToDB(), tid.ExportToDB(),
	).Scan(&private, &aclRole)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return errKVNodeMasked
	}
	if err != nil {
		return err
	}
	if !private {
		return errKVNodeMasked
	}
	if aclRole != nil && *aclRole == kvAclRoleOwner {
		return nil
	}
	adm, err := role.IsAdminOrAbove()
	if err != nil {
		return err
	}
	if adm {
		return nil
	}
	return errKVNodeMasked
}

// kvChannelCheckSetup resolves the two identities every channel check needs:
// the acting local user, and the team whose store this is. Channel storage
// exists only in team stores, so a tagged node reached via a user party, or
// with no usable actor, fails closed.
func kvChannelCheckSetup(
	m shared.MetaContext,
	pid proto.PartyID,
) (
	*proto.UID,
	*proto.TeamID,
	error,
) {
	tid, err := pid.TeamID()
	if err != nil {
		return nil, nil, errKVNodeMasked
	}
	uid := actorUID(m)
	if uid == nil {
		return nil, nil, errKVNodeMasked
	}
	return uid, &tid, nil
}

// registerChannelRoot writes the channel_kv_root row for KvChannelMkRoot,
// idempotently: registering the same root twice is a replay and succeeds,
// a different root for the same channel is refused.
func registerChannelRoot(
	m shared.MetaContext,
	db *pgxpool.Conn,
	pid proto.PartyID,
	channelID int64,
	dirID proto.DirID,
) error {
	tag, err := db.Exec(
		m.Ctx(),
		`INSERT INTO channel_kv_root
		    (short_host_id, short_party_id, channel_id, dir_id, ctime)
		 VALUES($1, $2, $3, $4, NOW())
		 ON CONFLICT DO NOTHING`,
		int(m.ShortHostID()), pid.Shorten().ExportToDB(), channelID,
		dirID.ExportToDB(),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var existing []byte
	err = db.QueryRow(
		m.Ctx(),
		`SELECT dir_id FROM channel_kv_root
		 WHERE short_host_id=$1 AND short_party_id=$2 AND channel_id=$3`,
		int(m.ShortHostID()), pid.Shorten().ExportToDB(), channelID,
	).Scan(&existing)
	if err != nil {
		return err
	}
	if bytes.Equal(existing, dirID.ExportToDB()) {
		return nil
	}
	return core.BadArgsError("channel already has a storage root")
}
