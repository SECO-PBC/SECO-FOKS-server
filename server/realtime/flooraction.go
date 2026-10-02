// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package realtime

import (
	"slices"

	"github.com/foks-proj/go-foks/lib/core"
)

// Floor actions: operations that, in a team with a roster delegation floor,
// the host config can require the floor role for. Each action is registered
// where the feature lives, with the built-in rule staying in force for
// teams without a floor (and everywhere when the action is not configured).
const (
	FloorActionChannelCreateOpen = "rt.channel.create_open"
)

// floorActionGated says whether a caller is stopped by a floor action:
// only when the host configured the action, the team has a floor, and the
// caller is a non-admin below it. Everyone else falls through to the
// action's built-in rule.
func floorActionGated(
	floorActions []string,
	action string,
	floor *core.RoleKey,
	role core.RoleKey,
) bool {
	if floor == nil || role.IsAdminOrAbove() {
		return false
	}
	if !slices.Contains(floorActions, action) {
		return false
	}
	return role.LessThan(*floor)
}
