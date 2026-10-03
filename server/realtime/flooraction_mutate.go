// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package realtime

import (
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/server/shared"
)

// Floor actions for channel metadata (fork; upstream after go-foks#376,
// which brings rename and archive). The built-in rule stays "team admin";
// with the action configured and the team delegating, a member at or above
// the floor may also:
//
//   - rename a channel or edit its description (rt.channel.edit), and
//   - archive or unarchive an OPEN channel (rt.channel.archive_open).
//
// Archiving a private channel stays admin-only. Nothing here touches the
// missing-ACL-row gate, which only admins bypass, so a floor member acts
// only on private channels they are in.
const (
	FloorActionChannelEdit        = "rt.channel.edit"
	FloorActionChannelArchiveOpen = "rt.channel.archive_open"
)

// floorOpensMutation says whether the host config plus the team's delegation
// floor open a metadata mutation (want: accessMutate or accessArchive) on
// this channel to a non-admin caller.
func floorOpensMutation(
	m shared.MetaContext,
	userdb shared.Querier,
	ca *channelAuth,
	want accessKind,
	role core.RoleKey,
) (
	bool,
	error,
) {
	action := FloorActionChannelEdit
	if want == accessArchive {
		if ca.private {
			return false, nil
		}
		action = FloorActionChannelArchiveOpen
	}
	return floorOpens(m, userdb, ca.team, role, action)
}
