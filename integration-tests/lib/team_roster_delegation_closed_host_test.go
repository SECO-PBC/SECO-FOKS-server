// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// Roster delegation on a host with closed user viewership (the test
// environment's default), the setup the SECO app runs on. The other delegate
// tests use an open vhost, and the closed-host add tests add as an admin, so
// neither covers a delegate adding by UID through the team's view token.

// addAs adds member by UID to c's team as actor, at dstRole when set.
func (c *closedHostTeam) addAs(t *testing.T, actor *TestUser, member *TestUser, dstRole *proto.Role) error {
	mc, tm := memberMinder(t, c.tew, actor)
	err := tm.Add(mc, lcl.TeamAddArg{
		Team:    *c.tm.ToFQTeamParsed(t),
		Members: []lcl.FQPartyParsedAndRole{byUID(member)},
		DstRole: dstRole,
	})
	if err == nil {
		c.tew.DirectDoubleMerklePokeInTest(t)
	}
	return err
}

// rosterRole is member's role in the roster viewer sees when loading the
// team fresh, or nil when member is not on it.
func (c *closedHostTeam) rosterRole(t *testing.T, viewer *TestUser, member *TestUser) *proto.Role {
	mc, tm := memberMinder(t, c.tew, viewer)
	roster, err := tm.ListTeamRoster(mc, *c.tm.ToFQTeamParsed(t))
	require.NoError(t, err)
	for _, m := range roster.Members {
		if m.Mem.Fqp.Party.Eq(member.uid.ToPartyID()) {
			return &m.DstRole
		}
	}
	return nil
}

// requireRole asserts viewer sees member on the roster at role.
func (c *closedHostTeam) requireRole(t *testing.T, viewer *TestUser, member *TestUser, role proto.Role) {
	got := c.rosterRole(t, viewer, member)
	require.NotNil(t, got, "member not on the roster")
	eq, err := got.Eq(role)
	require.NoError(t, err)
	require.True(t, eq, "member at %v, want %v", *got, role)
}

// The owner promotes a plain member to the floor and sets the floor in one
// link; the delegate then adds a newcomer by UID with no admin key, the
// newcomer loads the team, and the delegate removes them again.
func TestDelegateAddsByUIDOnClosedHost(t *testing.T) {
	c := newClosedHostTeam(t)
	setRosterDelegation(t, c.tew, true)
	defer setRosterDelegation(t, c.tew, false)

	delg := c.newUser(t, true)
	require.NoError(t, c.addAs(t, c.alice, delg, nil))

	mo, tmo := memberMinder(t, c.tew, c.alice)
	err := tmo.TeamChangeRoles(mo, lcl.TeamChangeRolesArg{
		Team:            *c.tm.ToFQTeamParsed(t),
		Changes:         []lcl.RoleChange{c.tm.uidChange(t, delg, "m/100")},
		DelegationFloor: ptrTo(proto.NewRoleWithMember(100)),
	})
	require.NoError(t, err)
	c.tew.DirectDoubleMerklePokeInTest(t)

	newbie := c.newUser(t, true)
	require.NoError(t, c.addAs(t, delg, newbie, nil))

	mc, tm := memberMinder(t, c.tew, newbie)
	membs, err := tm.ListMemberships(mc, nil)
	require.NoError(t, err)
	require.Len(t, membs.Teams, 1)
	require.Equal(t, proto.DefaultRole, membs.Teams[0].DstRole)
	tr, err := tm.LoadTeamWithFQTeam(
		mc,
		proto.FQTeam{Team: c.tid, Host: c.alice.host},
		libclient.LoadTeamOpts{Refresh: true},
	)
	require.NoError(t, err)
	require.NotNil(t, tr)
	c.requireRole(t, c.alice, newbie, proto.DefaultRole)

	// Closed viewership still holds for a delegate: someone who never
	// granted the team a view permission cannot be added.
	stranger := c.newUser(t, false)
	err = c.addAs(t, delg, stranger, nil)
	require.Error(t, err)
	var cle core.ChainLoaderError
	require.ErrorAs(t, err, &cle)
	require.ErrorIs(t, cle.Err, core.PermissionError("no view permission"))

	md, tmd := memberMinder(t, c.tew, delg)
	err = tmd.TeamChangeRoles(md, lcl.TeamChangeRolesArg{
		Team:    *c.tm.ToFQTeamParsed(t),
		Changes: []lcl.RoleChange{c.tm.uidChange(t, newbie, "n")},
	})
	require.NoError(t, err)
	c.tew.DirectDoubleMerklePokeInTest(t)
	require.Nil(t, c.rosterRole(t, c.alice, newbie))
}

// The owner demotes an admin to the floor in the same link that sets the
// floor (SECO's "Make Steward" on a Leader). The demotion rotates the admin
// keys; the demoted member must still load the team at its latest
// generation on a closed host, see later roster changes, and act as a
// delegate.
func TestAdminDemotedToFloorOnClosedHost(t *testing.T) {
	c := newClosedHostTeam(t)
	setRosterDelegation(t, c.tew, true)
	defer setRosterDelegation(t, c.tew, false)

	leo := c.newUser(t, true)
	require.NoError(t, c.addAs(t, c.alice, leo, &proto.AdminRole))

	mo, tmo := memberMinder(t, c.tew, c.alice)
	err := tmo.TeamChangeRoles(mo, lcl.TeamChangeRolesArg{
		Team:            *c.tm.ToFQTeamParsed(t),
		Changes:         []lcl.RoleChange{c.tm.uidChange(t, leo, "m/100")},
		DelegationFloor: ptrTo(proto.NewRoleWithMember(100)),
	})
	require.NoError(t, err)
	c.tew.DirectDoubleMerklePokeInTest(t)

	// A change made after the demotion is visible to the demoted member.
	carol := c.newUser(t, true)
	require.NoError(t, c.addAs(t, c.alice, carol, nil))
	c.requireRole(t, leo, carol, proto.DefaultRole)
	c.requireRole(t, leo, leo, proto.NewRoleWithMember(100))

	// The demoted member can act as a delegate...
	dave := c.newUser(t, true)
	require.NoError(t, c.addAs(t, leo, dave, nil))
	c.requireRole(t, c.alice, dave, proto.DefaultRole)

	// ...but no longer as an admin: a role change they sign is refused.
	ml, tml := memberMinder(t, c.tew, leo)
	err = tml.TeamChangeRoles(ml, lcl.TeamChangeRolesArg{
		Team:    *c.tm.ToFQTeamParsed(t),
		Changes: []lcl.RoleChange{c.tm.uidChange(t, carol, "m/100")},
	})
	require.Error(t, err)
	c.requireRole(t, c.alice, carol, proto.DefaultRole)
}
