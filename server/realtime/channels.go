package realtime

import (
	"bytes"
	"slices"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

func ListAllChannels(
	m shared.MetaContext,
	team proto.TeamID,
	app proto.RTAppID,
	last proto.RTChannelSetVersion,
) (
	*rem.RTChannelSet,
	error,
) {
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return nil, err
	}
	defer rtdb.Release()
	udb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return nil, err
	}
	defer udb.Release()

	role, err := AuthorizeUserForTeam(m, udb, team)
	if err != nil {
		return nil, err
	}

	vers, mtime, err := readChannelSet(m, rtdb, team, app)
	if err != nil {
		return nil, err
	}
	var ret rem.RTChannelSet
	ret.Mtime = mtime
	ret.Vers = vers

	// User already has a fresh version, so there's no need to send the channel
	// list back. Still return the current version (and an empty list) so the
	// caller can confirm its cache is current rather than collapsing to v0.
	if vers == last {
		ret.Lst = []rem.RTChannelMetadata{}
		return &ret, nil
	}

	lst, err := readAllChannels(m, rtdb, team, app, *role, last)
	if err != nil {
		return nil, err
	}
	ret.Lst = lst
	return &ret, nil
}

func readChannelSet(
	m shared.MetaContext,
	db shared.Querier,
	teamID proto.TeamID,
	appID proto.RTAppID,
) (
	proto.RTChannelSetVersion,
	proto.Time,
	error,
) {
	var vers int
	var mtime time.Time

	var retVers proto.RTChannelSetVersion
	var retTime proto.Time

	app, err := appID.ExportToDB()
	if err != nil {
		return retVers, retTime, err
	}

	err = db.QueryRow(
		m.Ctx(),
		`SELECT vers, mtime FROM channel_sets
		 WHERE short_host_id=$1 AND parent_team_id=$2 AND app_id=$3`,
		m.ShortHostID().ExportToDB(),
		teamID.ExportToDB(),
		app,
	).Scan(&vers, &mtime)
	// No channel_sets row means no channel has ever been created under this
	// (team, app): version 0, which is what a first create expects to see.
	if err == pgx.ErrNoRows {
		return retVers, retTime, nil
	}
	// Any other error must propagate. Returning a nil error here reported
	// version 0 for a team whose set may be at version 40, so the caller's
	// next create proposed version 1 and lost the CAS -- or, worse, a client
	// cached 0 and re-listed from scratch on every sync.
	if err != nil {
		return retVers, retTime, err
	}
	return proto.RTChannelSetVersion(vers), proto.ExportTime(mtime), nil
}

