// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

func (c *UserClientConn) ReserveTeamname(
	ctx context.Context,
	nm proto.Name,
) (
	rem.ReserveNameRes,
	error,
) {
	m := shared.NewMetaContextConn(ctx, c)
	return shared.ReserveName(m, nm, rem.NameType_Team, 0)
}

type teamEditor struct {
	*UserClientConn
	vtab      teamEditInterface
	tx        pgx.Tx
	arg       rem.EditTeamArg
	openres   *team.OpenTeamLinkRes
	signer    proto.EntityID
	teamID    proto.TeamID
	name      proto.Name
	seqno     proto.Seqno
	prev      *proto.BaseChainer
	res       rem.EditTeamRes
	tokTeamID *proto.TeamID

	// The link signer's role in the PRE-link roster (nil when not a member,
	// or on team create, where the signer is the founding owner).
	signerPreRole *core.RoleKey
}

type teamCreatorNameArg struct {
	NameUtf8              proto.NameUtf8
	TeamnameCommitmentKey proto.RandomCommitmentKey
	Rnr                   rem.ReserveNameRes
}

type teamCreator struct {
	*teamEditor
	openEldestRes *team.OpenEldestRes
	commonArg     rem.CreateTeamCommonArg
	nameArg       *teamCreatorNameArg
	successHooks  []func(m shared.MetaContext) error
}

type teamEditInterface interface {
	insertCreateTeam(m shared.MetaContext) error
	openLink(m shared.MetaContext) (*team.OpenTeamLinkRes, error)
}

func (e *teamEditor) insertCreateTeam(m shared.MetaContext) error { return nil }

func (c *teamCreator) openLink(m shared.MetaContext) (*team.OpenTeamLinkRes, error) {
	return &c.openEldestRes.OpenTeamLinkRes, nil
}

var _ teamEditInterface = (*teamCreator)(nil)
var _ teamEditInterface = (*teamEditor)(nil)

func (c *UserClientConn) CreateTeam(
	ctx context.Context,
	arg rem.CreateTeamArg,
) error {
	m := shared.NewMetaContextConn(ctx, c)
	db, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer db.Release()

	return shared.RetryTx2(
		m, db, "createTeam",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			obj := &teamCreator{
				nameArg: &teamCreatorNameArg{
					NameUtf8:              arg.NameUtf8,
					TeamnameCommitmentKey: arg.TeamnameCommitmentKey,
					Rnr:                   arg.Rnr,
				},
				commonArg: rem.CreateTeamCommonArg{
					Eta:                      arg.Eta,
					TeamMembershipLink:       arg.TeamMembershipLink,
					SubchainTreeLocationSeed: arg.SubchainTreeLocationSeed,
				},
				teamEditor: &teamEditor{
					UserClientConn: c,
					arg:            arg.Eta,
					tx:             tx,
				},
			}
			obj.teamEditor.vtab = obj
			return obj.run(m)
		})
}

func (c *teamCreator) handleNameReservation(
	m shared.MetaContext,
) (
	proto.Name,
	error,
) {

	if c.nameArg == nil {
		succHook, err := m.G().AdHocTeamNamesManager().InsertPlaceholderTeamName(m, c.tx)
		if err != nil {
			return "", err
		}
		// The success hook can only be called if the transaction commits successfully.
		// If the transaction rolls back, the name reservation is not valid, and we
		// should not mark it as inserted. The hook is nil on the fast path (the
		// placeholder was already known-inserted for this host).
		if succHook != nil {
			c.successHooks = append(c.successHooks, succHook)
		}
		return team.AdHocTeamName, nil
	}

	expectedName, err := core.NormalizeName(proto.NameUtf8(c.nameArg.NameUtf8))
	if err != nil {
		return "", err
	}
	err = shared.ClaimReservation(m, c.tx, m.HostID(), expectedName, c.nameArg.Rnr, rem.NameType_Team)
	if err != nil {
		return "", err
	}
	return expectedName, nil
}

func (c *teamEditor) insertIntoTeams(
	m shared.MetaContext,
) error {
	tag, err := c.tx.Exec(
		m.Ctx(),
		`INSERT INTO teams(short_host_id,team_id,name_ascii,ctime)
		VALUES($1, $2, $3, NOW())`,
		m.ShortHostID().ExportToDB(),
		c.teamID.ExportToDB(),
		string(c.name),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return core.InsertError("teams")
	}
	return err
}

