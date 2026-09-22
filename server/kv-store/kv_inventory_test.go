package kvStore

// The "bug in three years" guard (§9.4 of docs/kv-channel-acl.md), modelled on
// server/realtime/rt_inventory_test.go, which guards the same property for the
// message side.
//
// Channel storage is enforced by policy, not by keys. A private channel's
// files are sealed with the TEAM's key at the node's read role, so every
// member of the community at that role already holds the key; what stands
// between a non-member and the plaintext is this package declining to serve
// the bytes. A path that reaches a tagged node without consulting the ACL is
// therefore a silent, retroactive privacy failure: nothing throws, no existing
// test goes red, the data is simply readable by someone who should not see it.
//
// Two guards, both source-level, both deliberately annoying to a future author
// who adds a path without thinking about it:
//
//  1. Every RPC in the KVStore protocol must be classified below, with a
//     pointer to the test that covers it.
//  2. Every query in this package against a table that holds node data or the
//     channel tag must sit in an allowlisted function.
//
// This lives in the server package, not integration-tests/lib, on purpose: CI
// runs ./server/... but not the integration suite, which needs a live
// postgres, and a guard CI does not run is not a guard.

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
	// noNodeData: cannot return node rows, file bytes, or node metadata.
	noNodeData accessClass = "noNodeData"
	// nodeData: returns node rows, key boxes, file bytes or version numbers
	// -- must pass authorizeKVNodeRead for any tagged node it touches.
	nodeData accessClass = "nodeData"
	// nodeWrite: creates, links or mutates a node.
	nodeWrite accessClass = "nodeWrite"
)

type rpcClassification struct {
	class accessClass
	// covers names the test(s) exercising this RPC's channel-storage
	// behaviour, or says why none is needed.
	covers string
}

// rpcAccessClass classifies every method of the KVStore protocol. Adding an
// RPC without adding an entry here fails this test -- which is the point.
var rpcAccessClass = map[string]rpcClassification{
	"kvMkdir": {
		nodeWrite,
		"TestKvChannelCreationRequiresMembership, TestKvChannelTagIsImmutableOnReplay " +
			"(creation tagging, §6 row 8)",
	},
	"kvPut": {
		nodeWrite,
		"TestKvChannelContainment, TestKvChannelHardlinkAcrossChannelsRefused " +
			"(containment at link, §6 row 7)",
	},
	"kvPutRoot": {
		nodeWrite,
		"the party root is never channel storage (§6 row 13); putRoot reaches " +
			"only the root table, which holds no tag",
	},
	"kvFileUploadInit": {
		nodeWrite,
		"TestKvChannelCreationRequiresMembership covers the creation gate; the " +
			"tag is written by insLargeFile (§6 row 10)",
	},
	"kvFileUploadChunk": {
		nodeWrite,
		"assertUploading carries the ACL check: the RPC names a file by ID with " +
			"no directory in front of it, so a non-member must not be able to " +
			"append to a tagged file mid-upload (§6 row 10)",
	},
	"kvPutSmallFileOrSymlink": {
		nodeWrite,
		"TestKvChannelCreationRequiresMembership, TestKvChannelOrphanIsProtected, " +
			"TestKvChannelTagIsImmutableOnReplay (§6 row 9)",
	},
	"kvGetRoot": {
		nodeData,
		"the party root is never channel storage (§6 row 13)",
	},
	"kvGet": {
		nodeData,
		"TestKvChannelAclReadGate probes it raw with a real name MAC, because " +
			"the client API renders denial and absence identically (§6 row 16)",
	},
	"kvGetNode": {
		nodeData,
		"TestKvChannelAclReadGate, for both a small file and a large file " +
			"(§6 rows 1, 4, 5)",
	},
	"kvGetEncryptedChunk": {
		nodeData,
		"TestKvChannelAclReadGate; gated in getChunk via loadLargeFileReadRole, " +
			"since the RPC carries a file ID and no directory context (§6 row 6)",
	},
	"kvGetDir": {
		nodeData,
		"TestKvChannelAclReadGate (§6 row 1)",
	},
	"kvCacheCheck": {
		nodeData,
		"TestKvChannelAclReadGate probes the version vector; gated through " +
			"getCurrentDirVersion (§6 rows 11, 17)",
	},
	"kvList": {
		nodeData,
		"TestKvChannelAclReadGate (§6 row 2)",
	},
	"kvLockAcquire": {
		nodeWrite,
		"lockCheckPerms loads the parent dir through loadDir, which carries the " +
			"ACL check, before taking the lock (§6 row 14)",
	},
	"kvLockRelease": {
		nodeWrite,
		"same as kvLockAcquire: lockCheckPerms -> loadDir (§6 row 14)",
	},
	"kvUsage": {
		noNodeData,
		"party-wide totals with no per-node breakdown, so it cannot attribute " +
			"bytes to a channel (§6 row 15, Q2)",
	},
	"selectVHost": {
		noNodeData,
		"vhost selection; touches no node data (§6 row 18)",
	},
	"kvChannelMkRoot": {
		nodeWrite,
		"TestKvChannelMkRootAuthorization; gated by authorizeKVNodeManage",
	},
}

