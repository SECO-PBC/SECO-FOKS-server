// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

// The team system store (fork-only; docs/kv-system-store.md).
//
// A system store is a KV tree whose root the server tracks in system_root and
// which no dirent reaches, so nothing in the team's ordinary tree can name it.
// Its directories carry dir.daemon_locked, and only an account on the Sage
// list may change one. This is policy, not cryptography: the store is sealed
// with the team key at its read role, so every Leader holds the key. What
// stands between a Leader and the store is the server refusing the write, the
// same guarantee every FOKS write check gives.
//
// Hence one chokepoint, authorizeLockedWrite. It answers one question: is the
// verified actor on the Sage list? It never replaces a role check. Those run
// beside it, so a Sage must also hold the write role in that team.
//
// Locked directories are held to one tree in both directions. Only a locked
// parent and system_root may refer to a locked directory: a link from the
// ordinary tree would make Sage a confused deputy, its next ordinary write
// landing in the store and passing this gate. And a locked parent may not
// refer to an unlocked directory, which would be a hole any Leader could write.

import (
	"bytes"
	"context"
	"errors"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

type kvSageListKey struct{}

// withSageList records the users-database connection auth already holds for
// this request. The Sage list lives in that database, and a nested acquire
// from a pool the request is already holding a connection in is how a loaded
// server wedges itself. Only team auth with a verified actor sets it.
func withSageList(m shared.MetaContext, db shared.Querier) shared.MetaContext {
	return m.WithContext(context.WithValue(m.Ctx(), kvSageListKey{}, db))
}

// kvLockDenied is what a refused locked write looks like from outside.
func kvLockDenied() error {
	return core.KVPermssionError{
		KVPermError: proto.KVPermError{
			Op:       proto.KVOp_Write,
			Resource: proto.KVNodeType_Dir,
		},
	}
}

// authorizeLockedWrite passes only for a verified actor on the Sage list, in a
// team store. Every other caller is refused: a remote member and a team acting
// as a member carry no actor, and none means no.
func authorizeLockedWrite(m shared.MetaContext, pid proto.PartyID) error {
	if _, err := pid.TeamID(); err != nil {
		return kvLockDenied()
	}
	uid := actorUID(m)
	if uid == nil {
		return kvLockDenied()
	}
	rq, _ := m.Ctx().Value(kvSageListKey{}).(shared.Querier)
	if rq == nil {
		return kvLockDenied()
	}
	var one int
	err := rq.QueryRow(
		m.Ctx(),
		`SELECT 1 FROM daemon_accounts WHERE short_host_id=$1 AND uid=$2`,
		int(m.ShortHostID()), uid.ExportToDB(),
	).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return kvLockDenied()
	}
	return err
}

// loadDirDaemonLocked reads a directory's lock. A directory with no row reads
// as unlocked: nothing can link it anyway, since the refcount update fails.
func loadDirDaemonLocked(
	m shared.MetaContext,
	rq shared.Querier,
	pid proto.PartyID,
	dir proto.DirID,
) (bool, error) {
	var locked bool
	err := rq.QueryRow(
		m.Ctx(),
		`SELECT daemon_locked FROM dir
		 WHERE short_host_id=$1 AND short_party_id=$2 AND dir_id=$3
		 ORDER BY version DESC LIMIT 1`,
		int(m.ShortHostID()), pid.Shorten().ExportToDB(), dir.ExportToDB(),
	).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return locked, nil
}

// checkLockContainment refuses a dirent whose value is a directory on the
// other side of the lock from its parent. Files and tombstones are not
// directories and cross freely; a file's content cannot be overwritten, and
// the entry naming it is what the lock protects.
func checkLockContainment(
	m shared.MetaContext,
	rq shared.Querier,
	pid proto.PartyID,
	parentLocked bool,
	child proto.KVNodeID,
) error {
	typ, err := child.Type()
	if err != nil {
		return err
	}
	if typ != proto.KVNodeType_Dir {
		return nil
	}
	did, err := child.ToDirID()
	if err != nil {
		return err
	}
	childLocked, err := loadDirDaemonLocked(m, rq, pid, *did)
	if err != nil {
		return err
	}
	if childLocked != parentLocked {
		return kvLockDenied()
	}
	return nil
}

