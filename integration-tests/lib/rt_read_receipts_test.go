// Read receipts (fork-only; dm-read-receipts in SECO-FOKS,
// server/realtime/receipts.go). The contract under test:
//   - a member's explicit pointer reaches the other member's inbox row
//     (peerReadThrough) only on a two-row channel whose rows are both marked
//     as a DM (MarkRead with dm), and only while neither has receipts off;
//   - a read that moves the pointer there, or the mark itself, bumps the
//     other member's inbox, so their sync returns the row;
//   - the switch re-stamps both sides of every qualifying channel and never
//     leaves an inbox version with no row stamped at it;
//   - two members reading at once, against a send to both, never deadlock.
package lib

import (
	"sync"
	"testing"

	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func (s *privScene) markReadDM(t *testing.T, by *privActor, seq proto.RTMsgSeq) {
	require.NoError(t, by.minder.MarkRead(by.m, s.teamCfg(), proto.RTAppID_Chat,
		s.spec(), seq, true))
}

// peerReadThrough is the peerReadThrough on `a`'s inbox row for the scene's
// channel, after a sync.
func (s *privScene) peerReadThrough(t *testing.T, a *privActor) proto.RTMsgSeq {
	view, err := a.minder.InboxView(a.m, proto.RTAppID_Chat, false)
	require.NoError(t, err)
	for _, row := range view.Rows {
		if row.Ch.Id == s.chid {
			return row.PeerReadThrough
		}
	}
	t.Fatalf("no inbox row for the channel")
	return 0
}

func (s *privScene) setReceipts(t *testing.T, a *privActor, on bool) {
	require.NoError(t, a.minder.SetReadReceipts(a.m, proto.RTAppID_Chat, on))
}

func TestReadReceiptsNeedBothMarks(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	s1 := sc.send(t, sc.alice, "hi bob")

	// Only bob's app has marked the DM: nothing is shared either way.
	sc.markReadDM(t, sc.bob, s1)
	require.Equal(t, proto.RTMsgSeq(0), sc.peerReadThrough(t, sc.alice))
	require.Equal(t, proto.RTMsgSeq(0), sc.peerReadThrough(t, sc.bob))

	// Alice's first marked read carries a seq her own send already reached;
	// it still marks her row, and wakes bob.
	bobBefore := sc.inboxVersion(t, sc.bob)
	sc.markReadDM(t, sc.alice, s1)
	require.Equal(t, bobBefore+1, sc.inboxVersion(t, sc.bob))
	require.Equal(t, s1, sc.peerReadThrough(t, sc.alice))
	require.Equal(t, s1, sc.peerReadThrough(t, sc.bob))

	// A read by bob reaches alice; a repeated one bumps nobody.
	s2 := sc.send(t, sc.alice, "still there?")
	aliceBefore := sc.inboxVersion(t, sc.alice)
	sc.markReadDM(t, sc.bob, s2)
	require.Equal(t, aliceBefore+1, sc.inboxVersion(t, sc.alice))
	require.Equal(t, s2, sc.peerReadThrough(t, sc.alice))
	sc.markReadDM(t, sc.bob, s2)
	require.Equal(t, aliceBefore+1, sc.inboxVersion(t, sc.alice))

	sc.requireRTInvariants(t)
}

func TestReadReceiptsNotOnThreeMembers(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	sc.grant(t, sc.alice, sc.cleo, false)
	s1 := sc.send(t, sc.alice, "hi all")
	for _, a := range []*privActor{sc.alice, sc.bob, sc.cleo} {
		sc.markReadDM(t, a, s1)
	}
	for _, a := range []*privActor{sc.alice, sc.bob, sc.cleo} {
		require.Equal(t, proto.RTMsgSeq(0), sc.peerReadThrough(t, a))
	}
	sc.requireRTInvariants(t)
}

func TestReadReceiptsSwitch(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	s1 := sc.send(t, sc.alice, "hi bob")
	sc.markReadDM(t, sc.bob, s1)
	sc.markReadDM(t, sc.alice, s1)
	require.Equal(t, s1, sc.peerReadThrough(t, sc.alice))

	on, err := sc.alice.minder.ReadReceipts(sc.alice.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	require.True(t, on)

	// Off on one side withholds both ways, and tells both sides.
	bobBefore := sc.inboxVersion(t, sc.bob)
	sc.setReceipts(t, sc.alice, false)
	require.Equal(t, bobBefore+1, sc.inboxVersion(t, sc.bob))
	require.Equal(t, proto.RTMsgSeq(0), sc.peerReadThrough(t, sc.alice))
	require.Equal(t, proto.RTMsgSeq(0), sc.peerReadThrough(t, sc.bob))
	on, err = sc.alice.minder.ReadReceipts(sc.alice.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	require.False(t, on)

	// While off, bob's reads wake nobody.
	s2 := sc.send(t, sc.alice, "again")
	aliceBefore := sc.inboxVersion(t, sc.alice)
	sc.markReadDM(t, sc.bob, s2)
	require.Equal(t, aliceBefore, sc.inboxVersion(t, sc.alice))

	// The same value again is a no-op.
	sc.setReceipts(t, sc.alice, false)
	require.Equal(t, aliceBefore, sc.inboxVersion(t, sc.alice))

	// Back on: each side's pointer shows again (alice's own send of s2 moved
	// hers).
	sc.setReceipts(t, sc.alice, true)
	require.Equal(t, s2, sc.peerReadThrough(t, sc.alice))
	require.Equal(t, s2, sc.peerReadThrough(t, sc.bob))

	sc.requireRTInvariants(t)
}

func TestReadReceiptsConcurrentReadsDoNotDeadlock(t *testing.T) {
	sc := setupPrivScene(t, false)
	sc.grant(t, sc.alice, sc.bob, false)
	s1 := sc.send(t, sc.alice, "seed")
	sc.markReadDM(t, sc.alice, s1)
	sc.markReadDM(t, sc.bob, s1)

	const rounds = 20
	errs := make(chan error, 3*rounds)
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		res, err := sc.alice.minder.Send(sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat,
			sc.spec(), []byte("round"))
		require.NoError(t, err)
		seq := res.Seq
		wg.Add(3)
		for _, a := range []*privActor{sc.alice, sc.bob} {
			go func(a *privActor) {
				defer wg.Done()
				errs <- a.minder.MarkRead(a.m, sc.teamCfg(), proto.RTAppID_Chat,
					lcl.NewRTChannelSpecifierWithId(sc.chid), seq, true)
			}(a)
		}
		go func() {
			defer wg.Done()
			_, err := sc.bob.minder.Send(sc.bob.m, sc.teamCfg(), proto.RTAppID_Chat,
				sc.spec(), []byte("reply"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.False(t, isDeadlock(err), "deadlock: %v", err)
		require.NoError(t, err)
	}
	sc.requireRTInvariants(t)
}
