package lib

// Realtime invariants, checkable after any operation.
//
// These are properties the realtime schema assumes everywhere and states
// nowhere, so nothing notices when one stops holding. The unarchive bug is the
// case in point: it allocated an inbox version that stamped no row, which left
// every client's sync cursor permanently below the head -- SyncInbox stopping
// on an empty delta and PollInbox returning immediately, forever. No test
// failed. The invariant it broke fits in one query.
//
// Written as a suite a test calls after whatever it just did, rather than as
// tests of their own, because the value is in running them at the end of
// scenarios someone else wrote for another reason:
//
//	defer sc.requireRTInvariants(t)
//
// Asserted against a BASELINE, not against zero. Every test in this package
// shares one postgres, and one of these is legitimately violated elsewhere:
// the revoke path bumps a user's inbox version without stamping a row ("bump,
// don't stamp" in dropChannelMember), deliberately, so the revoked member's
// next sync is a full one. Asserting zero would fail in whichever test
// happened to run after a revoke -- wrong, and maddening to debug. A baseline
// also makes the check say what is actually meant: not "the database is
// pristine" but "this operation did not break anything".

import (
	"testing"

	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// rtInvariantCounts counts each violation across the whole test database.
// Whole-database on purpose: a mutation that corrupts a bystander channel's
// rows is exactly what a per-channel assertion misses.
func (s *privScene) rtInvariantCounts(t *testing.T) map[string]int {
	t.Helper()
	return map[string]int{
		// Every inbox version is allocated by bumping user_inbox and is
		// supposed to stamp exactly one user_channels row. A version that
		// stamps nothing is invisible but not harmless: a client's cursor can
		// only advance to a version it has seen on a row.
		"orphan inbox versions": s.rtdbScalar(t, `
			SELECT count(*) FROM user_inbox ui
			WHERE ui.inbox_version > COALESCE(
			    (SELECT max(uc.inbox_version) FROM user_channels uc
			     WHERE uc.short_host_id = ui.short_host_id
			       AND uc.uid = ui.uid
			       AND uc.app_id = ui.app_id), 0)`),

		// user_channels_inbox_idx is UNIQUE, so a violation means the index
		// was dropped or recreated without it.
		"duplicate inbox versions": s.rtdbScalar(t, `
			SELECT count(*) FROM (
			    SELECT short_host_id, uid, app_id, inbox_version
			    FROM user_channels
			    GROUP BY 1,2,3,4 HAVING count(*) > 1
			) dup`),

		"rows above their inbox head": s.rtdbScalar(t, `
			SELECT count(*) FROM user_channels uc
			WHERE uc.inbox_version > COALESCE(
			    (SELECT ui.inbox_version FROM user_inbox ui
			     WHERE ui.short_host_id = uc.short_host_id
			       AND ui.uid = uc.uid
			       AND ui.app_id = uc.app_id), 0)`),

		"delivery rows with no channel": s.rtdbScalar(t, `
			SELECT count(*) FROM user_channels uc
			WHERE NOT EXISTS (
			    SELECT 1 FROM channels c
			    WHERE c.short_host_id = uc.short_host_id
			      AND c.channel_id = uc.channel_id)`),

		// The private-channel guarantee rests on these two being the same set:
		// the send fan-out targets user_channels, so a delivery row without an
		// ACL row is a non-member receiving a private channel's traffic.
		"private delivery without ACL": s.rtdbScalar(t, `
			SELECT count(*) FROM user_channels uc
			JOIN channels c ON c.short_host_id = uc.short_host_id
			                AND c.channel_id = uc.channel_id
			WHERE c.private AND NOT EXISTS (
			    SELECT 1 FROM channel_acl a
			    WHERE a.short_host_id = uc.short_host_id
			      AND a.channel_id = uc.channel_id AND a.uid = uc.uid)`),

		"private ACL without delivery": s.rtdbScalar(t, `
			SELECT count(*) FROM channel_acl a
			WHERE NOT EXISTS (
			    SELECT 1 FROM user_channels uc
			    WHERE uc.short_host_id = a.short_host_id
			      AND uc.channel_id = a.channel_id AND uc.uid = a.uid)`),
	}
}

// rtInvariantWhy says what each violation costs. A count that moved from 3 to
// 4 is not a debuggable message on its own.
var rtInvariantWhy = map[string]string{
	"orphan inbox versions": "A user_inbox head now sits above every version stamped on " +
		"that user's channel rows. A version that stamps no row cannot be reached by a " +
		"client's cursor, so head stays permanently above it: SyncInbox stops on an empty " +
		"delta without advancing, and PollInbox returns instantly forever after. Something " +
		"allocated an inbox version and skipped the write meant to use it.",
	"duplicate inbox versions": "Two of a user's channel rows now share an inbox version. " +
		"get_changed_threads pages by version, so a group split across a page boundary is " +
		"skipped for good. user_channels_inbox_idx should make this impossible -- check it " +
		"is still UNIQUE.",
	"rows above their inbox head": "A channel row is stamped above its user's inbox head. " +
		"The head is the allocator, so nothing should ever be stamped past it; a client " +
		"syncing to the head would never see these rows.",
	"delivery rows with no channel": "A delivery row points at a channel that does not " +
		"exist, and the inbox sync joins these.",
	"private delivery without ACL": "Someone holds a delivery row for a private channel " +
		"they are not in. The send fan-out targets user_channels, so that is a private " +
		"channel's traffic reaching a non-member -- the one thing the design must not do.",
	"private ACL without delivery": "Someone is in a private channel's ACL but has no " +
		"delivery row, so they receive nothing from it.",
}

// requireRTInvariants asserts that nothing the test just did made any
// invariant worse than it was when the scene was built.
func (s *privScene) requireRTInvariants(t *testing.T) {
	t.Helper()
	require.NotNil(t, s.invBaseline,
		"no invariant baseline: the scene was not built by setupPrivScene")
	for name, now := range s.rtInvariantCounts(t) {
		require.LessOrEqualf(t, now, s.invBaseline[name],
			"invariant %q went from %d to %d during this test.\n\n%s",
			name, s.invBaseline[name], now, rtInvariantWhy[name])
	}
}

// rtdbScalar runs a one-value query against the realtime DB.
func (s *privScene) rtdbScalar(t *testing.T, q string, args ...any) int {
	t.Helper()
	m := s.tew.MetaContext()
	db, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var n int
	require.NoError(t, db.QueryRow(m.Ctx(), q, args...).Scan(&n))
	return n
}