func (c *teamEditor) readRotatedKeys(
	m shared.MetaContext,
) (
	[]proto.EntityID,
	error,
) {
	return shared.ReadRotatedPTKs(m, c.tx, c.teamID, c.openres.Sched)
}

func (c *teamEditor) insertLink(
	m shared.MetaContext,
) error {

	// On a downgrade, we wind up rotating keys. Each of those keys needs to be locked
	// via the revoke_key_locks table, so that we don't have a concurrent attempt to use that
	// key elsewhere. Say, for instance, when signing a link on a different chain like the
	// team membership chain. Thus, we load a list of all keys that are rotated away from
	// here, and pass them to InsertLink, which will lock them.
	rot, err := c.readRotatedKeys(m)
	if err != nil {
		return err
	}

	// For teams, it doesn't matter when the PTK verify key was introduced, since we'll
	// never wind up reusing them. So it's safe to say they are all at seqno=0 in the sigchain.
	// For device keys, we have to be more careful, since yubikeys can be reused, so we need
	// to disambiguate them with the seqno they appeared in the chain with.
	conv := func(e proto.EntityID) core.SignerPair {
		return core.SignerPair{Eid: e}
	}

	rotConv := make([]core.SignerPair, len(rot))
	for i, p := range rot {
		rotConv[i] = conv(p)
	}

	return shared.InsertLink(
		m,
		c.tx,
		proto.ChainType_Team,
		c.teamID.ToPartyID(),
		conv(c.signer),
		c.prev,
		c.openres.Gc.Chainer.Base,
		c.arg.Link,
		shared.TeamChangeInsertTrigger(m, c.teamID, c.seqno, c.openres.Gc.Changes, c.openres.Gc.SharedKeys),
		rotConv,
	)
}

func (c *teamEditor) editMembers(
	m shared.MetaContext,
) error {
	return shared.EditMembers(
		m,
		c.tx,
		c.teamID.EntityID(),
		c.openres.Gc.Chainer.Base.Seqno,
		c.openres.Gc.Chainer.Base.Root.Epno,
		c.openres.Gc.Changes,
		c.arg.Obd.Hepks,
	)
}

func (c *teamCreator) insertCreateTeam(
	m shared.MetaContext,
) error {

	// Ad-hoc teams don't have a name, so we don't need to insert a name.
	// We use the default name "-" for all ad-hoc teams, which should already
	// be in the database.
	var err error
	if c.nameArg != nil {
		err = shared.InsertName(
			m,
			c.tx,
			c.teamID.EntityID(),
			c.signer,
			m.HostID(),
			c.name,
			c.nameArg.NameUtf8,
			&c.nameArg.TeamnameCommitmentKey,
			c.openEldestRes.Tnc,
			c.nameArg.Rnr.Seq,
			rem.NameType_Team,
		)
	} else {
		err = shared.InsertAdHocTeam(m, c.tx, c.teamID, c.openEldestRes.Gc.Changes)
	}
	if err != nil {
		return err
	}
	err = c.insertIntoTeams(m)
	if err != nil {
		return err
	}

	err = shared.InsertSubchainTreeLocationSeed(m, c.tx, c.teamID.ToPartyID(),
		c.commonArg.SubchainTreeLocationSeed, c.openEldestRes.Stltc)
	if err != nil {
		return err
	}

	err = shared.InsertMemberLoadFloor(
		m,
		c.tx,
		c.teamID,
		c.openEldestRes.MemberLoadFloorOrDefault(),
	)
	if err != nil {
		return err
	}
	return nil
}

func (c *teamEditor) findNewKeyInChanges() error {
	nkor := c.arg.Obd.NewKeyOnRotate
	if nkor == nil {
		return nil
	}
	chng := team.FindChangeForMember(c.openres.Gc.Changes, *c.openres.Gc.Signer.KeyOwner)
	if chng == nil {
		return core.BadArgsError("cannot find key owner among changes")
	}
	typ, err := chng.Member.Keys.GetT()
	if err != nil {
		return err
	}
	if typ != proto.MemberKeysType_Team {
		return core.BadArgsError("bad non-team member keys type")
	}
	tmk := chng.Member.Keys.Team()
	if !tmk.VerifyKey.Eq(nkor) {
		return core.BadArgsError("bad new key on rotate")
	}
	return nil
}

