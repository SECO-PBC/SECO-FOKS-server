package realtime

// The "bug in three years" guard (§8.3 of docs/rt-private-channel-acl.md).
//
// Private channels are enforced by policy, not by keys: every team member at
// the channel's read role holds the key, and there is no rekey when channel
// membership changes. So a path that returns channel or message data without
// consulting the ACL is a silent, retroactive privacy failure -- nothing
// throws, no existing test goes red, the data is simply readable by someone
// who should not see it.
//
// Two guards, both source-level, both intentionally annoying to a future
// author who adds a path without thinking about privacy:
//
//  1. Every RPC in the RealTime protocol must be classified below, with a
//     pointer to the test that covers it.
//  2. Every query in this package against the tables that hold channel data
//     must sit in an allowlisted function -- the chokepoint, or one of the two
//     set-based paths that embed its predicate.
//
// This test lives in the server package, not integration-tests/lib, on purpose:
// CI runs ./server/... but does not run the integration suite (it needs a live
// postgres), and a guard that CI does not run is not a guard.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// accessClass says what kind of data an RPC can return to its caller.
type accessClass string

const (
	// noChannelData: cannot return channel rows, message rows, or channel
	// activity metadata for any channel.
	noChannelData accessClass = "noChannelData"
	// channelData: returns channel rows, message rows, or channel activity
	// metadata -- must be gated by authorizeChannel or an equivalent
	// set-based predicate.
	channelData accessClass = "channelData"
	// channelWrite: mutates a channel or its membership.
	channelWrite accessClass = "channelWrite"
)

type rpcClassification struct {
	class accessClass
	// covers names the test(s) that exercise this RPC's private-channel
	// behaviour, or says why none is needed.
	covers string
}

// rpcAccessClass classifies every method of the RealTime protocol. Adding an
// RPC without adding an entry here fails this test -- which is the point.
var rpcAccessClass = map[string]rpcClassification{
	"rtNewChannel": {
		channelWrite,
		"TestPrivateCreateRequiresAdmin, TestPrivateCreateFansOutToCreatorOnly (row 11)",
	},
	"rtGetChannel": {
		channelData,
		"unimplemented (returns NotImplementedError); when implemented it must " +
			"go through authorizeChannel(accessRead) -- the source guard below " +
			"fails the build if it does not (row 6)",
	},
	"rtListAllChannelsForTeam": {
		channelData,
		"TestPrivateChannelInvisibleInList (rows 5 and 14)",
	},
	"rtSend": {
		channelWrite,
		"TestPrivateSendDeniedToNonMember, TestPrivateFanoutOnlyMembers, " +
			"TestPrivatePushOutboxOnlyMembers (rows 3, 4, 15)",
	},
	"rtGetThread": {
		channelData,
		"TestPrivateThreadDeniedToNonMember (row 1)",
	},
	"rtGetInboxVersion": {
		noChannelData,
		"returns one integer, no channel identity; correct iff the fan-out is " +
			"(row 8), covered by TestPrivateNoInboxBumpForNonMember",
	},
	"rtGetChangedThreads": {
		channelData,
		"TestPrivateChangedThreadsAfterRevoke, TestPrivateNotFannedInOnJoin (rows 7, 9)",
	},
	"rtReadThrough": {
		channelWrite,
		"TestPrivateReadThroughAfterRevoke (row 10)",
	},
	"rtPollInbox": {
		noChannelData,
		"returns one integer, no channel identity (row 8)",
	},
	"rtSelectVHost": {
		noChannelData,
		"no channel data (row 13)",
	},
	"rtGetThreadRecents": {
		channelData,
		"TestPrivateRecentsDeniedToNonMember (row 2)",
	},
	"rtSetPushToken": {
		noChannelData,
		"no channel data (row 13)",
	},
	"rtChannelGrant": {
		channelWrite,
		"TestPrivateGrantRequiresOwnerOrAdmin (row 12)",
	},
	"rtChannelRevoke": {
		channelWrite,
		"TestPrivateChangedThreadsAfterRevoke, TestPrivateReadThroughAfterRevoke (row 12)",
	},
	"rtChannelMembers": {
		channelData,
		"TestPrivateGrantRequiresOwnerOrAdmin (row 12)",
	},
}

