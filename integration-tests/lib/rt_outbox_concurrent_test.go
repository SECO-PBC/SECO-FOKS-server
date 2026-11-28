// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// TestRTOutboxConcurrentSendHandOff: Send C is queued behind A, and a drain
// running beside it settles C before C's own drain looks at the channel. C's
// drain then finds nothing queued, but it must still come back with what the
// other drain decided -- the ack, or the refusal -- rather than "queued for
// delivery on reconnect". The other drain is held in the window between
// settling C's row and handing C the outcome, which is where C's drain looks.
func TestRTOutboxConcurrentSendHandOff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse bool
	}{
		{name: "acked"},
		{name: "refused", refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := makeRTOutboxTestRig(t)
			r.minder.SetTestHooks(&librt.MinderTestHooks{
				SendRPC: func(librt.MetaContext, rem.RTSendArg) (*rem.RTSendRes, error) {
					return nil, simTransportErr()
				},
			})
			r.sendExpectQueued(t, "a")

			var cID proto.RTMsgID
			enqueued := make(chan proto.RTMsgID, 1)
			releaseC := make(chan struct{})
			cDone := make(chan struct{})
			var rpcs atomic.Int32
			r.minder.SetTestHooks(&librt.MinderTestHooks{
				MutateSendOutcome: func(res *rem.RTSendRes, err error) (*rem.RTSendRes, error) {
					// The drain sends A, then C.
					if tc.refuse && rpcs.Add(1) == 2 {
						return nil, core.BadArgsError("simulated refusal")
					}
					return res, err
				},
				AfterOutboxSettle: func(id proto.RTMsgID) {
					if id != cID {
						return
					}
					close(releaseC)
					// Let C's drain run inside the window. C must wait for
					// this drain, so don't wait for C forever.
					select {
					case <-cDone:
					case <-time.After(500 * time.Millisecond):
					}
				},
			})

			var cRes *rem.RTSendRes
			var cErr error
			go func() {
				defer close(cDone)
				cRes, cErr = r.minder.SendWithTestHooks(r.mb, team.WrapNamedPtr(r.fqt),
					proto.RTAppID_Chat, makeChannelSpecifierWithString("foo"), []byte("c"),
					&librt.SendTestHooks{BeforeDrain: func(id proto.RTMsgID) {
						enqueued <- id
						<-releaseC
					}})
			}()
			cID = <-enqueued

			dres, err := r.minder.Drain(r.mb)
			require.NoError(t, err)
			require.NoError(t, dres.TransportErr)
			<-cDone

			rows, err := r.minder.ListOutbox(r.mb)
			require.NoError(t, err)
			require.Len(t, rows, 0)
			if tc.refuse {
				require.ErrorIs(t, cErr, core.BadArgsError("simulated refusal"))
				require.Nil(t, cRes)
				return
			}
			require.NoError(t, cErr)
			require.Equal(t, proto.RTMsgSeq(2), cRes.Seq)
			msgs := r.thread(t, 1, 2)
			require.Len(t, msgs, 2)
			require.Equal(t, "a", string(msgs[0].Body))
			require.Equal(t, "c", string(msgs[1].Body))
		})
	}
}

// TestRTOutboxSendStopsWaitingForDrain: a Send queued behind a drain of its
// channel that is stuck on the network gives up waiting when its own
// context ends -- the message stays queued -- instead of waiting the other
// drain out. The next drain delivers it in order.
func TestRTOutboxSendStopsWaitingForDrain(t *testing.T) {
	r := makeRTOutboxTestRig(t)
	r.minder.SetTestHooks(&librt.MinderTestHooks{
		SendRPC: func(librt.MetaContext, rem.RTSendArg) (*rem.RTSendRes, error) {
			return nil, simTransportErr()
		},
	})
	r.sendExpectQueued(t, "a")

	inFlight := make(chan struct{})
	releaseX := make(chan struct{})
	var rpcs atomic.Int32
	r.minder.SetTestHooks(&librt.MinderTestHooks{
		MutateSendOutcome: func(res *rem.RTSendRes, err error) (*rem.RTSendRes, error) {
			if rpcs.Add(1) == 1 {
				close(inFlight)
				<-releaseX
			}
			return res, err
		},
	})
	xDone := make(chan struct{})
	go func() {
		defer close(xDone)
		_, _ = r.minder.Drain(r.mb)
	}()
	<-inFlight

	mcBase, cancel := r.mb.Base().WithContextCancel()
	mc := librt.NewMetaContext(mcBase)
	defer cancel()
	cDone := make(chan error, 1)
	go func() {
		_, err := r.minder.SendWithTestHooks(mc, team.WrapNamedPtr(r.fqt),
			proto.RTAppID_Chat, makeChannelSpecifierWithString("foo"), []byte("c"),
			&librt.SendTestHooks{BeforeDrain: func(proto.RTMsgID) {
				// Cancel once the Send is waiting on the stuck drain.
				time.AfterFunc(300*time.Millisecond, cancel)
			}})
		cDone <- err
	}()
	select {
	case err := <-cDone:
		var qerr core.RTMsgQueuedError
		require.True(t, errors.As(err, &qerr), "expected RTMsgQueuedError, got %v", err)
	case <-time.After(10 * time.Second):
		close(releaseX)
		t.Fatal("Send kept waiting for the other drain after its context ended")
	}

	close(releaseX)
	<-xDone
	r.minder.SetTestHooks(nil)
	dres, err := r.minder.Drain(r.mb)
	require.NoError(t, err)
	require.NoError(t, dres.TransportErr)
	msgs := r.thread(t, 1, 2)
	require.Len(t, msgs, 2)
	require.Equal(t, "a", string(msgs[0].Body))
	require.Equal(t, "c", string(msgs[1].Body))
}
