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
		int(m.ShortHostID()), *channelID, uid.ExportToDB(), tid.ExportToDB(),
	).Scan(&one)
	if err != nil && errors.Is(err, pgx.ErrNoRows) {
		return errKVNodeMasked
	}
	return err
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
