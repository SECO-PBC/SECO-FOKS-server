// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"testing"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// The key must come from the values, not their addresses: a member role
// holds its viz level behind a pointer, and formatting it with %v printed the
// pointer, so every process built a different key and never found its own
// saved token.
func TestSavedViewTokenKeyIsByValue(t *testing.T) {
	var tm proto.TeamID
	member := proto.FQParty{}
	a := proto.NewRoleWithMember(proto.VizLevel(5))
	b := proto.NewRoleWithMember(proto.VizLevel(5))
	c := proto.NewRoleWithMember(proto.VizLevel(6))

	ka, err := savedViewTokenKey(tm, 3, member, a)
	require.NoError(t, err)
	kb, err := savedViewTokenKey(tm, 3, member, b)
	require.NoError(t, err)
	kc, err := savedViewTokenKey(tm, 3, member, c)
	require.NoError(t, err)
	kg, err := savedViewTokenKey(tm, 4, member, a)
	require.NoError(t, err)

	require.Equal(t, ka, kb, "equal roles, equal keys")
	require.NotEqual(t, ka, kc, "another viz level")
	require.NotEqual(t, ka, kg, "another key generation")
}
