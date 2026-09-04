package realtime

// Private-channel access control (fork-only; see docs/rt-private-channel-acl.md).
//
// A private channel is an ordinary channel of its tier that ADDITIONALLY
// requires an explicit channel_acl row. The guarantee this buys is policy, not
// mathematics: the channel is still sealed with the parent team's key at the
// channel's read role, so every team member at that role holds the key and
// there is no rekey when channel membership changes. Against outsiders the
// protection is unchanged and cryptographic; against other members of the same
// community it is the server refusing to serve them the ciphertext. A gap in
// that policy exposes the channel's entire history, silently and
// retroactively, to anyone who kept their keys.
//
// That is why every server path that can return channel rows, message rows or
// channel activity metadata goes through authorizeChannel below, and why the
// two paths that cannot (the channel listing and the changed-threads sync, both
// set-based) embed the equivalent predicate and are tested against the same
// scenarios. See §5 of the spec for the full path inventory, and
// TestRealtimeRpcInventory for the guard that fails the build when a new RPC or
// a new query against these tables is added without being classified.

import (
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

// accessKind says what the caller wants to do with a channel; it selects which
// gates authorizeChannel applies.
type accessKind int

const (
	// accessRead: return channel rows, message rows, or channel activity
	// metadata. Requires the caller's team role to be at or above the
	// channel's read role, and (if private) an ACL row.
	accessRead accessKind = iota
	// accessWrite: send into the channel. Requires the write role, and (if
	// private) an ACL row.
	accessWrite
	// accessManage: change the channel's ACL. Requires (if private) an owner
	// ACL row, or team admin-or-above -- an admin's grant is visible to the
	// channel's members through channel_acl.granted_by, which is what makes
	// "leaders can join, and you can see that they did" honest.
	accessManage
	// accessRoster: read the channel's ACL. Every channel MEMBER may, which is
	// what makes an admin's self-grant visible to the channel rather than
	// invisible (§6.1) -- so this is weaker than accessManage. Team admins may
	// too, and must: an admin's authority to revoke is useless if they cannot
	// first see who is in the channel.
	accessRoster
)

// managementKind reports whether this access is about the ACL rather than the
// channel's contents. Both skip the read-role gate: a team admin has to be
// able to moderate a private channel whose read role sits above their own.
func (a accessKind) managementKind() bool {
	return a == accessManage || a == accessRoster
}

// Values stored in channel_acl.acl_role.
const (
	aclRoleMember = 0
	aclRoleOwner  = 1
)

// channelAuth is what the chokepoint loaded from the channels row plus what it
// decided about the caller.
type channelAuth struct {
	team      proto.TeamID
	appID     proto.RTAppID
	tier      proto.RTChannelTier
	readRole  proto.Role
	writeRole proto.Role
	private   bool

	// lastMsgSeq is the channel's denormalized last message seq; NULL (nil)
	// when the channel has no messages yet.
	lastMsgSeq *int64

	// teamRole is the caller's CURRENT role in the parent team -- re-read on
	// every call, so a user removed from (or demoted in) the team is denied
	// even though their user_channels row lingers.
	teamRole *core.RoleKey

	// aclOwner reports whether the caller holds an owner row in channel_acl.
	// Always false for a non-private channel, which has no ACL at all.
	aclOwner bool
}

// channelAuthCols is the column list authorizeChannel scans. Kept together so
// the locking and non-locking variants can never drift.
const channelAuthCols = `parent_team_id, app_id, tier, private,
	 read_role_type, read_role_viz_level,
	 write_role_type, write_role_viz_level,
	 last_msg_seq`

// authorizeChannel is the one chokepoint for per-channel access. No path may
// query `channels` or `messages_enc` on behalf of a caller without going
// through here (or, for the two set-based paths, the equivalent predicate --
// see privateVisibleToCaller).
//
// Pass lock=true to take the channels row lock (SELECT ... FOR UPDATE), which
// the send path needs to serialize seq assignment; rtdb must then be the
// send's transaction.
//
// Error discipline: a private channel that the caller is not in returns
// RowNotFoundError -- byte-identical to the error for a channel id that does
// not exist -- so a non-member cannot learn that a private channel exists.
func authorizeChannel(
	m shared.MetaContext,
	rtdb shared.Querier,
	userdb shared.Querier,
	channelID int64,
	want accessKind,
	lock bool,
) (
	*channelAuth, error,
) {
	// 1. Load the channel. Absent -> RowNotFound.
	var ca channelAuth
	var appRaw, tierRaw string
	var teamBytes []byte
	var rrt, rvl, wrt, wvl int
	q := `SELECT ` + channelAuthCols + `
		 FROM channels
		 WHERE short_host_id=$1 AND channel_id=$2`
	if lock {
		q += ` FOR UPDATE`
	}
	err := rtdb.QueryRow(m.Ctx(), q, m.ShortHostID(), channelID).Scan(
		&teamBytes, &appRaw, &tierRaw, &ca.private,
		&rrt, &rvl, &wrt, &wvl, &ca.lastMsgSeq,
	)
	if err == pgx.ErrNoRows {
		return nil, core.RowNotFoundError{}
	}
	if err != nil {
		return nil, err
	}
	if err = ca.team.ImportFromDB(teamBytes); err != nil {
		return nil, err
	}
	if err = ca.appID.ImportFromDB(appRaw); err != nil {
		return nil, err
	}
	if err = ca.tier.ImportFromDB(tierRaw); err != nil {
		return nil, err
	}
	if err = ca.readRole.ImportFromDB(rrt, rvl); err != nil {
		return nil, err
	}
	if err = ca.writeRole.ImportFromDB(wrt, wvl); err != nil {
		return nil, err
	}

	// 2. Outer gate: the caller must be a CURRENT member of the parent team.
	// This is what denies every read to an ex-team-member, private or not.
	role, err := AuthorizeUserForTeam(m, userdb, ca.team)
	if err != nil {
		return nil, err
	}
	ca.teamRole = role

	// 3. Private existence gate, and it runs FIRST -- before the tier and role
	// gates -- because those answer with PermissionError, which is itself a
	// disclosure: it says "a channel exists at this id". For a public channel
	// that is fine; for a private one, existence is the thing being hidden.
	// A caller below the read role of a private channel they are not in must
	// be unable to tell it from a channel id that was never issued.
	if ca.private {
		aclRole, found, err := readChannelAclRole(m, rtdb, channelID, m.UID())
		if err != nil {
			return nil, err
		}
		ca.aclOwner = found && aclRole == aclRoleOwner

		// A team admin may manage (and hence join) a private channel they are
		// not in -- Q1/Q2: leaders are transparently peers rather than
		// invisible ones. Everyone else needs an ACL row, and gets the
		// missing-channel error without one.
		if !found && !(want.managementKind() && role.IsAdminOrAbove()) {
			return nil, core.RowNotFoundError{}
		}
	}

	// 4. Tier gate: an admin-tier channel is invisible below admin. Implied by
	// the read-role gate for every channel librt creates (the tier is derived
	// from the read role), but stated here so the chokepoint does not depend
	// on that coincidence. Reached only by a caller who already holds an ACL
	// row (or is an admin managing), so it cannot disclose a private channel.
	if ca.tier == proto.RTChannelTier_Admin && !role.IsAdminOrAbove() {
		return nil, core.PermissionError("user role too low for an admin-tier channel")
	}

	// 5. Role gate, for reads and writes only.
	//
	// Management is deliberately NOT gated on the read role. A team admin must
	// be able to moderate -- above all, to revoke someone from -- a private
	// channel whose read role sits above their own; that is the whole of the
	// authority §6.1 gives them, and gating it on readability would silently
	// withdraw it for exactly the channels most likely to need moderating.
	// Management cannot be used to smuggle an ineligible reader in either way:
	// GrantChannelMember independently requires the GRANTEE to clear the read
	// role, so an admin below it cannot even grant themselves.
	switch want {
	case accessWrite:
		writeRole, err := core.ImportRole(ca.writeRole)
		if err != nil {
			return nil, err
		}
		if role.LessThan(*writeRole) {
			return nil, core.PermissionError("user role too low to send into channel")
		}
	case accessRead:
		readRole, err := core.ImportRole(ca.readRole)
		if err != nil {
			return nil, err
		}
		if role.LessThan(*readRole) {
			return nil, core.PermissionError("user role too low to read channel")
		}
	case accessManage, accessRoster:
		// see above
	}

	// 6. ACL authority.
	if want.managementKind() {
		if !ca.private {
			// Reached only after the tier gate, so this cannot be used to
			// probe for an admin-tier channel from below admin.
			return nil, core.BadArgsError("channel is not private; it has no ACL")
		}
		// Reading the roster needs no more than the membership (or admin
		// standing) established in step 3. Changing it needs ownership.
		if want == accessManage && !ca.aclOwner && !role.IsAdminOrAbove() {
			// The caller holds an ACL row, so they already know the channel
			// exists: there is nothing left to hide behind RowNotFound.
			return nil, core.PermissionError("must be a channel owner or team admin to manage membership")
		}
	}
	return &ca, nil
}

// authorizeChannelCreate is the create-time half of the chokepoint: at create
// time there is no channels row to load, so the ACL check of step 5 collapses
// to a single question about the creator's team role.
//
// Only admins/leaders may create a private channel (Q2b), enforced here rather
// than only in the UI -- a UI-only rule is not a rule. The accepted consequence
// is that ordinary members cannot create private subgroups in v1; §7.1 of the
// spec records that and the reasoning.
//
// This is deliberately the ONE authorization branch for "who may create a
// private channel". If a carve-out for system- or daemon-created channels is
// ever wanted, it belongs here and nowhere else. None exists today, and
// whether the daemon's per-member conversation should be a private channel at
// all is an open product question -- so this does not decide it either way.
func authorizeChannelCreate(role *core.RoleKey, private bool) error {
	if !private {
		return nil
	}
	if role == nil || !role.IsAdminOrAbove() {
		return core.PermissionError("only team admins may create a private channel")
	}
	return nil
}

// readChannelAclRole point-reads one (channel, user) ACL row.
func readChannelAclRole(
	m shared.MetaContext,
	db shared.Querier,
	channelID int64,
	uid proto.UID,
) (
	int, bool, error,
) {
	var aclRole int
	err := db.QueryRow(
		m.Ctx(),
		`SELECT acl_role FROM channel_acl
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		m.ShortHostID(),
		channelID,
		uid.ExportToDB(),
	).Scan(&aclRole)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return aclRole, true, nil
}

// privateVisibleToCaller is the set-based equivalent of authorizeChannel's
// step 5, for the two paths that read many channels at once and so cannot call
// a per-row function: the channel listing and the changed-threads inbox sync.
// It must stay in lock-step with step 5 -- the private-channel tests run the
// same scenarios through both, which is what keeps them honest.
//
// chAlias is the `channels` alias in the caller's query; uidParam is the
// placeholder holding the caller's uid. The host is correlated off chAlias
// rather than bound separately, so there is no host placeholder to pass.
func privateVisibleToCaller(chAlias, uidParam string) string {
	return `(NOT ` + chAlias + `.private OR EXISTS (
	     SELECT 1 FROM channel_acl acl
	     WHERE acl.short_host_id = ` + chAlias + `.short_host_id
	     AND acl.channel_id = ` + chAlias + `.channel_id
	     AND acl.uid = ` + uidParam + `))`
}

// insertChannelAcl writes (or re-writes) one ACL row. A re-grant is explicit:
// re-adding a user who was pruned by a team-leave writes a fresh granted_by
// and ctime rather than silently reviving the old grant.
func insertChannelAcl(
	m shared.MetaContext,
	tx pgx.Tx,
	channelID int64,
	uid proto.UID,
	aclRole int,
	grantedBy proto.UID,
) error {
	_, err := tx.Exec(
		m.Ctx(),
		`INSERT INTO channel_acl
			(short_host_id, channel_id, uid, acl_role, granted_by, ctime)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (short_host_id, channel_id, uid)
		 DO UPDATE SET acl_role=$4, granted_by=$5, ctime=NOW()`,
		m.ShortHostID(),
		channelID,
		uid.ExportToDB(),
		aclRole,
		grantedBy.ExportToDB(),
	)
	return err
}

// dropChannelMember removes a user from a private channel: the ACL row (the
// authority) and the user_channels row (the delivery state), in the caller's
// transaction so they commit together. Returns whether an ACL row was actually
// removed.
//
// bumpInbox additionally bumps the user's global inbox version, so their client
// notices on its next sync that the channel is gone. An explicit revoke wants
// that. The send-time team-leave prune does NOT: the user has left the team, so
// the outer gate already denies them everything and their rows are gone either
// way -- and taking their user_inbox lock mid-send would break the ascending-uid
// lock order that the two fanouts maintain to stay deadlock-free (a pruned uid
// can sort after a still-delivered one, so prune-then-fanout is not globally
// ascending, and RetryTx2 does not retry an error raised inside the tx).
func dropChannelMember(
	m shared.MetaContext,
	tx pgx.Tx,
	channelID int64,
	uid proto.UID,
	appDB string,
	bumpInbox bool,
) (
	bool, error,
) {
	tag, err := tx.Exec(
		m.Ctx(),
		`DELETE FROM channel_acl
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		m.ShortHostID(),
		channelID,
		uid.ExportToDB(),
	)
	if err != nil {
		return false, err
	}
	removed := tag.RowsAffected() > 0

	_, err = tx.Exec(
		m.Ctx(),
		`DELETE FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
		m.ShortHostID(),
		channelID,
		uid.ExportToDB(),
	)
	if err != nil {
		return false, err
	}

	if !bumpInbox {
		return removed, nil
	}

	// Bump, don't stamp: the revoked user has no user_channels row left to
	// stamp, so this version is a deliberate gap. It is what makes their next
	// sync return a head they haven't seen, and hence run the full sync in
	// which the channel is simply absent. The client-side rule (drop the
	// channel and its cached plaintext) is the follow-up client spec's; the
	// server cannot enforce it.
	_, err = tx.Exec(
		m.Ctx(),
		`INSERT INTO user_inbox (short_host_id, uid, app_id, inbox_version, mtime)
		 VALUES ($1, $2, $3, 1, NOW())
		 ON CONFLICT (short_host_id, uid, app_id)
		 DO UPDATE SET inbox_version = user_inbox.inbox_version + 1, mtime = NOW()`,
		m.ShortHostID(),
		uid.ExportToDB(),
		appDB,
	)
	if err != nil {
		return false, err
	}
	return removed, nil
}

// activeTeamMembers returns the subset of `uids` that are currently active,
// direct, local user-members of the team, with their roles. Same membership
// convention as AuthorizeUserForTeam, in one round trip.
func activeTeamMembers(
	m shared.MetaContext,
	userdb shared.Querier,
	team proto.TeamID,
	uids []proto.UID,
) (
	map[proto.UID]core.RoleKey,
	error,
) {
	ret := make(map[proto.UID]core.RoleKey, len(uids))
	if len(uids) == 0 {
		return ret, nil
	}
	ownerType, ownerViz, err := proto.OwnerRole.ExportToDB()
	if err != nil {
		return nil, err
	}
	raw := make([][]byte, len(uids))
	for i, u := range uids {
		raw[i] = u.ExportToDB()
	}
	rows, err := userdb.Query(
		m.Ctx(),
		`SELECT member_id, dst_role_type, dst_viz_level
		 FROM team_members
		 WHERE short_host_id=$1
		 AND team_id=$2
		 AND member_host_id=$3
		 AND src_role_type=$4
		 AND src_viz_level=$5
		 AND active=true
		 AND member_id = ANY($6)`,
		m.ShortHostID(),
		team.ExportToDB(),
		shared.ExportHostP(nil),
		ownerType,
		ownerViz,
		raw,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var memRaw []byte
		var dstType, dstViz int
		if err = rows.Scan(&memRaw, &dstType, &dstViz); err != nil {
			return nil, err
		}
		var pid proto.PartyID
		if err = pid.ImportFromDB(memRaw); err != nil {
			return nil, err
		}
		if !pid.IsUser() {
			continue
		}
		uid, err := pid.UID()
		if err != nil {
			return nil, err
		}
		rk, err := core.ImportRoleKeyFromDB(dstType, dstViz)
		if err != nil {
			return nil, err
		}
		ret[uid] = *rk
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return ret, nil
}

// pruneStaleChannelMembers is the team-leave cascade for private channels
// (§6.3). The outer gate already denies an ex-team-member every *read*; what
// it does not cover is *delivery* -- fanoutInboxVersions targets user_channels
// rows, which are not deleted on team leave, so an ex-member would keep
// receiving inbox bumps and content-free push wakes. (That is a pre-existing
// upstream behaviour for public channels too, and is being reported upstream
// rather than fixed here.)
//
// For a private channel the fix is exact and cheap, because private channels
// are small: at send time, re-validate every recipient against the team roster
// and drop the ACL + delivery rows of anyone who has left. A member removed
// from the team stops receiving anything from the first message after their
// removal.
//
// Two limits, both from the same laziness, both stated exactly rather than
// rounded up (§6.3):
//
//   - It runs at SEND time and nothing else prunes, so a member removed from
//     the team and re-admitted before the channel's next message keeps their
//     rows throughout and is still a member -- no re-grant needed. They read
//     nothing while out, because the outer gate re-checks the roster on every
//     access; this is membership surviving a round trip, not a read leak.
//   - team_members lives in the users DB and cannot join this transaction, so
//     a removal committing between the roster read and the fan-out lets that
//     one message bump the removed member's inbox version and queue a
//     content-free push wake. The next send prunes them. The residual is one
//     "something happened" signal, never content.
//
// Closing either would take an eager cross-database cascade on team removal --
// exactly the lifecycle coupling §7.2 rejects.
//
// Runs in the send's transaction, before the fan-out selects its recipients.
func pruneStaleChannelMembers(
	m shared.MetaContext,
	tx pgx.Tx,
	userdb shared.Querier,
	team proto.TeamID,
	channelID int64,
	appDB string,
) error {
	rows, err := tx.Query(
		m.Ctx(),
		`SELECT uid FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2
		 ORDER BY uid`,
		m.ShortHostID(),
		channelID,
	)
	if err != nil {
		return err
	}
	var uids []proto.UID
	for rows.Next() {
		var uidRaw []byte
		if err = rows.Scan(&uidRaw); err != nil {
			rows.Close()
			return err
		}
		var uid proto.UID
		if err = uid.ImportFromDB(uidRaw); err != nil {
			rows.Close()
			return err
		}
		uids = append(uids, uid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}

	live, err := activeTeamMembers(m, userdb, team, uids)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		if _, ok := live[uid]; ok {
			continue
		}
		if _, err := dropChannelMember(m, tx, channelID, uid, appDB, false); err != nil {
			return err
		}
	}
	return nil
}

// touchChannelSet bumps the parent team's channel-set version and stamps the
// channel at the new version, so the channel surfaces in the INCREMENTAL
// channel listing.
//
// Needed because a grant is otherwise invisible to rtListAllChannelsForTeam: a
// client sends its cached set version as `last`, ListAllChannels short-circuits
// to an empty list when that equals the current version, and readAllChannels
// filters on `updated_at_set_vers > last` -- a value written only at channel
// creation. A freshly granted member would therefore never see the channel in
// the list, and librt resolves channel names against exactly that list, so they
// could not send to or read the channel by name at all. Bumping here is what
// makes "the channel appears on their next sync" true for the listing as well
// as for the inbox sync.
//
// A concurrent rtNewChannel loses its optimistic-concurrency check against this
// bump and retries -- the same RTRaceError path two concurrent creates already
// take.
func touchChannelSet(
	m shared.MetaContext,
	tx pgx.Tx,
	team proto.TeamID,
	appDB string,
	channelID int64,
) error {
	var vers int
	err := tx.QueryRow(
		m.Ctx(),
		`UPDATE channel_sets SET vers = vers + 1, mtime = NOW()
		 WHERE short_host_id=$1 AND parent_team_id=$2 AND app_id=$3
		 RETURNING vers`,
		m.ShortHostID(),
		team.ExportToDB(),
		appDB,
	).Scan(&vers)
	if err == pgx.ErrNoRows {
		// No channel-set row means no channel was ever created under this
		// (team, app), which contradicts the channel we just authorized.
		return core.InternalError("no channel_sets row for a team that has channels")
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		m.Ctx(),
		`UPDATE channels SET updated_at_set_vers=$3, mtime=NOW()
		 WHERE short_host_id=$1 AND channel_id=$2`,
		m.ShortHostID(),
		channelID,
		vers,
	)
	return err
}

// GrantChannelMember adds (or re-grants) a user to a private channel's ACL and
// fans them in, so the channel appears on their next sync. ACL row and
// delivery row are written in one transaction: for a private channel the two
// sets are the same set, and every path depends on that.
func GrantChannelMember(
	m shared.MetaContext,
	arg rem.RtChannelGrantArg,
) error {
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	userdb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer userdb.Release()

	chid := arg.ChannelID.Short().Int64()
	aclRole := aclRoleMember
	if arg.Owner {
		aclRole = aclRoleOwner
	}

	return shared.RetryTx2(m,
		rtdb,
		"realtime.GrantChannelMember",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			ca, err := authorizeChannel(m, tx, userdb, chid, accessManage, false)
			if err != nil {
				return nil, err
			}
			// The grantee must be able to read the channel by the ordinary
			// rules too: a current team member at or above the read role.
			// Granting anyone else would break the invariant that a private
			// channel's ACL and its delivery rows are the same set -- and they
			// could not decrypt a thing anyway.
			live, err := activeTeamMembers(m, userdb, ca.team, []proto.UID{arg.Uid})
			if err != nil {
				return nil, err
			}
			memRole, ok := live[arg.Uid]
			if !ok {
				return nil, core.PermissionError("grantee is not a member of the parent team")
			}
			readRole, err := core.ImportRole(ca.readRole)
			if err != nil {
				return nil, err
			}
			if memRole.LessThan(*readRole) {
				return nil, core.PermissionError("grantee's team role is below the channel's read role")
			}
			appDB, err := ca.appID.ExportToDB()
			if err != nil {
				return nil, err
			}
			err = insertChannelAcl(m, tx, chid, arg.Uid, aclRole, m.UID())
			if err != nil {
				return nil, err
			}
			// Idempotent: a re-grant of an existing member (say, promoting
			// them to owner) leaves their delivery row alone.
			_, err = fanUserIntoChannel(m, tx, arg.Uid, appDB, arg.ChannelID.Short())
			if err != nil {
				return nil, err
			}
			// Make the channel visible to the grantee's incremental channel
			// listing, not just to their inbox sync; see touchChannelSet.
			err = touchChannelSet(m, tx, ca.team, appDB, chid)
			if err != nil {
				return nil, err
			}
			app := ca.appID
			uid := arg.Uid
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, app, []proto.UID{uid})
			}, nil
		},
	)
}

// RevokeChannelMember removes a user from a private channel: ACL row, delivery
// row, and an inbox bump so their client drops the channel on its next sync.
//
// It does NOT rekey the team, so a revoked member who kept their keys can still
// decrypt any ciphertext they already hold, and could decrypt future messages
// if they ever obtained them. That is the accepted v1 consequence of doing this
// with policy rather than per-channel keys (Q4); the policy is the whole
// defence.
func RevokeChannelMember(
	m shared.MetaContext,
	arg rem.RtChannelRevokeArg,
) error {
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	userdb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer userdb.Release()

	chid := arg.ChannelID.Short().Int64()

	return shared.RetryTx2(m,
		rtdb,
		"realtime.RevokeChannelMember",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			ca, err := authorizeChannel(m, tx, userdb, chid, accessManage, false)
			if err != nil {
				return nil, err
			}
			appDB, err := ca.appID.ExportToDB()
			if err != nil {
				return nil, err
			}
			removed, err := dropChannelMember(m, tx, chid, arg.Uid, appDB, true)
			if err != nil {
				return nil, err
			}
			if !removed {
				return nil, core.RowNotFoundError{}
			}
			app := ca.appID
			uid := arg.Uid
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, app, []proto.UID{uid})
			}, nil
		},
	)
}

// ListChannelMembers returns a private channel's ACL.
//
// Gated at accessRoster, which is weaker than accessManage on purpose: §6.1
// turns on every MEMBER being able to see who is in the channel and who let
// them in, since that is what makes a team admin's self-grant transparent
// rather than invisible. It is also why this is not accessRead -- a team admin
// below the channel's read role must still be able to enumerate it, or their
// authority to revoke someone is useless for want of knowing who is there.
// Non-members still get the missing-channel error from the chokepoint.
func ListChannelMembers(
	m shared.MetaContext,
	channelID proto.RTChannelID,
) (
	[]rem.RTChannelAclEntry,
	error,
) {
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return nil, err
	}
	defer rtdb.Release()
	userdb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return nil, err
	}
	defer userdb.Release()

	chid := channelID.Short().Int64()
	_, err = authorizeChannel(m, rtdb, userdb, chid, accessRoster, false)
	if err != nil {
		return nil, err
	}
	rows, err := rtdb.Query(
		m.Ctx(),
		`SELECT uid, acl_role, granted_by, ctime FROM channel_acl
		 WHERE short_host_id=$1 AND channel_id=$2
		 ORDER BY ctime ASC, uid ASC`,
		m.ShortHostID(),
		chid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ret := []rem.RTChannelAclEntry{}
	for rows.Next() {
		var uidRaw, grantedByRaw []byte
		var aclRole int
		var ctime time.Time
		if err = rows.Scan(&uidRaw, &aclRole, &grantedByRaw, &ctime); err != nil {
			return nil, err
		}
		var e rem.RTChannelAclEntry
		if err = e.Uid.ImportFromDB(uidRaw); err != nil {
			return nil, err
		}
		if err = e.GrantedBy.ImportFromDB(grantedByRaw); err != nil {
			return nil, err
		}
		e.Owner = aclRole == aclRoleOwner
		e.Ctime = proto.ExportTime(ctime)
		ret = append(ret, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return ret, nil
}