func (c *teamEditor) insertLocalViewPermission(
	m shared.MetaContext,
) error {

	// BulkInsertLocalViewPermissions assumes open-viewership mode for
	// any additions. For open viewership, it makes sense to insert a low
	// role here. Maybe should be even lower than m/0. This is temporary
	// BTW, we should revisit this in subqeuent PRs.
	viewerRole := proto.DefaultRole

	err := shared.BulkInsertLocalViewPermissions(
		m,
		c.tx,
		c.teamID.ToPartyID(),
		viewerRole,
		c.arg.InsLocalPermsFor,
		c.openres.Gc.Changes,
	)
	return err
}

func (c *teamEditor) insertPTKs(
	m shared.MetaContext,
) error {

	key := c.signer
	if c.arg.Obd.NewKeyOnRotate != nil {
		err := c.findNewKeyInChanges()
		if err != nil {
			return err
		}
		key = c.arg.Obd.NewKeyOnRotate
	}

	return shared.InsertPTKs(
		m,
		c.tx,
		c.teamID.EntityID(),
		key,
		*c.openres,
		c.arg.Obd,
	)
}

func (c *teamEditor) checkAndInsertRemovalKeys(
	m shared.MetaContext,
) error {
	return shared.CheckAndInsertRemovalKeys(
		m,
		c.tx,
		c.openres.Gc.Chainer.Base.Seqno,
		c.teamID,
		c.openres.Sched,
		c.arg.Obd.RemovalKeys,
		c.openres.Gc.Changes,
	)
}

func (c *teamEditor) checkAndInsertRemovals(
	m shared.MetaContext,
) error {
	return shared.CheckAndInsertRemovals(
		m,
		c.tx,
		c.teamID,
		c.openres.Sched,
		c.arg.Obd.Removals,
	)
}

// insertRosterDelegationFloor stores the floor when this link carries one.
// It runs after insertPTKs so that the key check below sees keys this link
// itself creates. The link-open code already validated the value (member
// role at viz level >= 1, or NONE).
func (c *teamEditor) insertRosterDelegationFloor(
	m shared.MetaContext,
) error {
	flr := c.openres.RosterDelegationFloor
	if flr == nil {
		return nil
	}
	if c.teamID.IsAdHocTeam() {
		return core.TeamError("ad-hoc teams cannot have a roster delegation floor")
	}
	tcfg, err := m.G().Config().TeamConfig(m.Ctx())
	if err != nil {
		return err
	}
	if !tcfg.RosterDelegation() {
		return core.PermissionError("roster delegation is not enabled on this host")
	}
	active, err := team.RosterDelegationFloorActive(flr)
	if err != nil {
		return err
	}
	if active != nil {
		// A floor nobody can ever act at is a footgun, so require the floor
		// role to have a PTK once this link is in: someone holds the role,
		// or this same link grants it.
		ok, err := shared.TeamHasPTKAtRole(m, c.tx, c.teamID, *flr)
		if err != nil {
			return err
		}
		if !ok {
			return core.TeamError("roster delegation floor role has no PTK; grant the role in or before the link that sets the floor")
		}
	}
	return shared.InsertRosterDelegationFloor(m, c.tx, c.teamID, c.seqno, *flr)
}

// processDelegateRemovalKeys runs last in a link's processing, once the
// roster rows, removal-key rows and removals are all in: it applies any
// delegate-box fills the link carried, then checks the floor-team
// invariant -- every active plain member has a delegate box for the
// current floor role. Teams without an active floor skip all of it, and
// may not send fills.
func (c *teamEditor) processDelegateRemovalKeys(
	m shared.MetaContext,
) error {
	flr, err := shared.LoadRosterDelegationFloor(m, c.tx, c.teamID)
	if err != nil {
		return err
	}
	floor, err := team.RosterDelegationFloorActive(flr)
	if err != nil {
		return err
	}
	fills := c.arg.Obd.DelegateRemovalKeyFills
	if floor == nil {
		if len(fills) > 0 {
			return core.TeamError("delegate removal key fills on a team with no delegation floor")
		}
		return nil
	}
	// Only the links that can owe fills (floor changes and demotions) are
	// admin-signed, so a delegated signer never sends any; refusing theirs
	// keeps a member below admin from overwriting valid delegate boxes.
	if len(fills) > 0 && c.signerPreRole != nil && !c.signerPreRole.Typ.IsAdminOrAbove() {
		return core.PermissionError("only an admin may send delegate removal key fills")
	}
	for _, fill := range fills {
		err = shared.StoreDelegateRemovalKeyFill(m, c.tx, c.teamID, fill, *floor)
		if err != nil {
			return err
		}
	}
	return shared.CheckDelegateRemovalKeyCoverage(m, c.tx, c.teamID, *floor)
}