// protectedTables hold node data, file bytes, or the channel tag itself. The
// two realtime tables are here because acl.go reads them across databases to
// answer membership, and a second path doing that on its own -- rather than
// through the chokepoint -- is exactly as dangerous as an ungated node read.
var protectedTables = []string{
	"dir",
	"dirent",
	"large_file",
	"large_file_key",
	"large_file_chunk",
	"small_file_or_symlink",
	"channel_kv_root",
	"channel_acl",
	"channels",
}

// allowedQueries is one signed-off function: how many references to protected
// tables its body is expected to contain, and why reaching that data there is
// safe.
//
// The count matters as much as the name. Keying only on the name would catch a
// protected query landing in a NEW function while missing a second one slipped
// into an already-allowlisted one -- and those are precisely where a careless
// path is most likely to be added, since they are already full of this SQL.
type allowedQueries struct {
	n   int
	why string
}

// queryAllowlist maps a function that legitimately queries a protected table
// to its expected reference count and why that is safe.
//
// The rule being enforced: no path may reach a tagged node on behalf of a
// caller without passing through authorizeKVNodeRead (or, for the management
// RPC, authorizeKVNodeManage), and no path may write a tag without
// authorizeKVNodeCreate.
var queryAllowlist = map[string]allowedQueries{
	// --- the chokepoint and its helpers (acl.go) ---
	"kvChannelMembershipUncached": {2, "the membership probe itself: channel_acl joined to channels, scoped to the team owning this store. kvChannelMembership wraps it with the per-request memo and makes no query of its own"},
	"authorizeKVNodeManage":       {2, "the management gate: ACL owner or team admin, channel private and of this team"},
	"loadNodeChannelTag":          {3, "reads the tag of an existing node of any kind, for containment; returns no node contents"},
	"assertIsChannelRoot":         {1, "the single legal crossing: is this exact dir the channel's registered root"},
	"registerChannelRoot":         {2, "writes channel_kv_root; reached only from KvChannelMkRoot after authorizeKVNodeManage"},

	// --- the five read loaders, each of which IS a chokepoint ---
	"loadDir":                   {1, "chokepoint: calls authorizeKVNodeRead on the row it just loaded, before its callers' role gates"},
	"loadDirent":                {2, "chokepoint: joins dirent to its dir and authorizes on the dir's tag; a denial is this loader's nil row"},
	"mLoadSmallFilesOrSymlinks": {1, "chokepoint: authorizes per row, and a denied row is simply left out of the result"},
	"loadLargeFileMetadata":     {2, "chokepoint: authorizes before the role gate and before decoding the key box"},
	"loadLargeFileReadRole":     {2, "chokepoint for getChunk, which has a file ID and no directory context"},
	"getCurrentDirVersion":      {1, "chokepoint: version numbers are a probing side channel (§6 row 11)"},
	"getCurrentDirentVersion":   {1, "no check of its own by design (§6 row 12): reached only from checkVersionVector, which calls getCurrentDirVersion on the enclosing dir first, so it inherits that gate"},

	// --- reads that run after a chokepoint authorized the caller ---
	"BlobSQLStorage.Get": {1, "serves the ciphertext, and has no check of its own: reached only from getChunk, which calls loadLargeFileReadRole (ACL + read role) before it. If a second caller is ever added it must gate first -- that is what this entry is for"},
	"listDir":            {1, "selects the dirent rows of a dir that loadDir already authorized on the line above"},

	// --- writes ---
	"putDir":                        {2, "creation; the tag comes from authorizeKVNodeCreate in the handler, and the replay comparison weighs it so a resend cannot relabel"},
	"putSmallFileOrSymlink":         {2, "creation; same shape as putDir, tag included in the replay comparison"},
	"fileUploader.insLargeFile":     {1, "creation; writes the tag authorizeKVNodeCreate returned"},
	"fileUploader.insKey":           {1, "creation; the key row carries no tag, and rides insLargeFile's transaction"},
	"fileUploader.insChunk":         {1, "upload; reached from doChunk after assertUploading, which carries the ACL check"},
	"fileUploader.checkChunks":      {1, "upload; same transaction and same gate as insChunk"},
	"fileUploader.finalize":         {1, "upload; same transaction and same gate as insChunk"},
	"fileUploader.assertUploading":  {1, "upload gate: the only place the chunk path loads the file's row, so the ACL check lives here"},
	"direntUpdater.insertNewDirent": {2, "link; runs after checkPermissions and checkContainment"},
	"fileRef":                       {1, "refcount bookkeeping on link/unlink; reached from updateRefcounts, after containment, and returns nothing to a caller"},
	"smallFileRef":                  {1, "refcount bookkeeping; same as fileRef"},
	"ClientConn.KvChannelMkRoot":    {1, "reads the candidate dir's tag to confirm a member created it; runs after authorizeKVNodeManage"},
}

