// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package team

import (
	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
)

// plainMemberCeiling is the highest role a delegated change may add or
// remove: an ordinary member at m/0. Positive viz levels are how admins
// hand out extra access (and where delegation floors themselves live), so
// they stay admin-managed.
var plainMemberCeiling = core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 0}

// A team's roster delegation floor is a role stored in the team chain as
// link metadata (ChangeType_RosterDelegationFloor). When it is set to a
// member role, members at or above it may sign a restricted class of roster
// changes for the team. When it is NONE, or was never set, roster changes
// require an admin or owner, which is the pre-existing rule.
//
// CheckRosterDelegationFloor says which values may be written to the chain:
// a MEMBER role at viz level 1 or higher (so that the floor is never a role
// ordinary members already hold), or NONE to turn delegation off. Everything
// else -- m/0, negative viz levels, ADMIN, OWNER -- is refused at link-open
// time, by the server when it accepts the link and by every client that
// replays it.
func CheckRosterDelegationFloor(r proto.Role) error {
	rk, err := core.ImportRole(r)
	if err != nil {
		return err
	}
	switch {
	case rk.Typ == proto.RoleType_NONE:
		return nil
	case rk.Typ == proto.RoleType_MEMBER && rk.Lev >= 1:
		return nil
	default:
		return core.LinkError(
			"roster delegation floor must be a member role at viz level >= 1, or none to turn delegation off",
		)
	}
}

// RosterDelegationFloorActive turns a stored floor into the RoleKey at which
// delegation operates. It returns nil when the floor is absent or NONE, i.e.
// when delegation is off.
func RosterDelegationFloorActive(r *proto.Role) (*core.RoleKey, error) {
	if r == nil {
		return nil, nil
	}
	rk, err := core.ImportRole(*r)
	if err != nil {
		return nil, err
	}
	if rk.Typ == proto.RoleType_NONE {
		return nil, nil
	}
	return rk, nil
}

// delegatedChangesAllowedLocked is the one way a doer who is not an admin
// or owner may pass checkChangesLocked: the team has a delegation floor,
// the doer is a member at or above it, and the change set does nothing but
// add or remove plain members (m/0 or below). Everything else -- role
// changes, key-generation bumps, anything touching a member above m/0 or
// the doer themselves, and any link metadata -- stays admin-only.
//
// Locality needs no check of its own here: the doer MemberID is built at
// the team's host (GameplanWithChanges), so a signer from another host
// never matches a roster entry and fails "doer not in roster" first.
func (r *RosterCore) delegatedChangesAllowedLocked(
	opts *GameplanOpts,
	doerInfo MemberInfo,
	doer MemberID,
	changes ChangeSet,
) error {
	if opts == nil || opts.DelegationFloor == nil {
		// Delegation off: keep the pre-existing error for a non-admin doer.
		return core.TeamRosterError("doer doesn't have privileged role")
	}
	dr := doerInfo.Role
	if dr.Typ != proto.RoleType_MEMBER || dr.LessThan(*opts.DelegationFloor) {
		return core.TeamRosterError("doer is below the team's roster delegation floor")
	}
	if opts.LinkHasAdminMetadata {
		return core.TeamRosterError("a delegated change may not carry link metadata")
	}
	if len(changes) == 0 {
		return core.TeamRosterError("a delegated change must change the roster")
	}
	for _, chng := range changes {
		if chng.Member.Eq(doer) {
			return core.TeamRosterError("a delegated change may not touch the doer")
		}
		existing, inRoster := r.members[chng.Member]
		if chng.Info.Role.Typ == proto.RoleType_NONE {
			if !inRoster {
				return core.TeamRosterError("can't remove non-existent member")
			}
			if plainMemberCeiling.LessThan(existing.Role) {
				return core.TeamRosterError("a delegated change may only remove members at m/0 or below")
			}
		} else {
			if inRoster {
				return core.TeamRosterError("a delegated change may not change an existing member")
			}
			if plainMemberCeiling.LessThan(chng.Info.Role) {
				return core.TeamRosterError("a delegated change may only add members at m/0 or below")
			}
		}
	}
	return nil
}