// protectedTables are the tables that hold channel data, message data, or
// channel activity metadata for a particular caller. A query against any of
// them, in this package, must live in an allowlisted function.
var protectedTables = []string{
	"channels",
	"messages_enc",
	"messages_clear",
	"user_channels",
	"channel_parties",
	"channel_sets",
	"channel_acl",
}

// allowedQueries is one signed-off function: how many references to protected
// tables its body is expected to contain, and why reaching that data there is
// safe.
//
// The count matters as much as the name. Keying only on the function name would
// catch a protected query landing in a NEW function while missing a second one
// slipped into an already-allowlisted one -- and the allowlisted functions are
// precisely where a careless future path is most likely to be added, since they
// are already full of this SQL. Pinning the count makes any added reference,
// anywhere, trip the guard and force its author to re-justify the total.
type allowedQueries struct {
	n   int
	why string
}

// queryAllowlist maps a function that legitimately queries a protected table
// to its expected reference count and why that is safe. Everything else fails
// the test.
//
// The rule being enforced: no path may reach channel or message rows on behalf
// of a caller without first passing through authorizeChannel, or -- for the
// two paths that read many channels at once -- through the equivalent
// set-based predicate, privateVisibleToCaller / channelPrivacyCols.
var queryAllowlist = map[string]allowedQueries{
	// --- the chokepoint and its helpers (acl.go) ---
	"authorizeChannel":         {1, "the chokepoint itself"},
	"readChannelAclRole":       {1, "chokepoint step 5"},
	"privateVisibleToCaller":   {1, "the set-based form of chokepoint step 5"},
	"insertChannelAcl":         {1, "ACL write, reached only via authorizeChannel(accessManage)"},
	"dropChannelMember":        {2, "ACL + delivery write, reached only via authorizeChannel(accessManage) or the send-time prune"},
	"pruneStaleChannelMembers": {1, "team-leave cascade; reads its own channel's delivery rows inside the send transaction, after authorizeChannel"},
	"ListChannelMembers":       {1, "gated by authorizeChannel(accessRead) on the line above the query"},

	// --- set-based paths that embed the predicate ---
	"readAllChannels":     {1, "SET-BASED (inventory row 5): embeds privateVisibleToCaller"},
	"readChangedChannels": {2, "SET-BASED (inventory row 7): selects channelPrivacyCols; GetChangedThreads drops rows whose aclMember is false"},
	"findMissingChannels": {2, "SET-BASED (inventory row 9): excludes private channels outright (AND NOT c.private)"},

	// --- writes that run after the chokepoint authorized the caller ---
	"messageSender.internSender":  {2, "send path; runs after lockChannel -> authorizeChannel(accessWrite)"},
	"messageSender.insertMessage": {2, "send path; runs after lockChannel -> authorizeChannel(accessWrite)"},
	"messageSender.fanoutInboxVersions": {3, "send path; recipients are the channel's user_channels rows, " +
		"which for a private channel equal its ACL (invariant asserted by " +
		"TestPrivateAclEqualsUserChannels) and are re-validated against the team " +
		"roster by pruneStaleChannelMembers immediately before this runs"},
	"touchChannelSet":                     {2, "grant path; runs after authorizeChannel(accessManage) and touches only version bookkeeping"},
	"channelMaker.insertChannel":          {1, "creation; runs after channelMaker.checkPerms -> authorizeChannelCreate"},
	"fanUserIntoChannel":                  {1, "delivery-row write, reached from creation fan-out, grant, or the (private-excluding) fan-in"},
	"channelMaker.fanoutToUser":           {1, "thin wrapper over fanUserIntoChannel, inside the creation transaction"},
	"channelMaker.insertNewChannelSetRow": {1, "channel-set version bookkeeping; carries no channel identity to a caller"},
	"channelMaker.updateChannelSet":       {1, "channel-set version bookkeeping; carries no channel identity to a caller"},
	"readChannelSet":                      {1, "channel-set version only; no per-channel data"},

	// --- reads that run after the chokepoint authorized the caller ---
	"readThroughMarker.run": {3, "calls loadChannel -> authorizeChannel(accessRead) before anything else"},

	// --- shared SQL fragments (top-level consts). The functions that
	// interpolate these carry no protected-table reference of their own, so
	// the fragment is where the sign-off lives. ---
	"lastSenderJoin": {1, "LEFT JOIN onto channel_parties; used only by the two allowlisted set-based queries"},
	"threadMsgSelect": {2, "message column list + sender join; used only by readThreadBookends, " +
		"readThreadRecents and readMsgsBySeq, all of which run inside " +
		"getThreadGeneric after authorizeChannel(accessRead)"},
}