func (c *teamEditor) checkMemberIndexRangesAgainstTeam(
	m shared.MetaContext,
) error {
	return shared.CheckMemberIndexRangesAgainstTeam(
		m,
		c.tx,
		c.teamID,
		c.openres.Gc.Changes,
	)
}

func (c *teamEditor) insertTeamIndexRange(
	m shared.MetaContext,
) error {
	rng := c.openres.Range
	if rng == nil {
		return nil
	}
	return shared.InsertTeamIndexRange(
		m,
		c.tx,
		c.teamID,
		c.openres.Gc.Chainer.Base.Seqno,
		*rng,
	)
}

func (c *teamEditor) insertRemoteMemberViewTokens(
	m shared.MetaContext,
) error {
	return shared.InsertRemoteMemberViewTokens(
		m,
		c.tx,
		c.teamID,
		c.arg.Obd.RemoteMemberViewTokens,
	)
}

func (c *teamEditor) updateLocalJoinReqs(
	m shared.MetaContext,
) error {
	invitees, err := shared.UpdateLocalJoinReqs(
		m,
		c.tx,
		c.teamID,
		c.openres.Sched.Additions,
	)
	if err != nil {
		return err
	}
	c.res.LocalInvitees = invitees
	return nil
}

func (c *teamEditor) checkLocalMembers(
	m shared.MetaContext,
) error {

	err := shared.CheckLocalMembers(m, c.tx, c.openres.Gc.Changes)
	if err != nil {
		return err
	}
	return nil

}

func (c *teamCreator) checkSigner(
	m shared.MetaContext,
) error {
	lsk, err := shared.ReadLatestSharedKey(m, c.tx, m.UID().EntityID(), proto.OwnerRole)
	if err != nil {
		return err
	}
	if !c.openres.Gc.Signer.Key.RollingEq(lsk.VerifyKey) {
		return core.LinkError("bad signing key for signer")
	}
	return nil
}

func (c *teamCreator) checkArgs(m shared.MetaContext) error {

	typ := c.teamID.Type()
	if c.nameArg == nil && typ != proto.EntityType_AdHocTeam {
		return core.BadArgsError("bad team ID type; expected AdHocTeam")
	}
	if c.nameArg != nil && typ != proto.EntityType_NamedTeam {
		return core.BadArgsError("bad team ID type; expected NamedTeam")
	}
	return nil
}

func (c *teamCreator) run(
	m shared.MetaContext,
) (
	func(shared.MetaContext),
	error,
) {
	err := c.runInner(m)
	if err != nil {
		return nil, err
	}
	if len(c.successHooks) == 0 {
		return nil, nil
	}
	hook := func(m shared.MetaContext) {
		for _, h := range c.successHooks {
			if err := h(m); err != nil {
				m.Warnw("teamCreator.success", "err", err)
			}
		}
	}
	return hook, err
}

func (c *teamCreator) runInner(
	m shared.MetaContext,
) error {
	m = m.WithLogTag("TEAM.CREATE")

	var err error

	c.name, err = c.handleNameReservation(m)
	if err != nil {
		return err
	}
	hepks, err := core.ImportHEPKSet(&c.commonArg.Eta.Obd.Hepks)
	if err != nil {
		return err
	}
	openRes, err := team.OpenEldestLink(&c.commonArg.Eta.Link, hepks, m.HostID().Id)
	if err != nil {
		return err
	}

	c.openEldestRes = openRes
	c.teamEditor.openres = &openRes.OpenTeamLinkRes
	c.teamID, err = openRes.Gc.Entity.Entity.ToTeamID()
	if err != nil {
		return err
	}
	c.seqno = openRes.Gc.Chainer.Base.Seqno

	err = c.checkArgs(m)
	if err != nil {
		return err
	}

	// Usually the signer is already in the chain, but for the first link,
	// we need to check the signer against database.
	err = c.checkSigner(m)
	if err != nil {
		return err
	}

	err = c.runEditCommon(m)
	if err != nil {
		return err
	}

	err = shared.InsertTeamMembershipLink(m, c.tx, c.commonArg.TeamMembershipLink)
	if err != nil {
		return err
	}

	viewerPermRole := c.openEldestRes.MemberLoadFloorOrDefault()

	// Owner gives permission to *future* members to load him, once they are allowed
	// into the group. Otherwise, they can't.
	_, err = shared.InsertLocalViewPermission(m, c.tx, c.teamID.ToPartyID(), viewerPermRole, m.UID().ToPartyID())
	if err != nil {
		return err
	}

	err = shared.ShortPartyIns(m, c.tx, c.teamID.ToPartyID())
	if err != nil {
		return err
	}

	return nil
}

