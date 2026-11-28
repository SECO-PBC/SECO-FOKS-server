// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"encoding/hex"
	"sync"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// pushOpenRig is a community with one channel, the team files the SECO app
// reads when a push tap opens it, and a home where an earlier run already
// opened it.
type pushOpenRig struct {
	*coldThreadViewRig
	kvCfg lcl.KVConfig
}

// The team files the app reads on the way to the tapped chat (SECO-FOKS
// foks-gomobile admin_meta.go, service_team.go, channelStore): the
// admin-meta sentinel, the community's display name and type, the default
// channel pointer, and the member's follow record for the channel.
func (r *pushOpenRig) appFiles() []proto.KVPath {
	return []proto.KVPath{
		"/admin-meta/.keep",
		"/admin-meta/displayName",
		"/admin-meta/type",
		"/admin-meta/channel-default",
		proto.KVPath("/channel-members/" + hex.EncodeToString(r.chid[:]) + "/owner"),
	}
}

func makePushOpenRig(t *testing.T) *pushOpenRig {
	base := makeColdThreadViewRig(t)
	r := &pushOpenRig{
		coldThreadViewRig: base,
		kvCfg: lcl.KVConfig{
			ActingAs: team.WrapNamed(*base.fqt),
			Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		},
	}
	m := libkv.NewMetaContext(r.tew.NewClientMetaContextAtHome(t, r.owner, r.home))
	kvm := libkv.NewMinder(m.G().ActiveUser())
	putCfg := r.kvCfg
	putCfg.MkdirP = true
	for _, p := range r.appFiles() {
		writeRandomFileWithConfig(t, kvm, m, p, 40, putCfg, 0)
	}
	// The earlier run: everything the replay reads, read once over this home.
	r.replayCurrentTap(t, r.tew.NewClientMetaContextAtHome(t, r.owner, r.home))
	return r
}

// replayCurrentTap makes the agent calls the SECO app makes, in its order,
// from a cold-start tap on a message push to the tapped chat's messages.
// Each call goes through the agent's own entry (libkv/librt InitReq, which
// resolves the team first) in a context of its own, as each agent RPC has.
func (r *pushOpenRig) replayCurrentTap(t *testing.T, base libclient.MetaContext) {
	tm := team.WrapNamedPtr(r.fqt)
	files := r.appFiles()
	kvGet := func(p proto.KVPath) {
		m := libkv.NewMetaContext(base)
		kvm, err := libkv.InitReq(m, r.fqt)
		require.NoError(t, err)
		_, err = kvm.GetFile(m, r.kvCfg, p)
		require.NoError(t, err)
	}

	// The Chats list: membership dump, then the listing, then each
	// community's display name and type (admin-meta resolved first).
	tmm, err := base.G().TeamMinder()
	require.NoError(t, err)
	_, err = tmm.DumpMembershipChain(base)
	require.NoError(t, err)
	_, err = tmm.ListMemberships(base, nil)
	require.NoError(t, err)
	for _, p := range files[0:3] {
		kvGet(p)
	}

	// refreshChannels: the channel listing alongside the default-channel
	// pointer, then the follow record of each channel.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m := librt.NewMetaContext(base)
		mr, err := librt.InitReq(m, tm)
		require.NoError(t, err)
		_, err = mr.ListAllChannelsForTeam(m, tm, proto.RTAppID_Chat)
		require.NoError(t, err)
	}()
	go func() {
		defer wg.Done()
		kvGet(files[3])
	}()
	wg.Wait()
	kvGet(files[4])

	// The chat screen's read.
	m := librt.NewMetaContext(base)
	mr, err := librt.InitReq(m, tm)
	require.NoError(t, err)
	_, err = mr.GetThreadView(m, tm, proto.RTAppID_Chat, r.spec(), 0, 0)
	require.NoError(t, err)
}

// TestPushOpenReplay: a cold-start tap on a message push, as the app makes
// its agent calls today, in sequential round trips from the agent being ready.
func TestPushOpenReplay(t *testing.T) {
	r := makePushOpenRig(t)
	r.sendAs(t, r.otherM, r.otherR, "the pushed message")

	base := r.tew.NewClientMetaContextAtHome(t, r.owner, r.home)
	base, stats := withSimulatedLatency(base).WithRpcStats()
	r.replayCurrentTap(t, base)
	t.Logf("push open, current app order: %d RPCs, depth %d, by method %v",
		stats.Calls(), stats.Depth(), rpcCountsByMethod(stats))
}

// replayDirectTap is the tap once the notification carries what the app
// needs to navigate (host, DM or community, default channel or not): the
// chat opens at once, and the one call between the tap and its messages is
// the chat screen's read. The Chats list and channel directory still load,
// but nothing waits for them.
func (r *pushOpenRig) replayDirectTap(t *testing.T, base libclient.MetaContext) *librt.Minder {
	tm := team.WrapNamedPtr(r.fqt)
	m := librt.NewMetaContext(base)
	mr, err := librt.InitReq(m, tm)
	require.NoError(t, err)
	view, err := mr.GetThreadView(m, tm, proto.RTAppID_Chat, r.spec(), 0, 0)
	require.NoError(t, err)
	require.Equal(t, "the pushed message", string(view.Msgs[0].Body))
	return mr
}

// TestPushOpenDirect: the tap that opens the chat straight from the
// notification, in sequential round trips from the agent being ready.
func TestPushOpenDirect(t *testing.T) {
	r := makePushOpenRig(t)
	r.sendAs(t, r.otherM, r.otherR, "the pushed message")

	base := r.tew.NewClientMetaContextAtHome(t, r.owner, r.home)
	base, stats := withSimulatedLatency(base).WithRpcStats()
	mr := r.replayDirectTap(t, base)
	t.Logf("push open, direct: %d RPCs, depth %d, by method %v",
		stats.Calls(), stats.Depth(), rpcCountsByMethod(stats))
	// The client certificate (if not saved yet), then the team and the read
	// at once.
	require.LessOrEqual(t, stats.Depth(), 2)
	// The tapped message is marked read, once it has been shown.
	mr.WaitLaterMarks()
	require.Equal(t, proto.RTMsgSeq(3), r.ownerReadThrough(t))
}