// rpcLine matches a method declaration in a snowp protocol block, e.g.
//
//	rtGetThread @4 (
var rpcLine = regexp.MustCompile(`^\s*([a-z][A-Za-z0-9]*)\s+@(\d+)\s*[(:]`)

// TestRealtimeRpcInventory fails when a RealTime RPC is added without being
// classified against the private-channel path inventory (§5 of the spec).
func TestRealtimeRpcInventory(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "proto-src", "rem", "realtime.snowp"))
	require.NoError(t, err)

	var found []string
	inProtocol := false
	depth := 0
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "protocol RealTime") {
			inProtocol = true
			continue
		}
		if !inProtocol || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if depth == 0 && trimmed == "}" {
			break
		}
		// Only a line at the protocol's top level declares an RPC; the same
		// shape one level in is an argument (`md @0 : ...`).
		if depth == 0 {
			if mm := rpcLine.FindStringSubmatch(line); mm != nil {
				found = append(found, mm[1])
			}
		}
		depth += strings.Count(line, "(") - strings.Count(line, ")")
	}

	// Sanity: if the parse silently stops matching, every other assertion here
	// passes vacuously and the guard is worthless.
	require.GreaterOrEqual(t, len(found), 15,
		"parsed too few RPCs out of realtime.snowp -- the parser has drifted from the syntax, "+
			"which would make this guard silently vacuous")

	for _, nm := range found {
		cls, ok := rpcAccessClass[nm]
		require.True(t, ok,
			"RPC %q is not classified in rpcAccessClass. Every RealTime RPC must be "+
				"classified as noChannelData / channelData / channelWrite, and -- if it "+
				"can return channel data -- gated by authorizeChannel (or one of the two "+
				"set-based predicates) and covered by a private-channel test. "+
				"See docs/rt-private-channel-acl.md §5.", nm)
		require.NotEmpty(t, cls.covers, "RPC %q is classified but names no covering test", nm)
	}

	// The reverse direction: a classification left behind by a removed RPC is
	// dead weight that makes the map look more complete than it is.
	sort.Strings(found)
	for nm := range rpcAccessClass {
		idx := sort.SearchStrings(found, nm)
		require.True(t, idx < len(found) && found[idx] == nm,
			"rpcAccessClass classifies %q, which is not an RPC in realtime.snowp", nm)
	}
}

var (
	// Captures the receiver type (group 1, may be empty) and the function name
	// (group 2). The receiver matters: keying the allowlist on a bare method
	// name would let an entry justified for one type silently cover every
	// same-named method in the package, including a future one that never
	// reaches the chokepoint.
	funcDecl = regexp.MustCompile(`^func\s+(?:\(\s*\w+\s+\*?([A-Za-z0-9_]+)\s*\)\s*)?([A-Za-z0-9_]+)\s*\(`)
	// Top-level const/var/type: SQL fragments shared between functions live in
	// consts, and must be attributed to themselves rather than to whichever
	// function happens to precede them in the file.
	declLine = regexp.MustCompile(`^(?:const|var|type)\s+([A-Za-z0-9_]+)`)
	// \s+ (not [ \t]+) so a table name on the line after its keyword still
	// matches; the scan runs over a whole declaration's text, not one line.
	fromLine = regexp.MustCompile(`(?i)\b(?:FROM|INTO|UPDATE|JOIN)\s+([a-z_]+)\b`)
)

