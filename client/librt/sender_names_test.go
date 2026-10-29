// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"context"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func threadMsgFrom(uid proto.UID) ThreadMessage {
	pid := uid.ToPartyID()
	return ThreadMessage{Sender: &pid}
}

// TestResolveSenderNamesOffline runs ResolveSenderNames with no server to
// load users from: the active user is named from their own UserInfo, a sender
// in the username cache from the cache, and a sender who can't be loaded is
// left out -- no "<error: ...>" placeholder for a view to show as a name.
func TestResolveSenderNamesOffline(t *testing.T) {
	ctx := context.Background()
	g := libclient.NewGlobalContext()
	g.Cfg().TestSetHomeCLIFlag(t.TempDir())
	g.Cfg().TestSetLogTargets("stdout", "stderr")
	g.Cfg().TestSetTestingMode()
	require.NoError(t, g.Configure(ctx))
	m := NewMetaContext(libclient.NewMetaContextTODO(g))

	host := core.RandomHostID()
	me := core.RandomUID()
	bob := core.RandomUID()
	carol := core.RandomUID()

	d := NewMinder(&libclient.UserContext{Info: proto.UserInfo{
		Fqu:      proto.FQUser{Uid: me, HostID: host},
		Username: proto.NameBundle{Name: "me", NameUtf8: "Me"},
	}})
	require.NoError(t, g.UsernameLoader().Set(m.Base(),
		proto.FQUser{Uid: bob, HostID: host}, "bob"))
	rtp := &RTParty{plcn: &libclient.PLCNode{}}

	msgs := []ThreadMessage{threadMsgFrom(me), threadMsgFrom(bob), threadMsgFrom(carol)}
	names, err := d.ResolveSenderNames(m, lcl.ConfigTeam{}, rtp, msgs)
	require.NoError(t, err)
	require.Equal(t, map[proto.UID]proto.NameUtf8{me: "Me", bob: "bob"}, names)

	require.Nil(t, threadMessageToView(&msgs[2], names).SenderName)
}
