package lib

// Concurrency stress for the realtime write paths.
//
// TestRealtimeLockOrder (server/realtime) reads source and catches a
// transaction whose locks go out of order. It cannot catch ordering that only
// exists at runtime -- a helper reached from two transactions in different
// states, a branch taken only under contention, an interleaving nobody drew on
// a whiteboard. This test does the other half: run every write path against
// one team at once and see what breaks.
//
// It is deliberately not a precise test. It asserts three things that must
// hold no matter how the operations interleave:
//
//  1. nothing returns a Postgres deadlock or serialization error. Those are
//     the failure this whole exercise is about, and the RTRaceError retry
//     loops do not absorb them -- they surface to the caller as a failed RPC.
//  2. every error that does come back is one the API is allowed to return
//     (a lost CAS, a permission denial, an archived channel), never a raw
//     database error.
//  3. the invariants in rt_invariants_test.go still hold at the end.
//
// Failures here are real but may not reproduce; the seed and the operation log
// are printed so a failing run can be replayed.

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// isDeadlock reports whether err is Postgres giving up on a lock cycle
// (40P01) or a serialization failure (40001). Matched on text because the
// error crosses an RPC boundary and arrives as a status, not a pgconn.PgError.
func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "deadlock") ||
		strings.Contains(s, "40p01") ||
		strings.Contains(s, "40001") ||
		strings.Contains(s, "could not serialize")
}

// allowedUnderContention reports whether err is one the API may legitimately
// return when several writers race. Anything else is a bug -- in particular a
// raw database error, which means a transaction failed in a way no caller can
// interpret.
func allowedUnderContention(err error) bool {
	if err == nil {
		return true
	}
	switch err.(type) {
	case core.RTRaceError, // lost the metadata CAS; the caller retries
		core.RTChannelArchivedError, // raced an archive
		core.RTChannelExistsError,   // raced a rename onto the same name
		core.PermissionError,        // not an admin
		core.RowNotFoundError,       // raced something that removed the row
		core.RTGenericError,         // e.g. refusing the default channel
		core.RTNotFoundError:        // raced something that archived or removed it
		return true
	}
	return false
}

// rtStressOps weights the operation mix; see the comment at its use.
var rtStressOps = []int{0, 1, 1, 2, 3, 4, 4}

// TestRealtimeConcurrentWrites drives sends, renames, archives, unarchives,
// read-throughs and inbox syncs against one team from several goroutines.
//
// Short by default so it can sit in CI; raise rounds locally when chasing
// something. The operations are chosen to overlap on exactly the rows that
// have deadlocked before: channels, user_inbox, user_channels, push_outbox and
// the team's single channel_sets row.
func TestRealtimeConcurrentWrites(t *testing.T) {
	sc := setupMutScene(t)

	// A second channel, so a mutation on one races a send on the other -- the
	// case where two transactions share the team's channel_sets row without
	// sharing a channel row to serialize on. That is precisely the shape of
	// the rename/create deadlock.
	otherName := randomChannelName(t, "other-")
	otherID, err := sc.alice.minder.MakeChannel(
		sc.alice.m, sc.teamCfg(), proto.RTAppID_Chat, otherName, "second channel",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	)
	require.NoError(t, err)

	seed := time.Now().UnixNano()
	t.Logf("seed %d (pass -run TestRealtimeConcurrentWrites and this seed to replay)", seed)

	const rounds = 40
	type actor struct {
		name string
		a    *privActor
	}
	// alice and dara are admins and may mutate; bob and cleo are ordinary
	// members and may only send and read, so their attempts also exercise the
	// permission gate under contention.
	actors := []actor{
		{"alice", sc.alice}, {"dara", sc.dara},
		{"bob", sc.bob}, {"cleo", sc.cleo},
	}

	var mu sync.Mutex
	var log []string
	record := func(who, op string, err error) {
		mu.Lock()
		defer mu.Unlock()
		log = append(log, fmt.Sprintf("%s %s -> %v", who, op, err))
		require.Falsef(t, isDeadlock(err),
			"%s %s returned a database deadlock/serialization error: %v\n\n"+
				"Two transactions took row locks in opposite orders. "+
				"TestRealtimeLockOrder checks the orders it can see in source; this "+
				"one only appears when they interleave, so the culprit is likely a "+
				"helper reached from two transactions in different states.\n\nlog:\n%s",
			who, op, err, strings.Join(log, "\n"))
		require.Truef(t, allowedUnderContention(err),
			"%s %s returned an error the API should never produce: %v (%T)\n\n"+
				"Under contention a caller may see a lost CAS, a permission denial, an "+
				"archived channel or a missing row. A raw database error means a "+
				"transaction failed in a way no caller can act on.\n\nlog:\n%s",
			who, op, err, err, strings.Join(log, "\n"))
	}

	var wg sync.WaitGroup
	for i := range actors {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ac := actors[idx]
			rng := rand.New(rand.NewSource(seed + int64(idx)))
			for r := 0; r < rounds; r++ {
				target := sc.pubSpec()
				tname := "pub"
				if rng.Intn(2) == 0 {
					target = sc.specFor(*otherID)
					tname = "other"
				}
				// Weighted, not uniform. Rename and create are the pair whose
				// orders actually collided -- a rename holds the team's
				// channel_sets row and wants a member's user_inbox row, a
				// create holds that member and wants channel_sets -- and a
				// uniform mix put too few of them next to each other to find
				// it. Verified: with the ordering bug reintroduced this
				// catches SQLSTATE 40P01; with a uniform mix it did not.
				switch rtStressOps[rng.Intn(len(rtStressOps))] {
				case 0:
					_, err := ac.a.minder.Send(ac.a.m, sc.teamCfg(),
						proto.RTAppID_Chat, target, []byte("hello"))
					record(ac.name, "send "+tname, err)
				case 1:
					err := ac.a.minder.UpdateChannel(ac.a.m, sc.teamCfg(),
						proto.RTAppID_Chat, target, randomChannelName(t, "c-"), "")
					record(ac.name, "rename "+tname, err)
				case 2:
					err := ac.a.minder.SetChannelArchived(ac.a.m, sc.teamCfg(),
						proto.RTAppID_Chat, target, rng.Intn(2) == 0)
					record(ac.name, "archive "+tname, err)
				case 3:
					_, err := ac.a.minder.SyncInbox(ac.a.m, proto.RTAppID_Chat)
					record(ac.name, "sync", err)
				case 4:
					// A brand-new channel shares only the team's channel_sets
					// row with everything above -- the create/rename pair that
					// deadlocked.
					if idx%2 == 0 {
						_, err := ac.a.minder.MakeChannel(ac.a.m, sc.teamCfg(),
							proto.RTAppID_Chat, randomChannelName(t, "n-"), "",
							proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole})
						record(ac.name, "create", err)
					} else {
						_, err := ac.a.minder.GetThreadRecentMsgs(ac.a.m, sc.teamCfg(),
							proto.RTAppID_Chat, target, 5)
						record(ac.name, "read "+tname, err)
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// Leave both channels live, so the invariant pass at the end runs against
	// a team in its ordinary state rather than a half-archived one.
	for _, id := range []lcl.RTChannelSpecifier{sc.pubSpec(), sc.specFor(*otherID)} {
		// Required, not logged: if this fails the team is left archived and the
		// invariant pass that follows would be judging a state no test meant
		// to create.
		require.NoError(t, sc.alice.minder.SetChannelArchived(sc.alice.m,
			sc.teamCfg(), proto.RTAppID_Chat, id, false))
	}
	t.Logf("%d operations completed", len(log))
}
