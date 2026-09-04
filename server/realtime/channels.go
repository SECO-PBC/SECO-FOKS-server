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
	if err == pgx.ErrNoRows {
		return retVers, retTime, nil
	}
	if err != nil {
		return retVers, retTime, nil
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
		`SELECT `+channelMetadataCols+channelPrivacyCols("$6")+`
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

// channelPrivacyCols is the fork-only tail of channelMetadataRaw.scanDests:
// the channel's privacy flag, and whether the CALLER holds a channel_acl row
// for it. Appended (in this order, immediately after channelMetadataCols) by
// both queries that scan channelMetadataRaw. uidParam is the placeholder
// holding the caller's uid in the enclosing query, which differs between the
// two -- hence a function rather than a second const.
//
// acl_member is privateVisibleToCaller verbatim, by construction rather than by
// copy: readAllChannels puts the same expression in its WHERE and here in its
// SELECT, and if the two ever drifted the listing would filter on one rule
// while labelling rows by another -- admitting a channel it marks as
// non-member, or the reverse. One source of truth removes the possibility.
func channelPrivacyCols(uidParam string) string {
	return `, c.private, ` + privateVisibleToCaller("c", uidParam) + ` AS acl_member`
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
		&r.private, &r.aclMember,
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
	md.Private = r.private
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
	if err != nil {
		return nil
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
			 ctime, mtime, updated_at_set_vers, private)
		VALUES($1, $2, $3, $4, $5,
		       $6, $7, $8, $9, $10, $11,
		       $12, $13, $14, $15,
		       NOW(), NOW(), $16, $17)`,
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
