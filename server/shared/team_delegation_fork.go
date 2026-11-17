// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"errors"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/jackc/pgx/v5"
)

// LoadAdderOfCurrentMembership (fork-only) returns the party that signed
// the chain link which added this member's CURRENT membership: their
// newest team_removal_keys row names the link seqno (loadCurrentMembership),
// and the stored link names its signer. nil when no row exists.
//
// This is what enforces the fork's community rule that a delegated remover
// may only remove members they added themselves (docs/
// team-roster-delegation.md, "Own invitees"): the add link's signer is
// exact, needs no extra bookkeeping, and covers members added before this
// rule existed.
func LoadAdderOfCurrentMembership(
	m MetaContext,
	tx pgx.Tx,
	teamID proto.TeamID,
	mid team.MemberID,
) (
	*proto.PartyID,
	error,
) {
	cur, err := loadCurrentMembership(m, tx, teamID, mid)
	if err != nil || cur == nil {
		return nil, err
	}
	var body []byte
	err = tx.QueryRow(m.Ctx(),
		`SELECT body FROM links
		 WHERE short_host_id=$1 AND chain_type=$2
		 AND entity_id=$3 AND seqno=$4`,
		m.ShortHostID().ExportToDB(),
		int(proto.ChainType_Team),
		teamID.ToPartyID().ExportToDB(),
		cur.createSeqno,
	).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var link proto.LinkOuter
	err = core.DecodeFromBytes(&link, body)
	if err != nil {
		return nil, err
	}
	gc, _, err := core.OpenGroupChange(&link)
	if err != nil {
		return nil, err
	}
	if gc.Signer.KeyOwner == nil {
		return nil, nil
	}
	ret := gc.Signer.KeyOwner.Party
	return &ret, nil
}

// currentMembership is a member's newest team_removal_keys row: the team
// chain seqno that added their current membership, and when the server
// stored that add.
type currentMembership struct {
	createSeqno int
	ctime       time.Time
}