// getSystemRoot returns the team's system root. Absent is KVNoentError, the
// answer an ordinary root gives.
func getSystemRoot(
	m shared.MetaContext,
	rq shared.Querier,
	pid proto.PartyID,
	role proto.Role,
) (
	*proto.KVRoot,
	error,
) {
	var dir, bm []byte
	var v, ptkg, rt, vl int
	err := rq.QueryRow(m.Ctx(),
		`SELECT dir_id, version, binding_mac, ptk_gen, read_role_type, read_role_viz_level
		 FROM system_root
		 WHERE short_host_id=$1 AND party_id=$2`,
		int(m.ShortHostID()),
		pid.ExportToDB(),
	).Scan(&dir, &v, &bm, &ptkg, &rt, &vl)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, core.KVNoentError{}
	}
	if err != nil {
		return nil, err
	}
	ret := proto.KVRoot{
		Vers: proto.KVVersion(v),
		Rg: proto.RoleAndGen{
			Gen: proto.Generation(ptkg),
		},
	}
	err = ret.Root.ImportFromDB(dir)
	if err != nil {
		return nil, err
	}
	err = ret.BindingMac.ImportFromDB(bm)
	if err != nil {
		return nil, err
	}
	err = ret.Rg.Role.ImportFromDB(rt, vl)
	if err != nil {
		return nil, err
	}
	err = assertAtOrAbove(role, ret.Rg.Role, proto.KVOp_Read, proto.KVNodeType_Dir)
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

// putSystemRoot records a team's system root. It is insert-only: there is no
// path that replaces or removes the row, which is what lets a reader trust
// that the store it found is the store Sage created.
func putSystemRoot(
	m shared.MetaContext,
	tx pgx.Tx,
	pid proto.PartyID,
	role proto.Role,
	arg rem.KvPutSystemRootArg,
) error {
	if _, err := pid.TeamID(); err != nil {
		return kvLockDenied()
	}
	err := assertAtOrAbove(role, proto.AdminRole, proto.KVOp_Write, proto.KVNodeType_Dir)
	if err != nil {
		return err
	}
	err = authorizeLockedWrite(m, pid)
	if err != nil {
		return err
	}
	if arg.Root.BindingMac.IsZero() {
		return core.BadArgsError("binding mac must not be zero")
	}
	if arg.Root.Vers != proto.KVVersion(1) {
		return core.BadArgsError("system root version must be 1")
	}
	locked, err := loadDirDaemonLocked(m, tx, pid, arg.Root.Root)
	if err != nil {
		return err
	}
	if !locked {
		return core.BadArgsError("system root must be an existing locked directory")
	}
	rk, err := core.ImportRole(arg.Root.Rg.Role)
	if err != nil {
		return err
	}

	tag, err := tx.Exec(m.Ctx(),
		`INSERT INTO system_root(short_host_id, party_id, dir_id, version, ptk_gen,
			read_role_type, read_role_viz_level, binding_mac, ctime)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		 ON CONFLICT DO NOTHING`,
		int(m.ShortHostID()),
		pid.ExportToDB(),
		arg.Root.Root.ExportToDB(),
		int(arg.Root.Vers),
		int(arg.Root.Rg.Gen),
		int(rk.Typ),
		int(rk.Lev),
		arg.Root.BindingMac.ExportToDB(),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		// The refcount keeps the store from ever being collected.
		return dirIncref(m, tx, pid, arg.Root.Root)
	}

	var dir, bm []byte
	var gen, rt, vl int
	err = tx.QueryRow(m.Ctx(),
		`SELECT dir_id, binding_mac, ptk_gen, read_role_type, read_role_viz_level
		 FROM system_root
		 WHERE short_host_id=$1 AND party_id=$2`,
		int(m.ShortHostID()),
		pid.ExportToDB(),
	).Scan(&dir, &bm, &gen, &rt, &vl)
	if err != nil {
		return err
	}
	if bytes.Equal(dir, arg.Root.Root.ExportToDB()) &&
		bytes.Equal(bm, arg.Root.BindingMac.ExportToDB()) &&
		gen == int(arg.Root.Rg.Gen) &&
		rt == int(rk.Typ) && vl == int(rk.Lev) {
		return nil
	}
	return core.KVRaceError("team already has a system root")
}