func (e *teamEditor) runEdit(m shared.MetaContext) error {

	teamid, seqno, err := team.ExtractTeamAndSeqno(&e.arg.Link)
	if err != nil {
		return err
	}
	e.teamID = *teamid
	e.seqno = seqno

	// Ad-hoc teams have a fixed membership set at creation; reject any later
	// edit (add/remove/role-change/rotation). This is the authoritative guard --
	// the client enforces the same rule, but a client can disable its check, so
	// the server must independently refuse. Checked before any other validation
	// so a malformed edit can't slip past.
	if e.teamID.Type().IsAdHocTeam() {
		return core.TeamError(team.AdHocTeamImmutableMsg)
	}

	return e.runEditCommon(m)
}

func (c *teamEditor) openLink(m shared.MetaContext) (*team.OpenTeamLinkRes, error) {
	roster, prev, err := shared.LoadRoster(m, c.tx, c.teamID)
	if err != nil {
		return nil, err
	}
	if roster == nil {
		return nil, core.InternalError("need a roster or cannot continue")
	}
	c.prev = prev

	hepks, err := core.ImportHEPKSet(&c.arg.Obd.Hepks)
	if err != nil {
		return nil, err
	}

	// The floor in force before this link, from the server's own copy; it
	// is read in the same transaction (and under the same team lock) as the
	// roster, so a racing floor change serializes with this link.
	floor, err := shared.LoadRosterDelegationFloor(m, c.tx, c.teamID)
	if err != nil {
		return nil, err
	}
	res, err := team.OpenTeamLink(&c.arg.Link, hepks, &c.teamID, m.HostID().Id, roster, floor)
	if err == nil && roster != nil && res != nil {
		c.signerPreRole, err = roster.RoleOfSigner(*res.Gc.Signer.KeyOwner, m.HostID().Id)
	}
	if err != nil {
		return nil, err
	}

	// Refuse an edit that takes the team from having at least one owner to
	// having none — nobody would be left who could ever add an owner back
	// (issue #309). This is the authoritative guard: the client enforces the
	// same rule via GameplanOpts.RequireOwner, but a doctored client can skip
	// it. Only the >=1 -> 0 transition is blocked, so teams that are already
	// ownerless can still rekey.
	if roster.HasOwner() && !res.RosterPost.HasOwner() {
		return nil, core.TeamRosterError("edit would leave team without an owner")
	}

	return res, nil
}

func (c *teamEditor) checkAgainstCurrentParty(m shared.MetaContext) error {

	if c.openres.Gc.Signer.KeyOwner.Party.EntityID().Eq(m.UID().EntityID()) {
		return nil
	}
	if c.tokTeamID != nil && c.tokTeamID.Eq(c.teamID) {
		return nil
	}
	return core.PermissionError("poster must be logged in or authorized to act on behalf of the team")
}

func (c *teamEditor) loadBearerToken(m shared.MetaContext) error {
	if c.arg.Tok == nil {
		return nil
	}
	tid, role, err := shared.LoadBearerToken(m, c.tx, *c.arg.Tok, 0)
	if err != nil {
		return err
	}
	ok, err := role.IsAdminOrAbove()
	if err != nil {
		return err
	}
	if !ok {
		return core.PermissionError("bearer token must be admin or above")
	}
	c.tokTeamID = tid
	return nil
}

func (c *teamEditor) checkTeamLimits(m shared.MetaContext) error {
	if c.openres.RosterPost == nil {
		return nil
	}
	nKeys := c.openres.RosterPost.KeyGens.Num()
	cfg, err := m.G().Config().TeamConfig(m.Ctx())
	if err != nil {
		return err
	}
	max := cfg.MaxRoles()
	if nKeys > int(max) {
		return core.TeamRosterError(
			fmt.Sprintf("too many roles (%d); max is %d", nKeys, max),
		)
	}

	return nil
}

