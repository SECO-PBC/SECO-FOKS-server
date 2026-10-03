// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package realtime

import (
	"slices"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
)

// Fork-only floor action: removing another member from a private channel
// (RevokeChannelMember). The built-in rule stays "channel owner or team
// admin"; with this action configured and the team delegating, a member at
// or above the floor may revoke too -- but only in channels they are in,
// since nothing here touches the missing-ACL-row gate, which only admins
// bypass. Granting someone in stays owner-or-admin: adds follow the
// community's add policy; removal is the delegated-moderation action.
const FloorActionChannelRevokePrivate = "rt.channel.revoke_private"

// floorOpens says whether the host configures action and the team's active
// delegation floor is at or below role -- i.e. whether the floor opens an
// otherwise admin-only action to this non-admin caller.
func floorOpens(
	m shared.MetaContext,
	userdb shared.Querier,
	teamID proto.TeamID,
	role core.RoleKey,
	action string,
) (
	bool,
	error,
) {
	tcfg, err := m.G().Config().TeamConfig(m.Ctx())
	if err != nil {
		return false, err
	}
	if !slices.Contains(tcfg.FloorActions(), action) {
		return false, nil
	}
	flr, err := shared.LoadRosterDelegationFloor(m, userdb, teamID)
	if err != nil {
		return false, err
	}
	floor, err := team.RosterDelegationFloorActive(flr)
	if err != nil {
		return false, err
	}
	if floor == nil {
		return false, nil
	}
	return !role.LessThan(*floor), nil
}