// rpcLine matches a method declaration in a snowp protocol block.
var rpcLine = regexp.MustCompile(`^\s*([a-z][A-Za-z0-9]*)\s+@(\d+)\s*[(:]`)

// TestKvStoreRpcInventory fails when a KVStore RPC is added without being
// classified against the path inventory (§6 of the spec).
func TestKvStoreRpcInventory(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "proto-src", "rem", "kv.snowp"))
	require.NoError(t, err)

	var found []string
	inProtocol := false
	depth := 0
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "protocol KVStore") {
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
		// shape one level in is an argument (`dir @1 : ...`).
		if depth == 0 {
			if mm := rpcLine.FindStringSubmatch(line); mm != nil {
				found = append(found, mm[1])
			}
		}
		depth += strings.Count(line, "(") - strings.Count(line, ")")
	}

	// Sanity: if the parse silently stops matching, every assertion below
	// passes vacuously and the guard is worthless.
	require.GreaterOrEqual(t, len(found), 15,
		"parsed too few RPCs out of kv.snowp -- the parser has drifted from the "+
			"syntax, which would make this guard silently vacuous")

	for _, nm := range found {
		cls, ok := rpcAccessClass[nm]
		require.True(t, ok,
			"RPC %q is not classified in rpcAccessClass. Every KVStore RPC must be "+
				"classified as noNodeData / nodeData / nodeWrite, and -- if it can "+
				"reach a tagged node -- gated by authorizeKVNodeRead (or "+
				"authorizeKVNodeCreate / authorizeKVNodeManage for writes) and covered "+
				"by a test. See docs/kv-channel-acl.md §6.", nm)
		require.NotEmpty(t, cls.covers, "RPC %q is classified but names no covering test", nm)
	}

	// The reverse: a classification left behind by a removed RPC is dead
	// weight that makes the map look more complete than it is.
	sort.Strings(found)
	for nm := range rpcAccessClass {
		idx := sort.SearchStrings(found, nm)
		require.True(t, idx < len(found) && found[idx] == nm,
			"rpcAccessClass classifies %q, which is not an RPC in kv.snowp", nm)
	}
}

var (
	// Captures the receiver type (group 1, may be empty) and the function
	// name (group 2). The receiver matters: keying on a bare method name
	// would let an entry justified for one type silently cover every
	// same-named method in the package.
	funcDecl = regexp.MustCompile(`^func\s+(?:\(\s*\w+\s+\*?([A-Za-z0-9_]+)\s*\)\s*)?([A-Za-z0-9_]+)\s*\(`)
	// Top-level const/var/type: shared SQL fragments live in consts and must
	// be attributed to themselves, not to whichever function precedes them.
	declLine = regexp.MustCompile(`^(?:const|var|type)\s+([A-Za-z0-9_]+)`)
	// \s+ (not [ \t]+) so a table name on the line after its keyword still
	// matches; the scan runs over a whole declaration's text, not one line.
	fromLine = regexp.MustCompile(`(?i)\b(?:FROM|INTO|UPDATE|JOIN)\s+([a-z_]+)\b`)
)

// TestKvStoreProtectedTableQueries fails when a query against a table holding
// node data or the channel tag appears in a function not signed off as safe.
//
// It is deliberately dumb: it does not understand SQL, it understands "this
// function names a protected table". A new query in a new function fails until
// its author says, in queryAllowlist, why reaching that data there is safe.
func TestKvStoreProtectedTableQueries(t *testing.T) {
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
		// lines, and a FROM whose table name sits on the following line would
		// be invisible to a per-line match -- a silent hole in exactly the
		// guard that is supposed to have none.
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
				// Keep the declaration line's own text: a single-line const
				// carries its whole query there, so dropping it would hide
				// that SQL from the guard entirely.
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

	require.Greater(t, sawAny, 20,
		"found almost no queries against the protected tables -- the scanner has "+
			"drifted and this guard is silently vacuous")

	for _, c := range counts {
		t.Errorf(
			"%s: %s() now makes %d references to protected tables (%s), but "+
				"queryAllowlist records %d.\n"+
				"A query was added to a function that was already signed off -- exactly "+
				"where an ungated read is easiest to miss, because the function is "+
				"already full of this SQL. Confirm the new one reaches a tagged node "+
				"only after authorizeKVNodeRead (or authorizeKVNodeCreate / "+
				"authorizeKVNodeManage for a write), then update the count and the "+
				"justification, and add a test. See docs/kv-channel-acl.md §6 and §9.4.",
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
				"Channel storage is enforced by policy, not keys: a path that reads a "+
				"tagged node without the ACL exposes a private channel's whole storage, "+
				"silently and retroactively, to anyone in the community. Route it "+
				"through authorizeKVNodeRead, then add an entry saying why it is safe, "+
				"plus a test. See docs/kv-channel-acl.md §6 and §9.4.",
			o.file, o.fn, o.tables)
	}
}