func (c *teamEditor) runEditCommon(m shared.MetaContext) error {

	// Locking the chain means we can go ahead and make SELECTs against team
	// data without fear of races. Other threads who lost the race to acquire
	// this lock will be blocked until we commit or rollback. On rollback, they
	// can go forward. On commit, they will error out here with a primary key
	// violation and then rollback.
	err := shared.LockEntity(m, c.tx, c.teamID.EntityID(), proto.ChainType_Team, c.seqno)
	if err != nil {
		return err
	}

	openres, err := c.vtab.openLink(m)
	if err != nil {
		return err
	}

	c.openres = openres
	c.signer = c.openres.Gc.Signer.Key

	err = c.checkTeamLimits(m)
	if err != nil {
		return err
	}

	err = c.loadBearerToken(m)
	if err != nil {
		return err
	}

	err = c.checkAgainstCurrentParty(m)
	if err != nil {
		return err
	}

	// need to insert name before we can insert into teams to satisfy the
	// foreign key constraints.
	err = c.vtab.insertCreateTeam(m)
	if err != nil {
		return err
	}

	// in OpenEldestLink we opened the team roster as a result of the specified
	// changes. Therefore, we know that local users have their hostID=nil set
	// on changes. We here check that these users (or teams) are specified with the
	// corect keys and generations, and aren't behind a rotation due to a race.
	err = c.checkLocalMembers(m)
	if err != nil {
		return err
	}

	err = c.insertLink(m)
	if err != nil {
		return err
	}

	err = c.editMembers(m)
	if err != nil {
		return err
	}

	err = c.insertPTKs(m)
	if err != nil {
		return err
	}

	err = c.insertRosterDelegationFloor(m)
	if err != nil {
		return err
	}

	err = c.insertLocalViewPermission(m)
	if err != nil {
		return err
	}

	err = c.insertTeamIndexRange(m)
	if err != nil {
		return err
	}

	err = c.checkMemberIndexRangesAgainstTeam(m)
	if err != nil {
		return err
	}

	err = c.insertRemoteMemberViewTokens(m)
	if err != nil {
		return err
	}

	err = c.updateLocalJoinReqs(m)
	if err != nil {
		return err
	}

	err = c.checkAndInsertRemovalKeys(m)
	if err != nil {
		return err
	}

	err = c.checkAndInsertRemovals(m)
	if err != nil {
		return err
	}

	err = c.processDelegateRemovalKeys(m)
	if err != nil {
		return err
	}

	// fork-only: delegated removers may only remove members they added.
	err = c.checkDelegatedRemovalsOwnAdds(m)
	if err != nil {
		return err
	}

	err = shared.InsertTreeLocationMachinery(m,
		c.tx,
		proto.ChainType_Team,
		c.teamID.EntityID(),
		c.openres.Gc.Chainer.Base.Seqno,
		c.openres.Gc.LocationVRFID,
		c.arg.NextTreeLocation,
		c.openres.Gc.Chainer.NextLocationCommitment,
	)
	if err != nil {
		return err
	}

	return nil
}

func (c *UserClientConn) EditTeam(
	ctx context.Context,
	arg rem.EditTeamArg,
) (
	rem.EditTeamRes,
	error,
) {
	var zed rem.EditTeamRes
	m := shared.NewMetaContextConn(ctx, c)
	db, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return zed, err
	}
	defer db.Release()

	var ret rem.EditTeamRes
	err = shared.RetryTx(m, db, "edit", func(m shared.MetaContext, tx pgx.Tx) error {
		obj := &teamEditor{
			UserClientConn: c,
			arg:            arg,
			tx:             tx,
		}
		obj.vtab = obj
		err := obj.runEdit(m)
		if err != nil {
			return err
		}
		ret = obj.res
		return nil
	})
	if err != nil {
		return zed, err
	}
	return ret, nil
}

func (c *UserClientConn) MakeInertTeamBearerToken(
	ctx context.Context,
	arg rem.MakeInertTeamBearerTokenArg,
) (
	rem.TeamBearerToken,
	error,
) {
	var tok rem.TeamBearerToken
	m := shared.NewMetaContextConn(ctx, c)
	db, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return tok, err
	}
	defer db.Release()
	err = shared.RetryTx(m, db, "MakeInertTeamBearerToken", func(m shared.MetaContext, tx pgx.Tx) error {
		err := core.RandomFill(tok[:])
		if err != nil {
			return err
		}
		return shared.InsertInertTeamBearerToken(m, tx, tok, arg, nil)
	})
	if err != nil {
		return tok, err
	}
	return tok, nil
}

