// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package realtime

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func TestFloorActionGated(t *testing.T) {
	m0 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 0}
	m100 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 100}
	m200 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 200}
	admin := core.AdminRole
	owner := core.OwnerRole
	cfg := []string{FloorActionChannelCreateOpen}

	tests := []struct {
		name    string
		actions []string
		floor   *core.RoleKey
		role    core.RoleKey
		gated   bool
	}{
		{"no floor: built-in rule", cfg, nil, m0, false},
		{"not configured: built-in rule", nil, &m100, m0, false},
		{"other action configured", []string{"rt.other"}, &m100, m0, false},
		{"plain member below the floor", cfg, &m100, m0, true},
		{"member at the floor", cfg, &m100, m100, false},
		{"member above the floor", cfg, &m100, m200, false},
		{"admins always pass", cfg, &m100, admin, false},
		{"owners always pass", cfg, &m100, owner, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.gated,
				floorActionGated(tc.actions, FloorActionChannelCreateOpen, tc.floor, tc.role))
		})
	}
}
