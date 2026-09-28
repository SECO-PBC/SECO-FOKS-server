// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// The inviter's seed box is sealed to the PUK generation current at create
// time. A device revoke rotates the PUK; listing afterwards must still open
// the box, with the older generation the seed chain hands to the current key.
func TestSocialInviteListAfterPUKRotation(t *testing.T) {
	tew := testEnvBeta(t)
	alice := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, alice)
	tew.DirectDoubleMerklePokeInTest(t)

	mc := tew.NewClientMetaContext(t, alice)
	res, err := libclient.SocialInviteCreate(mc, lcl.SocialInviteCreateArg{
		Team: *tm.ToFQTeamParsed(t),
		Text: "hi",
	})
	require.NoError(t, err)

	beta := alice.ProvisionNewDevice(t, alice.eldest, "beta", proto.DeviceType_Computer, proto.OwnerRole)
	tew.DirectMerklePokeForLeafCheck(t)
	alice.RevokeDevice(t, alice.eldest, beta)
	tew.DirectMerklePokeForLeafCheck(t)

	mc = tew.NewClientMetaContext(t, alice)
	puks, err := mc.G().ActiveUser().RefreshPUKs(mc)
	require.NoError(t, err)
	require.Equal(t, proto.Generation(2), puks.Current().Metadata().Gen)

	rows, err := libclient.SocialInviteList(mc)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, res.Id, rows[0].Id)
	require.Equal(t, res.Seed, rows[0].Seed)
	require.Len(t, rows[0].Msgs, 1)
	require.Equal(t, "hi", rows[0].Msgs[0].Text)
}