func (u *UserClientConn) ActivateTeamBearerToken(
	ctx context.Context,
	arg rem.ActivateTeamBearerTokenArg,
) error {
	m := shared.NewMetaContextConn(ctx, u)
	db, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer db.Release()
	return shared.RetryTx(m, db, "ActivateTeamBearerToken", func(m shared.MetaContext, tx pgx.Tx) error {
		return shared.ActivateTeamBearerToken(m, tx, arg, false)
	})
}

func inTeamRoleContext(
	ctx context.Context,
	u *UserClientConn,
	tok rem.TeamBearerToken,
	f func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error,
) error {
	m := shared.NewMetaContextConn(ctx, u)
	db, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer db.Release()
	tt := u.srv.TestTimeTravel()
	tid, role, err := shared.LoadBearerToken(m, db, tok, tt)
	if err != nil {
		return err
	}
	return f(m, *tid, *role)
}

func (u *UserClientConn) CheckTeamBearerToken(
	ctx context.Context,
	arg rem.TeamBearerToken,
) (
	proto.TeamID,
	error,
) {
	var ret proto.TeamID
	err := inTeamRoleContext(ctx, u, arg,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			ret = tid
			return nil
		},
	)
	return ret, err
}

func (u *UserClientConn) PutTeamCert(
	ctx context.Context,
	arg rem.PutTeamCertArg,
) error {
	err := inTeamRoleContext(ctx, u, arg.Tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can put a cert"))
			if err != nil {
				return err
			}
			return shared.RetryTxUserDB(m, "StoreTeamCert",
				func(m shared.MetaContext, tx pgx.Tx) error {
					return shared.StoreTeamCert(m, tx, tid, arg.Cert)
				},
			)
		},
	)
	return err
}

func (u *UserClientConn) LoadTeamRemoteJoinReq(
	ctx context.Context,
	arg rem.LoadTeamRemoteJoinReqArg,
) (
	rem.TeamRemoteJoinReq,
	error,
) {
	var ret rem.TeamRemoteJoinReq
	err := inTeamRoleContext(ctx, u, arg.Tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can get the cert"))
			if err != nil {
				return err
			}
			tmp, err := shared.LoadRemoteJoinReq(m, tid, arg.Jrt)
			if err != nil {
				return err
			}
			ret = *tmp
			return nil
		},
	)
	return ret, err
}

func (u *UserClientConn) PostTeamMembershipLink(
	ctx context.Context,
	arg rem.PostTeamMembershipLinkArg,
) error {
	return inTeamRoleContext(ctx, u, arg.Tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can post to membership chain"))
			if err != nil {
				return err
			}
			return shared.RetryTxUserDB(m, "PostTeamMembershipLink",
				func(m shared.MetaContext, tx pgx.Tx) error {

					return shared.PostGenericLinkTryTx(
						m, tx, arg.Link, tid.ToPartyID(),
						&u.srv.TestStopPostMembershipLink,
					)
				},
			)
		},
	)
}

func (u *UserClientConn) LoadRemovalKeyBoxForTeamAdmin(
	ctx context.Context,
	arg rem.LoadRemovalKeyBoxForTeamAdminArg,
) (
	proto.TeamRemovalKeyBox,
	error,
) {
	var ret proto.TeamRemovalKeyBox
	err := inTeamRoleContext(ctx, u, arg.Tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can get the removal key box"))
			if err != nil {
				return err
			}
			return shared.RetryTxUserDB(m, "PostTeamMembershipLink",
				func(m shared.MetaContext, tx pgx.Tx) error {
					tmp, err := shared.LoadRemovalKeyBoxForTeamAdmin(m, tx, tid, arg.Member, arg.SrcRole)
					if err != nil {
						return err
					}
					ret = *tmp
					return nil
				},
			)
		},
	)
	return ret, err
}

func (u *UserClientConn) LoadRemovalKeysForDelegation(
	ctx context.Context,
	tok rem.TeamBearerToken,
) (
	[]rem.TeamDelegateRemovalKeyFill,
	error,
) {
	var ret []rem.TeamDelegateRemovalKeyFill
	err := inTeamRoleContext(ctx, u, tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can bulk-load removal keys"))
			if err != nil {
				return err
			}
			db, err := m.Db(shared.DbTypeUsers)
			if err != nil {
				return err
			}
			defer db.Release()
			ret, err = shared.LoadRemovalKeysForDelegation(m, db, tid)
			return err
		},
	)
	return ret, err
}

