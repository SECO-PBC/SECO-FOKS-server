// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"context"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
)

func (u *UserClientConn) AcceptInviteLocal(
	ctx context.Context,
	arg rem.AcceptInviteLocalArg,
) (
	proto.TeamRSVPLocal,
	error,
) {
	m := shared.NewMetaContextConn(ctx, u)
	return shared.AcceptInviteLocal(m, arg)
}

func (u *UserClientConn) GrantRemoteViewPermissionForTeam(
	ctx context.Context,
	arg rem.GrantRemoteViewPermissionForTeamArg,
) (
	proto.PermissionToken,
	error,
) {
	return shared.GrantRemoteViewPermission(ctx, u, arg.P, &arg.Sig, proto.PartyType_Team)
}

func (u *UserClientConn) GrantLocalViewPermissionForTeam(
	ctx context.Context,
	arg rem.GrantLocalViewPermissionForTeamArg,
) (
	proto.PermissionToken,
	error,
) {
	return shared.GrantLocalViewPermission(ctx, u, arg.P, &arg.Sig, proto.PartyType_Team)
}

var _ rem.TeamMemberInterface = (*UserClientConn)(nil)

// LoadDelegatedRemovalKeyBox hands a member at or above the team's roster
// delegation floor the delegate box of one plain member's removal key. The
// caller is the logged-in user; every check is against the current roster.
func (u *UserClientConn) LoadDelegatedRemovalKeyBox(
	ctx context.Context,
	arg rem.LoadDelegatedRemovalKeyBoxArg,
) (
	proto.TeamRemovalKeyBox,
	error,
) {
	var ret proto.TeamRemovalKeyBox
	m := shared.NewMetaContextConn(ctx, u)
	db, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return ret, err
	}
	defer db.Release()
	box, err := shared.LoadDelegatedRemovalKeyBox(m, db, arg.Team, m.UID(), arg.Member, arg.SrcRole)
	if err != nil {
		return ret, err
	}
	return *box, nil
}
