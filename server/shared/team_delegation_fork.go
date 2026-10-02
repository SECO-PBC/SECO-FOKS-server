// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"errors"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/jackc/pgx/v5"
)

// LoadAdderOfCurrentMembership (fork-only) returns the party that signed
// the chain link which added this member's CURRENT membership: their
// newest team_removal_keys row names the link seqno, and the stored link
// names its signer. Newest by create_seqno, not "no removal recorded" --
// this runs inside the transaction that is removing the member, after the
// removal MAC has already been stamped onto that very row. nil when no
// row exists.
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
	fqp, err := mid.Fqe.Unfix().FQParty()
	if err != nil {
		return nil, err
	}
	var createSeqno int
	err = tx.QueryRow(m.Ctx(),
		`SELECT create_seqno FROM team_removal_keys
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
	).Scan(&createSeqno)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
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
		createSeqno,
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