func readAllChannels(
	m shared.MetaContext,
	db shared.Querier,
	team proto.TeamID,
	app proto.RTAppID,
	role core.RoleKey,
	last proto.RTChannelSetVersion,
) (
	[]rem.RTChannelMetadata,
	error,
) {

	// Every can read channel tier "bottom", but only
	// admins and above can read "admin" channels.
	var tiers []string = []string{"bottom"}
	if role.IsAdminOrAbove() {
		tiers = append(tiers, "admin")
	}
	appDB, err := app.ExportToDB()
	if err != nil {
		return nil, err
	}
	// Inventory row 5. This is one of the two set-based paths that cannot call
	// authorizeChannel per row, so it embeds the equivalent of the
	// chokepoint's step 5: a private channel is listed only to callers holding
	// a channel_acl row -- plus, per Q1, to team admins, who may see that a
	// private channel exists (and then grant themselves into it, visibly).
	// The private-channel tests run the same scenarios through this predicate
	// and through the chokepoint; keep the two in lock-step.
	privateGate := privateVisibleToCaller("c", "$6")
	if role.IsAdminOrAbove() {
		privateGate = "TRUE"
	}
	rows, err := db.Query(
		m.Ctx(),
		`SELECT `+channelMetadataCols+channelForkCols("$6")+`
		 FROM channels c
		 `+lastSenderJoin+`
		 WHERE c.short_host_id=$1
		 AND c.parent_team_id=$2
		 AND c.app_id=$3
		 AND c.tier = ANY($4)
		 AND c.updated_at_set_vers > $5
		 AND `+privateGate+`
		 ORDER BY c.channel_id ASC`,
		m.ShortHostID().ExportToDB(),
		team.ExportToDB(),
		appDB,
		tiers,
		last.Int(),
		m.UID().ExportToDB(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ret []rem.RTChannelMetadata
	for rows.Next() {
		var raw channelMetadataRaw
		err := rows.Scan(raw.scanDests()...)
		if err != nil {
			return nil, err
		}
		md, err := raw.export(team, app)
		if err != nil {
			return nil, err
		}
		err = applyReadRoleGate(md, role)
		if err != nil {
			return nil, err
		}
		applyPrivateGate(md, raw.aclMember)
		ret = append(ret, *md)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return ret, nil
}

// channelMetadataRaw holds one scanned row of channelMetadataCols, prior to
// conversion into an rem.RTChannelMetadata. Shared by the channel-list read
// (readAllChannels) and the inbox sync (GetChangedThreads), whose queries both
// SELECT channelMetadataCols (the inbox query appends per-user columns of its
// own after these).
type channelMetadataRaw struct {
	idRaw, nameBoxRaw, descBoxRaw []byte
	partyIDRaw, uidRaw            []byte
	seqno                         int
	rrt, rvl, wrt, wvl            int
	lastMsgType                   *string
	lastMsgSeq                    *int64
	lastSendTime                  *time.Time
	ctime, mtime                  time.Time
	updatedAtSetVers              int
	tierRaw                       string
	private                       bool
	// aclMember says whether the CALLER holds a channel_acl row for this
	// channel. Always true for a non-private channel (which has no ACL);
	// false only for a team admin looking at a private channel they are not a
	// member of -- see applyPrivateGate.
	aclMember bool
	// noPush: the channel is excluded from push fan-out (fork-only). Read
	// back so a listing's metadata is truthful, not because any client
	// decision hangs on it.
	noPush bool
	// archivedAt is non-NULL for an archived channel. Unlike noPush, a client
	// decision DOES hang on this one: an archived channel stays in the team
	// listing so its encrypted name stays reserved, and the client is what
	// hides it from the channel list while keeping it in its collision map.
	archivedAt *time.Time
}

// channelMetadataCols is the column list matching channelMetadataRaw.scanDests,
// against a query whose FROM clause binds c = channels and cp = the
// channel_parties row of the channel's last sender.
const channelMetadataCols = `c.channel_id_full, c.seqno, c.name_box, c.desc_box,
	        c.read_role_type, c.read_role_viz_level,
	        c.write_role_type, c.write_role_viz_level,
	        c.last_msg_type, c.last_msg_seq, c.last_send_time,
	        cp.party_id, cp.uid,
	        c.ctime, c.mtime, c.updated_at_set_vers, c.tier`

// channelForkCols is the fork-only tail of channelMetadataRaw.scanDests: the
// channel's privacy flag, whether the CALLER holds a channel_acl row for it,
// and its no-push flag. Appended (immediately after channelMetadataCols) by
// both queries that scan channelMetadataRaw. uidParam is the placeholder
// holding the caller's uid in the enclosing query, which differs between the
// two -- hence a function rather than a second const.
//
// ORDER IS THE CONTRACT, and it is positional in both directions: scanDests
// lists its destinations in exactly this sequence, and nothing checks the two
// agree. A new fork column therefore goes at the END of both, never in the
// middle of either -- inserting one here without the matching insertion in
// scanDests silently scans each later column into its neighbour's
// destination, in the team listing and the inbox changed-threads path alike.
//
// acl_member is privateVisibleToCaller verbatim, by construction rather than by
// copy: readAllChannels puts the same expression in its WHERE and here in its
// SELECT, and if the two ever drifted the listing would filter on one rule
// while labelling rows by another -- admitting a channel it marks as
// non-member, or the reverse. One source of truth removes the possibility.
func channelForkCols(uidParam string) string {
	return `, c.private, ` + privateVisibleToCaller("c", uidParam) + ` AS acl_member, c.no_push, c.archived_at`
}

// lastSenderJoin attributes the channel's denormalized last message to its
// sender; LEFT so channels with no messages still row.
const lastSenderJoin = `LEFT JOIN channel_parties cp ON
	     cp.short_host_id = c.short_host_id
	     AND cp.channel_id = c.channel_id
	     AND cp.party_no = c.last_sender_no`

func (r *channelMetadataRaw) scanDests() []any {
	return []any{
		&r.idRaw, &r.seqno, &r.nameBoxRaw, &r.descBoxRaw,
		&r.rrt, &r.rvl, &r.wrt, &r.wvl,
		&r.lastMsgType, &r.lastMsgSeq, &r.lastSendTime,
		&r.partyIDRaw, &r.uidRaw,
		&r.ctime, &r.mtime, &r.updatedAtSetVers,
		&r.tierRaw,
		&r.private, &r.aclMember, &r.noPush, &r.archivedAt,
	}
}

func (r *channelMetadataRaw) export(
	team proto.TeamID,
	app proto.RTAppID,
) (
	*rem.RTChannelMetadata,
	error,
) {
	md := rem.RTChannelMetadata{
		ParentTeam: team,
		AppID:      app,
		Seqno:      proto.RTChannelSeqno(r.seqno),
		Ctime:      proto.ExportTime(r.ctime),
		Mtime:      proto.ExportTime(r.mtime),
		UpdatedAt:  proto.RTChannelSetVersion(r.updatedAtSetVers),
	}
	if len(r.idRaw) != len(md.Id) {
		return nil, core.BadServerDataError("bad channel_id_full length")
	}
	copy(md.Id[:], r.idRaw)

	err := core.DecodeFromBytes(&md.NameBox, r.nameBoxRaw)
	if err != nil {
		return nil, err
	}
	if len(r.descBoxRaw) > 0 {
		var box proto.RTBoxRG
		err = core.DecodeFromBytes(&box, r.descBoxRaw)
		if err != nil {
			return nil, err
		}
		md.DescBox = &box
	}
	err = md.Roles.Read.ImportFromDB(r.rrt, r.rvl)
	if err != nil {
		return nil, err
	}
	err = md.Roles.Write.ImportFromDB(r.wrt, r.wvl)
	if err != nil {
		return nil, err
	}

	lm, err := r.importLastMessage()
	if err != nil {
		return nil, err
	}
	if lm != nil {
		md.LastMsg = lm
	}
	err = md.Tier.ImportFromDB(r.tierRaw)
	if err != nil {
		return nil, err
	}
	md.Archived = r.archivedAt != nil
	md.Private = r.private
	md.NoPush = r.noPush
	return &md, nil
}

func (r *channelMetadataRaw) hasLastMessage() bool {
	return r.lastMsgType != nil &&
		r.lastMsgSeq != nil &&
		r.lastSendTime != nil &&
		len(r.partyIDRaw) > 0 &&
		len(r.uidRaw) > 0
}

func (r *channelMetadataRaw) importLastMessage() (
	*rem.RTLastMsg,
	error,
) {
	var ret rem.RTLastMsg
	if !r.hasLastMessage() {
		return nil, nil
	}
	if r.lastMsgType == nil {
		return nil, core.BadServerDataError("nil last msgType unexpected")
	}
	err := ret.Typ.ImportFromDB(*r.lastMsgType)
	if err != nil {
		return nil, err
	}
	if r.lastMsgSeq == nil {
		return nil, core.BadServerDataError("nil last msgSeq unexpected")
	}
	ret.Seq = proto.RTMsgSeq(*r.lastMsgSeq)
	if r.lastSendTime == nil {
		return nil, core.BadServerDataError("nil last sendtime unexpected")
	}

	t := proto.ExportTime(*r.lastSendTime)
	ret.InsertTime = t
	if len(r.partyIDRaw) == 0 {
		return nil, core.BadServerDataError("nil last sender unexpected")
	}

	var pid proto.PartyID
	err = pid.ImportFromDB(r.partyIDRaw)
	if err != nil {
		return nil, err
	}
	ret.Sender = &pid

	if r.uidRaw != nil {
		var uid proto.UID
		err = uid.ImportFromDB(r.uidRaw)
		if err != nil {
			return nil, err
		}
		ret.FurtherUserAttribution = &uid
	}
	return &ret, nil
}

// applyReadRoleGate withholds the read-role-sealed portions of md from a
// caller whose team role is below the channel's read role. The channel name is
// sealed at the tier floor, so its name (and very existence) is visible to
// everyone the tier gate admits -- this is needed for name-collision detection
// at create time, since names are ciphertext the server can't dedupe itself.
// The description and last-message preview, however, are sealed at the finer
// read role: don't hand them to a caller whose team role is below it. They
// couldn't decrypt the description anyway (it would only panic/err on the
// client), and the last-message metadata would leak channel activity they
// can't read.
func applyReadRoleGate(md *rem.RTChannelMetadata, role core.RoleKey) error {
	readRoleKey, err := core.ImportRole(md.Roles.Read)
	if err != nil {
		return err
	}
	if role.LessThan(*readRoleKey) {
		md.DescBox = nil
		md.LastMsg = nil
		md.Unreadable = true
	}
	return nil
}

// applyPrivateGate marks a private channel as such on the wire, and withholds
// its activity metadata from a caller who is not in its ACL.
//
// The only caller who can reach this with aclMember == false is a team admin
// (Q1: leaders may see that a private channel exists, and then grant
// themselves into it visibly). They get existence, not activity: no
// description, no last-message preview, and `unreadable` set, exactly as the
// read-role gate does for a channel a caller's role cannot read.
//
// DEVIATION, recorded deliberately: §6.2 of the spec also says an admin
// non-member should not receive the channel's name box. That is not
// expressible today -- RTChannelMetadata.nameBox is a required field and the
// client decrypts it unconditionally, so an absent box fails the whole list
// read -- and withholding it would defeat §6.2's own rationale, that an admin
// must be able to FIND a channel in order to moderate it. The name is sealed
// at the tier floor, so it is a name every member at that floor could read if
// they held the ciphertext; handing it to admins is consistent with Q1/Q2
// making leaders transparent peers rather than invisible ones. If the product
// later wants existence-without-name, it needs an Option(nameBox) wire change
// and a client that tolerates it.
func applyPrivateGate(md *rem.RTChannelMetadata, aclMember bool) {
	if aclMember {
		return
	}
	md.DescBox = nil
	md.LastMsg = nil
	md.Unreadable = true
}

type channelMaker struct {
	md      rem.RTChannelMetadata
	vers    proto.RTChannelSetVersion
	userdb  *pgxpool.Conn
	rtdbtx  pgx.Tx
	dstRole *core.RoleKey

	// members whose inbox versions the fanout bumped; the caller wakes their
	// parked long-pollers after the transaction commits.
	wakeUIDs []proto.UID
}

func (c *channelMaker) checkPerms(m shared.MetaContext) error {
	role, err := AuthorizeUserForTeam(m, c.userdb, c.md.ParentTeam)
	if err != nil {
		return err
	}
	c.dstRole = role
	writeRole, err := core.ImportRole(c.md.Roles.Write)
	if err != nil {
		return err
	}
	readRole, err := core.ImportRole(c.md.Roles.Read)
	if err != nil {
		return err
	}
	if c.dstRole.LessThan(*readRole) {
		return core.PermissionError("read role too high for user")
	}
	if c.dstRole.LessThan(*writeRole) {
		return core.PermissionError("write role too high for user")
	}
	if writeRole.LessThan(*readRole) {
		return core.PermissionError("write role is less than read role")
	}
	if c.md.Tier == proto.RTChannelTier_Admin && !c.dstRole.IsAdminOrAbove() {
		return core.PermissionError("user role too low to make an admin tier channel")
	}
	// Inventory row 11: only admins/leaders may create a private channel (Q2b).
	return authorizeChannelCreate(c.dstRole, c.md.Private)
}

func (c *channelMaker) checkArgs(m shared.MetaContext) error {
	if !c.vers.IsValid() {
		return core.BadArgsError("c.vers must be 1 or greater")
	}
	if c.vers != c.md.UpdatedAt {
		return core.BadArgsError("c.vers must match c.md.UpdatedAt")
	}
	return nil
}

func (c *channelMaker) commit(m shared.MetaContext) error {
	switch {
	case !c.vers.IsValid():
		return core.BadArgsError("bad channel version (must be > 0)")
	case c.vers.IsFirst():
		return c.insertNewChannelSetRow(m)
	default:
		return c.updateChannelSet(m)
	}
}

func (c *channelMaker) insertNewChannelSetRow(m shared.MetaContext) error {
	app, err := c.md.AppID.ExportToDB()
	if err != nil {
		return err
	}
	_, err = c.rtdbtx.Exec(
		m.Ctx(),
		`INSERT INTO channel_sets
			(short_host_id, parent_team_id, app_id, vers, mtime)
		VALUES($1, $2, $3, $4, NOW())`,
		m.ShortHostID(),
		c.md.ParentTeam.ExportToDB(),
		app,
		c.vers,
	)
	if shared.IsDuplicateKeyError(err, "channel_sets_pkey") {
		return core.RTRaceError{Which: "channels"}
	}
	if err != nil {
		return err
	}
	return nil
}

func (c *channelMaker) updateChannelSet(m shared.MetaContext) error {

	app, err := c.md.AppID.ExportToDB()
	if err != nil {
		return err
	}
	tag, err := c.rtdbtx.Exec(
		m.Ctx(),
		`UPDATE channel_sets
		SET vers=$1, mtime=NOW()
		WHERE short_host_id=$2
		AND parent_team_id=$3
		AND app_id=$4
		AND vers=$5`,
		c.vers,
		m.ShortHostID(),
		c.md.ParentTeam.ExportToDB(),
		app,
		c.vers-1,
	)
	// Must propagate: returning nil here swallowed the failure and skipped the
	// RowsAffected check below, so a channel-set version bump that never
	// committed was reported to the caller as a successful create.
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return core.RTRaceError{Which: "channels"}
	}
	return nil
}

func (c *channelMaker) insertChannel(m shared.MetaContext) error {

	nameBox, err := core.EncodeToBytes(&c.md.NameBox)
	if err != nil {
		return err
	}

	// desc_box is optional; both the box and its PTK gen are NULL when absent.
	var descBox []byte
	var descGen *int
	if c.md.DescBox != nil {
		descBox, err = core.EncodeToBytes(c.md.DescBox)
		if err != nil {
			return err
		}
		g := int(c.md.DescBox.Rg.Gen)
		descGen = &g
	}

	readType, readViz, err := c.md.Roles.Read.ExportToDB()
	if err != nil {
		return err
	}
	writeType, writeViz, err := c.md.Roles.Write.ExportToDB()
	if err != nil {
		return err
	}

	tier, err := c.md.Tier.ExportToDB()
	if err != nil {
		return err
	}

	app, err := c.md.AppID.ExportToDB()
	if err != nil {
		return err
	}

	_, err = c.rtdbtx.Exec(
		m.Ctx(),
		`INSERT INTO channels
			(short_host_id, channel_id, parent_team_id, app_id, channel_id_full,
			 seqno, name_box, name_box_ptk_gen, tier, desc_box, desc_box_ptk_gen,
			 read_role_type, read_role_viz_level, write_role_type, write_role_viz_level,
			 ctime, mtime, updated_at_set_vers, private, no_push)
		VALUES($1, $2, $3, $4, $5,
		       $6, $7, $8, $9, $10, $11,
		       $12, $13, $14, $15,
		       NOW(), NOW(), $16, $17, $18)`,
		m.ShortHostID(),
		int64(c.md.Id.Short()),
		c.md.ParentTeam.ExportToDB(),
		app,
		c.md.Id.Bytes(),
		int64(c.md.Seqno),
		nameBox,
		int(c.md.NameBox.Rg.Gen),
		tier,
		descBox,
		descGen,
		readType,
		readViz,
		writeType,
		writeViz,
		c.vers.ExportToDB(),
		c.md.Private,
		c.md.NoPush,
	)
	if shared.IsDuplicateKeyError(err, "channels_pkey") {
		return core.RTRaceError{Which: "channels"}
	}
	if err != nil {
		return err
	}
	return nil
}

func (c *channelMaker) fanoutUsers(m shared.MetaContext) error {

	// Stage 1a: fan out only to direct, local user-members of the parent team
	// whose team role is high enough to read the channel. Nested-team
	// (transitive) membership is deferred to stage 1b. We match the
	// device-membership convention used by AuthorizeUserForTeam: local host,
	// source role Owner, most-recent active row.
	//
	// Users added to the team (or promoted past the read role) after channel
	// creation are fanned in lazily by reconcileUserChannels (fanin.go), which
	// runs on inbox sync and poll; see issue #301. The inverse (removal) is
	// handled by re-authorizing at sync time.
	// Inventory row 11, private half: a private channel fans out to its
	// creator alone and to nobody else. Members arrive later, one explicit
	// rtChannelGrant at a time. The creator is seeded as the ACL owner, which
	// is what lets them grant without being a team admin afterwards.
	if c.md.Private {
		err := insertChannelAcl(m, c.rtdbtx, int64(c.md.Id.Short()),
			m.UID(), aclRoleOwner, m.UID())
		if err != nil {
			return err
		}
		err = c.fanoutToUser(m, m.UID())
		if err != nil {
			return err
		}
		c.wakeUIDs = []proto.UID{m.UID()}
		return nil
	}

	readRole, err := core.ImportRole(c.md.Roles.Read)
	if err != nil {
		return err
	}
	ownerType, ownerViz, err := proto.OwnerRole.ExportToDB()
	if err != nil {
		return err
	}

	rows, err := c.userdb.Query(
		m.Ctx(),
		`SELECT member_id, dst_role_type, dst_viz_level
		 FROM team_members
		 WHERE short_host_id=$1
		 AND team_id=$2
		 AND member_host_id=$3
		 AND src_role_type=$4
		 AND src_viz_level=$5
		 AND active=true`,
		m.ShortHostID(),
		c.md.ParentTeam.ExportToDB(),
		shared.ExportHostP(nil),
		ownerType,
		ownerViz,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Collect eligible UIDs before writing, since the membership cursor lives
	// on c.userdb while the fan-out writes go to the c.rtdbtx transaction.
	var uids []proto.UID
	for rows.Next() {
		var memRaw []byte
		var dstType, dstViz int
		err = rows.Scan(&memRaw, &dstType, &dstViz)
		if err != nil {
			return err
		}
		var pid proto.PartyID
		err = pid.ImportFromDB(memRaw)
		if err != nil {
			return err
		}
		// Stage 1a is users-only; nested-team members are skipped here.
		if !pid.IsUser() {
			continue
		}
		memRole, err := core.ImportRoleKeyFromDB(dstType, dstViz)
		if err != nil {
			return err
		}
		// The member can read the channel iff their team role is at or above
		// the channel's read role.
		if memRole.LessThan(*readRole) {
			continue
		}
		uid, err := pid.UID()
		if err != nil {
			return err
		}
		uids = append(uids, uid)
	}
	if err = rows.Err(); err != nil {
		return err
	}

	// Deterministic order: fanoutToUser takes user_inbox row locks, so sorting
	// by UID keeps the lock order consistent with the message-send fanout
	// (messageSender.fanoutInboxVersions, which ORDERs BY uid), avoiding
	// deadlocks between concurrent transactions over overlapping member sets.
	slices.SortFunc(uids, func(a, b proto.UID) int {
		return bytes.Compare(a[:], b[:])
	})
	for _, uid := range uids {
		err = c.fanoutToUser(m, uid)
		if err != nil {
			return err
		}
	}
	c.wakeUIDs = uids
	return nil
}

// fanoutToUser bumps the user's inbox version for this app and writes the
// denormalized user_channels membership row at that version, so the new
// channel surfaces on the user's next inbox sync. Both writes go to the
// realtime transaction, so they roll back atomically with the channel itself.
func (c *channelMaker) fanoutToUser(m shared.MetaContext, uid proto.UID) error {
	app, err := c.md.AppID.ExportToDB()
	if err != nil {
		return err
	}
	inserted, err := fanUserIntoChannel(m, c.rtdbtx, uid, app, c.md.Id.Short())
	if err != nil {
		return err
	}
	if !inserted {
		// The channel is brand new inside this very transaction, so an
		// existing membership row can only be a bug.
		return core.InsertError("failed insert into user_channels, unexpected")
	}
	return nil
}

// fanUserIntoChannel bumps the user's per-app inbox version and writes their
// denormalized user_channels membership row at that fresh version -- one
// version allocation per stamped row, as the UNIQUE user_channels_inbox_idx
// requires. Shared by the channel-creation fanout and the late-join fan-in
// (issue #301). Returns false if the membership row already existed (a benign
// race for the late-join path: a concurrent device fanned the user in first;
// the version bump is then a harmless gap).
func fanUserIntoChannel(
	m shared.MetaContext,
	tx pgx.Tx,
	uid proto.UID,
	appDB string,
	channelID proto.RTChannelIDShort,
) (
	bool,
	error,
) {
	var inboxVers int64
	err := tx.QueryRow(
		m.Ctx(),
		`INSERT INTO user_inbox (short_host_id, uid, app_id, inbox_version, mtime)
		 VALUES ($1, $2, $3, 1, NOW())
		 ON CONFLICT (short_host_id, uid, app_id)
		 DO UPDATE SET inbox_version = user_inbox.inbox_version + 1, mtime = NOW()
		 RETURNING inbox_version`,
		m.ShortHostID(),
		uid.ExportToDB(),
		appDB,
	).Scan(&inboxVers)
	if err != nil {
		return false, err
	}

	tag, err := tx.Exec(
		m.Ctx(),
		`INSERT INTO user_channels
			(short_host_id, channel_id, uid, app_id, inbox_version,
			 last_msg_time, earliest_msg_time, read_through, hidden, muted,
			 ctime, mtime)
		VALUES ($1, $2, $3, $4, $5,
		        NOW(), NULL, 0, false, false,
		        NOW(), NOW())
		ON CONFLICT (short_host_id, channel_id, uid) DO NOTHING`,
		m.ShortHostID(),
		channelID.Int64(),
		uid.ExportToDB(),
		appDB,
		inboxVers,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (c *channelMaker) run(m shared.MetaContext) error {
	err := c.checkPerms(m)
	if err != nil {
		return err
	}
	err = c.checkArgs(m)
	if err != nil {
		return err
	}
	err = c.insertChannel(m)
	if err != nil {
		return err
	}
	err = c.fanoutUsers(m)
	if err != nil {
		return err
	}
	err = c.commit(m)
	if err != nil {
		return err
	}
	return nil
}

func MakeChannel(
	m shared.MetaContext,
	md rem.RTChannelMetadata,
	vers proto.RTChannelSetVersion,
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

	return shared.RetryTx2(m,
		rtdb,
		"realtime.MakeChannel",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			mk := channelMaker{
				md:     md,
				vers:   vers,
				rtdbtx: tx,
				userdb: userdb,
			}
			if err := mk.run(m); err != nil {
				return nil, err
			}
			// Once the fan-in bumps commit, wake the members' parked
			// long-pollers so the channel surfaces immediately.
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, md.AppID, mk.wakeUIDs)
			}, nil
		},
	)
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

// notArchived is the set-based archived predicate. Exactly one path uses it:
// the late-join fan-in's anti-join, which must never create a delivery row for
// an archived channel.
//
// Neither of the two paths that RETURN channel metadata filters on it, and
// both omissions are deliberate. The inbox changed-threads query does not,
// because an archived channel has to be delivered once, carrying the flag, or
// the client has no way to learn it should drop its stored row -- a delta of
// rows cannot express a removal. The team channel LISTING does not, The channel set
// doubles as the team's name registry, and because names are PTK-encrypted only
// a client can compare them -- so a client can only refuse a duplicate name it
// can still see. Dropping archived rows from the listing would free the name and
// make every unarchive a collision the server has no way to detect.
//
// chAlias is the `channels` alias in the caller's query.
func notArchived(chAlias string) string {
	return chAlias + `.archived_at IS NULL`
}

// archivedBlocks is the per-row archived gate: an archived channel is closed to
// new activity, so the callers that represent activity refuse it.
//
// Takes the loaded timestamp rather than an accessKind so it stays independent
// of the fork-only authorization vocabulary in acl.go -- each call site decides
// whether the access it represents counts as activity. Reads are not blocked:
// the channel is hidden, not destroyed.
func archivedBlocks(archivedAt *time.Time) error {
	if archivedAt != nil {
		return core.RTChannelArchivedError{}
	}
	return nil
}

// channelMutator carries the state shared by the two metadata-mutation RPCs.
// Both CAS on channels.seqno, both bump the team's channel-set version so the
// change reaches every member's incremental listing, and both re-stamp the
// members' inbox rows so it reaches their inbox too.
type channelMutator struct {
	chid   int64
	seqno  proto.RTChannelSeqno
	tx     pgx.Tx
	userdb shared.Querier
	ca     *channelAuth
	appDB  string

	// wakeUIDs are the members whose inbox versions were bumped; the caller
	// wakes their parked long-pollers after the transaction commits.
	wakeUIDs []proto.UID
}

// authorize runs the chokepoint at accessMutate and caches what it loaded.
func (c *channelMutator) authorize(m shared.MetaContext) error {
	ca, err := authorizeChannel(m, c.tx, c.userdb, c.chid, accessMutate, true)
	if err != nil {
		return err
	}
	c.ca = ca
	c.appDB, err = ca.appID.ExportToDB()
	return err
}

// casSeqno applies `set` to the channel row, conditional on the seqno the
// caller last saw, and increments it. A lost race is RTRaceError, which librt
// retries after re-reading the channel -- the same path two concurrent creates
// already take.
//
// `set` is a SQL fragment whose placeholders start at $4.
func (c *channelMutator) casSeqno(
	m shared.MetaContext,
	set string,
	args ...any,
) error {
	all := append([]any{m.ShortHostID(), c.chid, int64(c.seqno)}, args...)
	tag, err := c.tx.Exec(
		m.Ctx(),
		`UPDATE channels
		 SET seqno=seqno+1, mtime=NOW(), `+set+`
		 WHERE short_host_id=$1 AND channel_id=$2 AND seqno=$3`,
		all...,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return core.RTRaceError{Which: "channels"}
	}
	return nil
}

// stampMembers re-stamps every member's user_channels row for this channel at
// a fresh inbox version, so the channel is re-delivered on their next inbox
// sync carrying its new metadata.
//
// One inbox-version allocation per stamped row, never a batch: the UNIQUE
// user_channels_inbox_idx forbids two of a user's rows sharing a version, and
// get_changed_threads' cursor pagination depends on that.
//
// This is what makes a rename reach the inbox rather than only the channel
// listing, and what lets a client drop an archived channel's persisted inbox
// row (the delta re-delivers it once with archived=true; SyncInbox never
// deletes local rows on its own). The cost is O(members) writes per mutation,
// which is the same cost channel CREATION already pays in fanoutUsers -- and
// these are rare admin actions, not per-message work. If channel metadata ever
// becomes frequently mutated, this is the first thing to reconsider.
//
// Ordered by uid so the user_inbox row locks are taken in the same order as
// the send fan-out, which keeps concurrent transactions deadlock-free.
func (c *channelMutator) stampMembers(m shared.MetaContext) error {
	rows, err := c.tx.Query(
		m.Ctx(),
		`SELECT uid FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2
		 ORDER BY uid`,
		m.ShortHostID(),
		c.chid,
	)
	if err != nil {
		return err
	}
	var uids []proto.UID
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var uid proto.UID
		if err = uid.ImportFromDB(raw); err != nil {
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

	for _, uid := range uids {
		var vers int64
		err = c.tx.QueryRow(
			m.Ctx(),
			`INSERT INTO user_inbox (short_host_id, uid, app_id, inbox_version, mtime)
			 VALUES ($1, $2, $3, 1, NOW())
			 ON CONFLICT (short_host_id, uid, app_id)
			 DO UPDATE SET inbox_version = user_inbox.inbox_version + 1, mtime = NOW()
			 RETURNING inbox_version`,
			m.ShortHostID(),
			uid.ExportToDB(),
			c.appDB,
		).Scan(&vers)
		if err != nil {
			return err
		}
		_, err = c.tx.Exec(
			m.Ctx(),
			`UPDATE user_channels SET inbox_version=$4, mtime=NOW()
			 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
			m.ShortHostID(),
			c.chid,
			uid.ExportToDB(),
			vers,
		)
		if err != nil {
			return err
		}
	}
	c.wakeUIDs = uids
	return nil
}

// finish does the bookkeeping both mutations share: surface the change in the
// members' inboxes, and in the incremental channel listing.
//
// ORDER IS A LOCK ORDER, not a preference. Every writer in this package takes
// its row locks in the sequence channels -> user_inbox -> push_outbox ->
// channel_sets: channel creation locks user_inbox in fanoutUsers and only then
// channel_sets in commit, and a send locks user_inbox and then push_outbox in
// fanoutInboxVersions. Touching channel_sets first here would invert that
// against a concurrent create in the same team -- the create holding a
// member's user_inbox row and waiting for channel_sets, this transaction
// holding channel_sets and waiting for the same member -- which Postgres
// resolves by killing one of them with a deadlock error the retry loop is not
// built to absorb.
func (c *channelMutator) finish(m shared.MetaContext) error {
	if err := c.stampMembers(m); err != nil {
		return err
	}
	return touchChannelSet(m, c.tx, c.ca.team, c.appDB, c.chid)
}

// dropChannelPushes discards a closing channel's undelivered push rows.
//
// Held rows would otherwise sit forever: only a hold's holder may decide one,
// and nothing will release a hold on a channel nobody can write to again.
// Queued rows would fire after the channel closed, buzzing members about a
// room that has just been shut. Rows already 'sending' are left alone -- the
// relay owns those, and racing it is worse than one late notification.
func dropChannelPushes(m shared.MetaContext, tx pgx.Tx, channelID int64) error {
	_, err := tx.Exec(
		m.Ctx(),
		`DELETE FROM push_outbox
		 WHERE short_host_id=$1 AND channel_id=$2 AND status IN ('held', 'queued')`,
		m.ShortHostID(),
		channelID,
	)
	return err
}

// UpdateChannel renames a channel and/or replaces its description. Team admins
// only (enforced by the chokepoint at accessMutate).
//
// The server cannot check what the name says -- name_box is sealed with the
// parent team's key -- so two things stay the client's responsibility, and
// librt does both: sealing the name at the channel's TIER name role rather
// than the caller's own, and refusing a name that collides with another
// channel of the same tier.
//
// Permitted on an archived channel: renaming one is how its reserved name is
// released for reuse.
func UpdateChannel(m shared.MetaContext, arg rem.RtUpdateChannelArg) error {
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

	nameBox, err := core.EncodeToBytes(&arg.NameBox)
	if err != nil {
		return err
	}
	var descBox []byte
	var descGen *int
	if arg.DescBox != nil {
		descBox, err = core.EncodeToBytes(arg.DescBox)
		if err != nil {
			return err
		}
		g := int(arg.DescBox.Rg.Gen)
		descGen = &g
	}

	return shared.RetryTx2(m,
		rtdb,
		"realtime.UpdateChannel",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			mu := channelMutator{
				chid:   arg.Chid.Short().Int64(),
				seqno:  arg.Seqno,
				tx:     tx,
				userdb: userdb,
			}
			if err := mu.authorize(m); err != nil {
				return nil, err
			}
			// The name is sealed at the tier's name role; the description at
			// the channel's read role. Reject a box sealed at any other role
			// rather than persisting one nobody (or everybody) can open.
			if err := checkNameBoxRole(mu.ca, arg.NameBox); err != nil {
				return nil, err
			}
			if arg.DescBox != nil {
				if err := checkDescBoxRole(mu.ca, *arg.DescBox); err != nil {
					return nil, err
				}
			}
			err := mu.casSeqno(m,
				`name_box=$4, name_box_ptk_gen=$5, desc_box=$6, desc_box_ptk_gen=$7`,
				nameBox, int(arg.NameBox.Rg.Gen), descBox, descGen,
			)
			if err != nil {
				return nil, err
			}
			if err := mu.finish(m); err != nil {
				return nil, err
			}
			app, uids := mu.ca.appID, mu.wakeUIDs
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, app, uids)
			}, nil
		},
	)
}

// SetChannelArchived archives or unarchives a channel. Team admins only.
//
// Archiving closes the channel to new activity and drops it out of the inbox
// and the late-join fan-in, but deletes nothing: messages, parties, ACL and
// delivery rows all stay, and the channel remains in the team's channel
// listing so its encrypted name stays reserved. Unarchiving is the exact
// inverse and needs no separate call.
func SetChannelArchived(m shared.MetaContext, arg rem.RtSetChannelArchivedArg) error {
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

	return shared.RetryTx2(m,
		rtdb,
		"realtime.SetChannelArchived",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			mu := channelMutator{
				chid:   arg.Chid.Short().Int64(),
				seqno:  arg.Seqno,
				tx:     tx,
				userdb: userdb,
			}
			if err := mu.authorize(m); err != nil {
				return nil, err
			}
			if arg.Archived {
				// The default channel is never archived. The server cannot
				// read names, so it identifies it structurally instead: the
				// oldest channel of this (team, app), which is the default one
				// by construction -- it is created first, on the team's first
				// send.
				isDefault, err := isDefaultChannel(m, tx, mu.ca.team, mu.appDB, mu.chid)
				if err != nil {
					return nil, err
				}
				if isDefault {
					return nil, core.RTGenericError("cannot archive a team's default channel")
				}
			}
			var set string
			if arg.Archived {
				set = `archived_at=NOW()`
			} else {
				set = `archived_at=NULL`
			}
			if err := mu.casSeqno(m, set); err != nil {
				return nil, err
			}
			// stampMembers takes the user_inbox locks; dropping the channel's
			// push rows must come after it, because a send takes those two in
			// that order too (see finish's note on lock order).
			if err := mu.stampMembers(m); err != nil {
				return nil, err
			}
			if arg.Archived {
				if err := dropChannelPushes(m, tx, mu.chid); err != nil {
					return nil, err
				}
			}
			if err := touchChannelSet(m, tx, mu.ca.team, mu.appDB, mu.chid); err != nil {
				return nil, err
			}
			app, uids := mu.ca.appID, mu.wakeUIDs
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, app, uids)
			}, nil
		},
	)
}

// isDefaultChannel reports whether chid is this (team, app)'s default channel
// -- the one the blueprint calls #general and never archives.
//
// It is an APPROXIMATION, and deliberately a narrow one. The server cannot
// read channel names (name_box is sealed with the team's key), so it
// identifies the default channel structurally: the oldest PUBLIC, BOTTOM-TIER
// channel of the pair. The default channel is always both -- it is created on
// the team's first send, before any private or admin-tier channel can exist --
// and restricting the query to those two properties is what keeps a private
// channel created early in a team's life from being mistaken for it, which an
// oldest-of-all-channels query would do.
//
// The exact check lives in the client, which can read the name; this is the
// backstop for a caller that skips it. Ties on ctime break by channel_id so
// the answer is deterministic.
func isDefaultChannel(
	m shared.MetaContext,
	tx pgx.Tx,
	team proto.TeamID,
	appDB string,
	chid int64,
) (bool, error) {
	var oldest int64
	err := tx.QueryRow(
		m.Ctx(),
		`SELECT channel_id FROM channels
		 WHERE short_host_id=$1 AND parent_team_id=$2 AND app_id=$3
		 AND NOT private AND tier='bottom'
		 ORDER BY ctime ASC, channel_id ASC
		 LIMIT 1`,
		m.ShortHostID(),
		team.ExportToDB(),
		appDB,
	).Scan(&oldest)
	if err == pgx.ErrNoRows {
		// A team with no public bottom-tier channel at all: whatever the
		// caller is archiving, it is not the default one.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return oldest == chid, nil
}

// checkNameBoxRole verifies the client sealed the channel name at the role the
// channel's TIER dictates -- the minimum realtime role for a bottom-tier
// channel, admin for an admin-tier one -- rather than at whatever role the
// caller happens to hold.
//
// Sealing higher would lock out members who could read the old name; sealing
// lower would hand an admin-tier channel's name to everyone. Either way the
// box is wrong, so reject it rather than persist a name that the wrong set of
// people can open. This mirrors checkEncryptionRole on the send path.
func checkNameBoxRole(ca *channelAuth, box proto.RTBoxRG) error {
	want := proto.MinRTRole
	if ca.tier == proto.RTChannelTier_Admin {
		want = proto.AdminRole
	}
	wantKey, err := core.ImportRole(want)
	if err != nil {
		return err
	}
	gotKey, err := core.ImportRole(box.Rg.Role)
	if err != nil {
		return err
	}
	if !gotKey.Eq(*wantKey) {
		return core.BadArgsError("channel name must be sealed at the channel tier's name role")
	}
	return nil
}

// checkDescBoxRole verifies the description was sealed at the channel's read
// role, exactly as at creation.
func checkDescBoxRole(ca *channelAuth, box proto.RTBoxRG) error {
	readKey, err := core.ImportRole(ca.readRole)
	if err != nil {
		return err
	}
	gotKey, err := core.ImportRole(box.Rg.Role)
	if err != nil {
		return err
	}
	if !gotKey.Eq(*readKey) {
		return core.BadArgsError("channel description must be sealed at the channel's read role")
	}
	return nil
}