// loadCurrentMembership reads the newest team_removal_keys row for this
// member. Newest by create_seqno, not "no removal recorded": it runs inside
// the transaction that is removing the member, after the removal MAC has
// already been stamped onto that very row. nil when no row exists.
func loadCurrentMembership(
	m MetaContext,
	tx pgx.Tx,
	teamID proto.TeamID,
	mid team.MemberID,
) (
	*currentMembership,
	error,
) {
	fqp, err := mid.Fqe.Unfix().FQParty()
	if err != nil {
		return nil, err
	}
	var ret currentMembership
	err = tx.QueryRow(m.Ctx(),
		`SELECT create_seqno, ctime FROM team_removal_keys
		 WHERE short_host_id=$1 AND team_id=$2
		 AND member_id=$3 AND member_host_id=$4
		 AND src_role_type=$5 AND src_viz_level=$6
		 ORDER BY create_seqno DESC LIMIT 1`,
		m.ShortHostID().ExportToDB(),
		teamID.ExportToDB(),
		fqp.Party.ExportToDB(),
		ExportHostInScope(m, fqp.Host),
		int(mid.SrcRole.Typ),
		int(mid.SrcRole.Lev),
	).Scan(&ret.createSeqno, &ret.ctime)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

// MemberLeftSinceCurrentAdd (fork-only) reports whether a user on this host
// has left the team themselves since their current membership was added.
// FOKS has no self-removal: a leaver's client (TeamMinder.TeamLeaveSelf)
// only posts a Removed TeamMembershipLink on the leaver's own membership
// chain, and they stay on the roster until someone removes them. This lets
// a delegated remover clear such a leaver even when a Leader added them
// (docs/team-roster-delegation.md, "Leavers").
//
// The leaver's links for this team (user src role) are read newest first:
//
//   - Requested links are skipped. Accepting a re-invite posts one, and
//     the Steward's Accept of that re-invite is the removal this exists for.
//   - The newest other link must be Removed, and the link for this team
//     just before it must be Approved or ApprovedAdHoc. That is the shape
//     TeamLeaveSelf writes; a Removed after a Requested is a cancelled
//     request (TeamCancelRequest), not a leave.
//   - The Removed link must have been posted after the current membership
//     was added, so a leave from an earlier membership does not carry over
//     to a re-add. Both times are server clock: the removal key row's ctime
//     and the link's merkle_work_queue ctime (links.ctime is the time the
//     client signed into the link, so it is not used).
//
// false for team members and for users on other hosts: their membership
// chains do not live in this host's database.
func MemberLeftSinceCurrentAdd(
	m MetaContext,
	tx pgx.Tx,
	teamID proto.TeamID,
	mid team.MemberID,
) (
	bool,
	error,
) {
	fqp, err := mid.Fqe.Unfix().FQParty()
	if err != nil {
		return false, err
	}
	if !fqp.Party.IsUser() || !m.HostID().Id.Eq(fqp.Host) {
		return false, nil
	}
	userSrc, err := core.ImportRole(team.UserSrcRole)
	if err != nil {
		return false, err
	}
	if !mid.SrcRole.Eq(*userSrc) {
		return false, nil
	}
	cur, err := loadCurrentMembership(m, tx, teamID, mid)
	if err != nil || cur == nil {
		return false, err
	}

	rows, err := tx.Query(m.Ctx(),
		`SELECT l.body, q.ctime > $4 FROM links AS l
		 JOIN merkle_work_queue AS q
		   ON q.short_host_id=l.short_host_id AND q.id=l.entity_id
		   AND q.chain_type=l.chain_type AND q.seqno=l.seqno
		 WHERE l.short_host_id=$1 AND l.chain_type=$2 AND l.entity_id=$3
		 ORDER BY l.seqno DESC`,
		m.ShortHostID().ExportToDB(),
		int(proto.ChainType_TeamMembership),
		fqp.Party.ExportToDB(),
		cur.ctime,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	fqt := proto.FQTeam{Host: m.HostID().Id, Team: teamID}
	var removedAfterAdd *bool
	for rows.Next() {
		var body []byte
		var afterAdd bool
		err = rows.Scan(&body, &afterAdd)
		if err != nil {
			return false, err
		}
		state, ok, err := teamMembershipStateFor(body, fqt)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		if removedAfterAdd != nil {
			// The link just before the Removed one: a leave follows an
			// active membership.
			left := state == proto.TeamMembershipLinkState_Approved ||
				state == proto.TeamMembershipLinkState_ApprovedAdHoc
			return left && *removedAfterAdd, nil
		}
		switch state {
		case proto.TeamMembershipLinkState_Requested:
			continue
		case proto.TeamMembershipLinkState_Removed:
			removedAfterAdd = &afterAdd
		default:
			return false, nil
		}
	}
	return false, rows.Err()
}

// teamMembershipStateFor decodes one membership-chain link and returns its
// state when it is about this team at the user src role.
func teamMembershipStateFor(
	body []byte,
	fqt proto.FQTeam,
) (
	proto.TeamMembershipLinkState,
	bool,
	error,
) {
	var link proto.LinkOuter
	err := core.DecodeFromBytes(&link, body)
	if err != nil {
		return 0, false, err
	}
	ovgl, err := core.OpenAndVerifyGenericLink(link)
	if err != nil {
		return 0, false, err
	}
	typ, err := ovgl.Link.Payload.GetT()
	if err != nil {
		return 0, false, err
	}
	if typ != proto.ChainType_TeamMembership {
		return 0, false, nil
	}
	tml := ovgl.Link.Payload.Teammembership()
	if !tml.Team.Eq(fqt) {
		return 0, false, nil
	}
	same, err := tml.SrcRole.Eq(team.UserSrcRole)
	if err != nil || !same {
		return 0, false, err
	}
	state, err := tml.State.GetT()
	if err != nil {
		return 0, false, err
	}
	return state, true, nil
}
