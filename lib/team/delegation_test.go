// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package team

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func TestCheckRosterDelegationFloor(t *testing.T) {
	member := func(v proto.VizLevel) proto.Role { return proto.NewRoleWithMember(v) }
	tests := []struct {
		name   string
		role   proto.Role
		ok     bool
		active bool
	}{
		{"none turns delegation off", proto.NewRoleDefault(proto.RoleType_NONE), true, false},
		{"m/1 is the lowest legal floor", member(1), true, true},
		{"m/100", member(100), true, true},
		{"m/32767 is the highest viz level", member(32767), true, true},
		{"m/0 is what ordinary members hold", member(0), false, false},
		{"negative viz levels are below ordinary members", member(-10), false, false},
		{"admin already manages the roster", proto.NewRoleDefault(proto.RoleType_ADMIN), false, false},
		{"owner already manages the roster", proto.NewRoleDefault(proto.RoleType_OWNER), false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRosterDelegationFloor(tc.role)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			rk, err := RosterDelegationFloorActive(&tc.role)
			require.NoError(t, err)
			if tc.active {
				require.NotNil(t, rk)
			} else {
				require.Nil(t, rk)
			}
		})
	}

	// No floor at all means delegation off.
	rk, err := RosterDelegationFloorActive(nil)
	require.NoError(t, err)
	require.Nil(t, rk)
}

// The delegated-change rule, one row per line of the design's checklist.
// Every row that expects an error is a mutation guard for one condition in
// delegatedChangesAllowedLocked.
func TestDelegatedRosterChanges(t *testing.T) {

	owner := core.OwnerRole
	admin := core.AdminRole
	m0 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 0}
	bot := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: -10}
	m5 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 5}
	m100 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 100}
	m200 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 200}
	none := core.RoleKey{Typ: proto.RoleType_NONE}

	ownr := testFQE(t, 1, 1)
	admn := testFQE(t, 1, 2)
	delg := testFQE(t, 1, 3)
	delg2 := testFQE(t, 1, 4)
	plain := testFQE(t, 1, 5)
	vip := testFQE(t, 1, 6) // member at m/5: admin-granted extra access
	boty := testFQE(t, 1, 7)
	newbie := testFQE(t, 1, 8)
	newbie2 := testFQE(t, 1, 9)

	roster := NewRosterCore()
	roster.Add(ownr, owner, 1, 0, 0)
	roster.Add(admn, admin, 1, 0, 0)
	roster.Add(delg, m100, 1, 0, 0)
	roster.Add(delg2, m100, 1, 0, 0)
	roster.Add(plain, m0, 1, 0, 0)
	roster.Add(vip, m5, 1, 0, 0)
	roster.Add(boty, bot, 1, 0, 0)

	keys := make(KeyGens)
	for _, k := range []core.RoleKey{owner, admin, m100, m5, m0, bot} {
		keys[k] = 1
	}

	add := func(who MemberID, r core.RoleKey) Change {
		return Change{Member: who, Info: MemberInfo{Role: r, Gen: 1}}
	}
	rm := func(who MemberID) Change {
		return Change{Member: who, Info: MemberInfo{Role: none}}
	}
	floor := &m100

	tests := []struct {
		name    string
		doer    MemberID
		floor   *core.RoleKey
		adminMd bool
		changes ChangeSet
		errstr  string
	}{
		{"delegate adds a plain member", delg, floor, false, ChangeSet{add(newbie, m0)}, ""},
		{"delegate adds a bot-level member", delg, floor, false, ChangeSet{add(newbie, bot)}, ""},
		{"delegate adds two plain members", delg, floor, false, ChangeSet{add(newbie, m0), add(newbie2, m0)}, ""},
		{"delegate removes a plain member", delg, floor, false, ChangeSet{rm(plain)}, ""},
		{"delegate adds and removes in one link", delg, floor, false, ChangeSet{add(newbie, m0), rm(plain)}, ""},
		{"member above the floor also passes", testFQEWithSrcRole(t, 1, 3, m100.Export()), floor, false, nil, "doer not in roster"},

		{"no floor: pre-existing refusal", delg, nil, false, ChangeSet{add(newbie, m0)}, "doer doesn't have privileged role"},
		{"plain member is below the floor", plain, floor, false, ChangeSet{add(newbie, m0)}, "below the team's roster delegation floor"},
		{"floor above the delegate shuts them out", delg, &m200, false, ChangeSet{add(newbie, m0)}, "below the team's roster delegation floor"},
		{"delegate adds at m/1", delg, floor, false, ChangeSet{add(newbie, core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: 1})}, "only add members at m/0 or below"},
		{"delegate adds at m/5", delg, floor, false, ChangeSet{add(newbie, m5)}, "only add members at m/0 or below"},
		{"delegate adds another delegate", delg, floor, false, ChangeSet{add(newbie, m100)}, "only add members at m/0 or below"},
		{"delegate adds an admin", delg, floor, false, ChangeSet{add(newbie, admin)}, "only add members at m/0 or below"},
		{"delegate removes an m/5 member", delg, floor, false, ChangeSet{rm(vip)}, "only remove members at m/0 or below"},
		{"delegate removes another delegate", delg, floor, false, ChangeSet{rm(delg2)}, "only remove members at m/0 or below"},
		{"delegate removes an admin", delg, floor, false, ChangeSet{rm(admn)}, "only remove members at m/0 or below"},
		{"delegate promotes a plain member", delg, floor, false, ChangeSet{add(plain, m5)}, "may not change an existing member"},
		{"delegate bumps a member's generation", delg, floor, false, ChangeSet{{Member: plain, Info: MemberInfo{Role: m0, Gen: 2}}}, "may not change an existing member"},
		{"delegate removes themselves", delg, floor, false, ChangeSet{rm(delg)}, "may not touch the doer"},
		{"delegate signs link metadata", delg, floor, true, ChangeSet{add(newbie, m0)}, "may not carry link metadata"},
		{"delegate signs an empty change set", delg, floor, false, ChangeSet{}, "must change the roster"},
		{"one bad change taints the set", delg, floor, false, ChangeSet{add(newbie, m0), rm(vip)}, "only remove members at m/0 or below"},

		{"admins are untouched by the floor", admn, floor, true, ChangeSet{add(newbie, m5)}, ""},
		{"admins unaffected when delegation off", admn, nil, false, ChangeSet{add(newbie, m0)}, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := &GameplanOpts{
				DelegationFloor:      tc.floor,
				LinkHasAdminMetadata: tc.adminMd,
			}
			_, _, _, err := tc.changes.Gameplan(tc.doer, roster, keys, opts)
			if tc.errstr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.errstr)
			}
		})
	}
}