// TestRealtimeProtectedTableQueries fails when a query against a table holding
// channel data appears in a function that has not been signed off as safe.
//
// It is deliberately dumb: it does not understand SQL, it understands "this
// function names a protected table". A new query in a new function fails until
// its author says, in queryAllowlist, why reaching that data there is safe.
func TestRealtimeProtectedTableQueries(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	protected := make(map[string]bool, len(protectedTables))
	for _, tbl := range protectedTables {
		protected[tbl] = true
	}

	type hit struct {
		file, fn, tables string
		got, want        int
	}
	var offenders []hit
	var counts []hit
	var sawAny int
	seen := map[string]bool{}

	for _, e := range entries {
		nm := e.Name()
		if e.IsDir() || !strings.HasSuffix(nm, ".go") || strings.HasSuffix(nm, "_test.go") {
			continue
		}
		body, err := os.ReadFile(nm)
		require.NoError(t, err)

		// Accumulate each declaration's lines and scan the whole block at
		// once, rather than line by line: SQL here is written across many
		// lines, and a `FROM` whose table name sits on the following line
		// would be invisible to a per-line match -- a silent hole in exactly
		// the guard that is supposed to have no holes.
		curFn := "<file scope>"
		var block strings.Builder

		scanBlock := func(fn string, text string) {
			var n int
			var tables []string
			for _, mm := range fromLine.FindAllStringSubmatch(text, -1) {
				tbl := strings.ToLower(mm[1])
				if !protected[tbl] {
					continue
				}
				n++
				tables = append(tables, tbl)
			}
			if n == 0 {
				return
			}
			sawAny += n
			seen[fn] = true
			allowed, ok := queryAllowlist[fn]
			if !ok {
				offenders = append(offenders, hit{nm, fn, strings.Join(tables, ", "), n, 0})
				return
			}
			if allowed.n != n {
				counts = append(counts, hit{nm, fn, strings.Join(tables, ", "), n, allowed.n})
			}
		}

		for _, line := range strings.Split(string(body), "\n") {
			var next string
			if mm := funcDecl.FindStringSubmatch(line); mm != nil {
				next = mm[2]
				if mm[1] != "" {
					next = mm[1] + "." + mm[2]
				}
			} else if mm := declLine.FindStringSubmatch(line); mm != nil {
				next = mm[1]
			}
			if next != "" {
				scanBlock(curFn, block.String())
				block.Reset()
				curFn = next
				// Keep the declaration line's own text, attributed to the new
				// declaration: a single-line const like lastSenderJoin carries
				// its whole query on the `const ... = ` line, so dropping it
				// would hide that SQL from the guard entirely.
				block.WriteString(line)
				block.WriteString("\n")
				continue
			}
			// Skip Go comments: the prose in this package names these tables
			// constantly, and a comment cannot read a row.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			block.WriteString(line)
			block.WriteString("\n")
		}
		scanBlock(curFn, block.String())
	}

	require.Greater(t, sawAny, 10,
		"found almost no queries against the protected tables -- the scanner has "+
			"drifted and this guard is silently vacuous")

	for _, c := range counts {
		t.Errorf(
			"%s: %s() now makes %d references to protected tables (%s), but "+
				"queryAllowlist records %d.\n"+
				"A query was added to a function that was already signed off. That is "+
				"exactly where an ungated read is easiest to miss, because the function "+
				"is already full of this SQL. Confirm the new one reaches channel or "+
				"message rows only after authorizeChannel (or behind "+
				"privateVisibleToCaller / channelPrivacyCols), then update the count and "+
				"the justification in queryAllowlist, and add a test. "+
				"See docs/rt-private-channel-acl.md §5 and §8.3.",
			c.file, c.fn, c.got, c.tables, c.want)
	}

	// A stale entry makes the allowlist look more considered than it is, and
	// leaves a name signed off for a function that no longer exists -- which a
	// future function could then reuse for free.
	for fn := range queryAllowlist {
		if !seen[fn] {
			t.Errorf("queryAllowlist has an entry for %q, which no longer queries "+
				"any protected table; remove it", fn)
		}
	}

	for _, o := range offenders {
		t.Errorf(
			"%s: %s() queries protected table %q but is not in queryAllowlist.\n"+
				"Private channels are enforced by policy, not keys: a path that reads "+
				"channel or message rows without the ACL exposes a private channel's "+
				"entire history, silently and retroactively. Route the read through "+
				"authorizeChannel (or, for a set-based read, embed "+
				"privateVisibleToCaller / channelPrivacyCols), then add an entry to "+
				"queryAllowlist saying why it is safe, plus a test. "+
				"See docs/rt-private-channel-acl.md §5 and §8.3.",
			o.file, o.fn, o.tables)
	}
}