func (u *UserClientConn) PostTeamRemoval(
	ctx context.Context,
	arg rem.PostTeamRemovalArg,
) error {
	return inTeamRoleContext(ctx, u, arg.Tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can post removals"))
			if err != nil {
				return err
			}
			return shared.RetryTxUserDB(m, "PostTeamRemoval",
				func(m shared.MetaContext, tx pgx.Tx) error {
					return shared.PostTeamRemoval(m, tx, tid, arg.Rm)
				},
			)
		},
	)
}

func (u *UserClientConn) GetCurrentTeamCerts(
	ctx context.Context,
	arg rem.TeamBearerToken,
) (
	[]rem.TeamCert,
	error,
) {
	var ret []rem.TeamCert
	err := inTeamRoleContext(ctx, u, arg,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can put a cert"))
			if err != nil {
				return err
			}
			tmp, err := shared.GetCurrentTeamCerts(m, tid)
			if err != nil {
				return err
			}
			ret = tmp
			return nil
		},
	)
	return ret, err
}

func (u *UserClientConn) LoadTeamRawInbox(
	ctx context.Context,
	arg rem.LoadTeamRawInboxArg,
) (
	rem.TeamRawInbox,
	error,
) {
	var ret rem.TeamRawInbox
	err := inTeamRoleContext(ctx, u, arg.Tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can load the inbox"))
			if err != nil {
				return err
			}
			tmp, err := shared.LoadTeamInbox(m, tid, arg.Pagination)
			if err != nil {
				return err
			}
			ret = *tmp
			return nil
		},
	)
	return ret, err
}

func (u *UserClientConn) RejectJoinReq(
	ctx context.Context,
	arg rem.RejectJoinReqArg,
) error {
	return inTeamRoleContext(ctx, u, arg.Tok,
		func(m shared.MetaContext, tid proto.TeamID, r proto.Role) error {
			err := r.AssertAdminOrAbove(core.PermissionError("only admins can reject join requests"))
			if err != nil {
				return err
			}
			return shared.RetryTxUserDB(m, "RejectJoinReq",
				func(m shared.MetaContext, tx pgx.Tx) error {
					return shared.RejectJoinReq(m, tx, tid, arg.Req)
				},
			)
		},
	)
}

func (u *UserClientConn) GetTeamConfig(ctx context.Context) (rem.TeamConfig, error) {
	m := shared.NewMetaContextConn(ctx, u)
	var ret rem.TeamConfig
	tcfg, err := m.G().Config().TeamConfig(ctx)
	if err != nil {
		return ret, err
	}
	ret.MaxRoles = uint64(tcfg.MaxRoles())
	ret.FloorActions = tcfg.FloorActions()
	// A labelled role is only assignable while the host allows delegation,
	// so the labels go out only then: a client can treat their presence as
	// "this host has delegated roles" without knowing about the switch.
	if !tcfg.RosterDelegation() {
		return ret, nil
	}
	labels, err := tcfg.RoleLabels()
	if err != nil {
		return ret, err
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ret.RoleLabels = append(ret.RoleLabels, rem.RoleLabel{Name: name, Role: labels[name]})
	}
	return ret, nil
}

func (c *UserClientConn) CreateTeamAdHoc(
	ctx context.Context,
	arg rem.CreateTeamCommonArg,
) error {
	m := shared.NewMetaContextConn(ctx, c)

	// Ad-hoc teams expose their founding membership in the clear, so they are only
	// permitted on open-viewership hosts.
	cfg, err := m.G().HostIDMap().Config(m, m.ShortHostID())
	if err != nil {
		return err
	}
	if cfg.Viewership.User != proto.ViewershipMode_Open {
		return core.TeamAdhocOpenViewershipError{}
	}

	db, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer db.Release()

	return shared.RetryTx2(
		m, db, "createTeamAdhoc",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			obj := &teamCreator{
				commonArg: arg,
				teamEditor: &teamEditor{
					UserClientConn: c,
					arg:            arg.Eta,
					tx:             tx,
				},
			}
			obj.teamEditor.vtab = obj
			return obj.run(m)
		})
}

var _ rem.TeamAdminInterface = (*UserClientConn)(nil)
