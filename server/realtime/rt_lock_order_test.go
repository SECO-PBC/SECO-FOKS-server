package realtime

// Lock-order guard.
//
// Every transaction in this package takes its Postgres row locks in one order.
// Two transactions that disagree can each hold what the other is waiting for,
// and Postgres resolves that by killing one with a deadlock error -- which is
// not the RTRaceError the retry loops are built to absorb, so it surfaces as a
// failed RPC under exactly the concurrency that makes it hard to reproduce.
//
// The rule used to live only in comments, and was broken three times in one
// change: a channel rename took channel_sets before user_inbox while channel
// creation did the reverse; archive took push_outbox before user_inbox while a
// send did the reverse; and a revoke took user_channels before user_inbox while
// everything else did the reverse. None of the three was visible in the
// function that contained it -- each was an ordering between two helper calls.
// Hence this test, which expands calls rather than reading one function at a
// time.
//
// WHAT IT CHECKS. For every function that opens a transaction (one that calls
// RetryTx or RetryTx2), it walks the call graph within this package, collects
// the protected tables that get ROW-LOCKED in source order, and asserts that
// order is non-decreasing in lockRank. A table already acquired earlier in the
// same transaction is free to re-appear -- the lock is already held -- so only
// the first acquisition of each table counts.
//
// TWO PARSER LESSONS, both found by this test covering nothing while passing.
// Comments are stripped before anything else: prose here names functions
// freely ("fanned in lazily by reconcileUserChannels (fanin.go)"), and reading
// that as a call spliced a whole unrelated lock sequence into its neighbour's,
// inventing a violation. And a call is resolved through its receiver's type,
// not its bare name: three types declare `run`, so a bare-name lookup gave up
// on all of them and MakeChannel, SendMessage and MarkReadThrough expanded to
// nothing at all -- the three transactions this guard most needed to cover.
// mustCover now fails if any of them stops producing a sequence.
//
// WHAT IT DOES NOT CHECK. It reads source, not execution: a lock taken in a
// branch that never runs still counts, a lock taken through a function value
// or an interface is invisible, and it cannot see ordering that depends on
// runtime values. It also cannot judge a lock taken in a callee that is itself
// called from several transactions with different state -- the send path's
// prune is the live example, and it is exempted below with its reasoning.
// Composition that only goes wrong at runtime is the concurrency test's job,
// not this one's.

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// lockRank is the canonical order. A transaction may skip ranks but must never
// go backwards.
//
// The order is not arbitrary: it runs from the most specific object a
// transaction touches to the most shared. `channels` is first because almost
// every write authorizes against a channel row and many take it FOR UPDATE, so
// holding it first is what serializes writers to the same channel against each
// other. `channel_sets` is last because it is per-TEAM: taking it early would
// make one channel's mutation block every other channel's in the team.
var lockRank = map[string]int{
	"channels":        0,
	"channel_acl":     1,
	"channel_parties": 2,
	"messages_enc":    3,
	"messages_clear":  3,
	"user_inbox":      4,
	"user_channels":   5,
	"push_outbox":     6,
	"channel_sets":    7,
}

// lockOrderExceptions names transactions whose expanded order is legitimately
// not ascending, with the reason. An entry here is a claim that a human
// checked it, so keep them few and keep them explained.
var lockOrderExceptions = map[string]string{
	// pruneStaleChannelMembers deletes the delivery rows of members who have
	// left the team, and it runs early in the send -- before the fan-out that
	// takes user_inbox. So a send touches user_channels, then user_inbox, then
	// user_channels again. It is bounded in a way the rank cannot express: the
	// rows it touches belong to users who are NO LONGER team members, so no
	// concurrent transaction of theirs can be holding the user_inbox row this
	// one would go on to want. Revisit if the prune ever widens beyond
	// ex-members.
	"SendMessage": "send-time prune deletes ex-members' user_channels rows before the fan-out takes user_inbox; those users have left the team, so nothing of theirs races this",
}

var (
	// SQL in this package is written as backtick literals, so each one can be
	// examined as a whole statement -- which matters because FOR UPDATE often
	// sits several lines below its FROM.
	lockSQLLiteral = regexp.MustCompile("(?s)`([^`]*)`")

	// Statements that take a row lock. A plain SELECT does not: these
	// transactions run at READ COMMITTED, where an unqualified read takes no
	// row lock and so cannot participate in a deadlock cycle.
	lockWrites = []*regexp.Regexp{
		regexp.MustCompile(`(?is)\binsert\s+into\s+([a-z_]+)`),
		regexp.MustCompile(`(?is)\bdelete\s+from\s+([a-z_]+)`),
		// Excludes `DO UPDATE SET`, which is the ON CONFLICT clause of an
		// INSERT already counted above, not a second statement.
		regexp.MustCompile(`(?is)(?:^|[^o][^ ]\s|\n\s*)\bupdate\s+([a-z_]+)\s`),
	}
	lockSelectFor = regexp.MustCompile(`(?is)\bfrom\s+([a-z_]+)`)

	// Group 1 is the receiver VARIABLE, group 2 its type, group 3 the method
	// name. The variable matters: inside a method, `c.checkPerms(m)` is only
	// resolvable if `c` is known to be a channelMaker.
	lockFuncDecl = regexp.MustCompile(`^func\s+(?:\(\s*(\w+)\s+\*?([A-Za-z0-9_]+)\s*\)\s*)?([A-Za-z0-9_]+)\s*\(`)
	// A call to something in this package. Group 1 is the receiver variable
	// when there is one (`mk` in `mk.run(m)`), group 2 the name. Over-matches
	// stdlib and other types, which is safe -- a name that is not a function
	// here resolves to nothing.
	lockCallSite = regexp.MustCompile(`(?:^|[^\w.])(?:([a-zA-Z_][\w]*)\.)?([a-zA-Z_][\w]*)\s*\(`)
	// `mk := channelMaker{` / `s := &messageSender{` -- how every receiver in
	// this package is built, and what lets an ambiguous method name resolve.
	// The type is [a-z] as often as [A-Z] here -- channelMaker, messageSender
	// and readThroughMarker are all unexported, and requiring a capital made
	// this match nothing at exactly the call sites that needed it.
	lockVarDecl = regexp.MustCompile(`(?m)^\s*([a-zA-Z_][\w]*)\s*:?=\s*&?([a-zA-Z_][\w]*)\{`)
)

// stripComments blanks out // and /* */ comments, preserving byte offsets so
// positions stay comparable, and leaving backtick strings alone (SQL is
// written in them and can contain anything).
func stripComments(src string) string {
	out := []byte(src)
	inTick, inLine, inBlock := false, false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
			} else {
				out[i] = ' '
			}
		case inBlock:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				inBlock = false
			} else if c != '\n' {
				out[i] = ' '
			}
		case inTick:
			if c == '`' {
				inTick = false
			}
		case c == '`':
			inTick = true
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			out[i], out[i+1] = ' ', ' '
			i++
			inLine = true
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			i++
			inBlock = true
		}
	}
	return string(out)
}

// lockFn is one function's own locks and the functions it calls.
type lockFn struct {
	name string
	// steps preserves the interleaving of locks and calls: each entry is
	// either a table ("t:x") or a call ("c:x"), so an expansion walks them in
	// the order they actually appear. Keeping the two in one list is the whole
	// trick -- a lock and a call are only comparable by position.
	steps []string
}

func parseLockFns(t *testing.T) map[string]*lockFn {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	// Two passes: collect every declared name first, so call resolution can
	// tell a package function from a stdlib call.
	fns := map[string]*lockFn{}
	byBare := map[string]string{} // bare method name -> qualified, when unambiguous
	ambiguous := map[string]bool{}

	type pending struct {
		name    string
		recvVar string
		recvTyp string
		body    []string
	}
	var all []pending

	for _, e := range entries {
		nm := e.Name()
		if e.IsDir() || !strings.HasSuffix(nm, ".go") || strings.HasSuffix(nm, "_test.go") {
			continue
		}
		body, err := os.ReadFile(nm)
		require.NoError(t, err)
		cur, curVar, curTyp := "", "", ""
		var lines []string
		flush := func() {
			if cur != "" {
				all = append(all, pending{cur, curVar, curTyp, lines})
			}
			lines = nil
		}
		for _, ln := range strings.Split(string(body), "\n") {
			if m := lockFuncDecl.FindStringSubmatch(ln); m != nil {
				flush()
				curVar, curTyp = m[1], m[2]
				if m[2] != "" {
					cur = m[2] + "." + m[3]
				} else {
					cur = m[3]
				}
				bare := m[3]
				if prev, ok := byBare[bare]; ok && prev != cur {
					ambiguous[bare] = true
				}
				byBare[bare] = cur
			}
			lines = append(lines, ln)
		}
		flush()
	}
	for b := range ambiguous {
		delete(byBare, b)
	}

	for _, p := range all {
		fn := &lockFn{name: p.name}
		// Comments stripped FIRST. Prose in this package names functions
		// freely -- "fanned in lazily by reconcileUserChannels (fanin.go)"
		// reads as a call to reconcileUserChannels, and spliced that
		// function's whole lock sequence into the middle of its neighbour's.
		// That is not a hypothetical: it put user_inbox ahead of channel_acl
		// in MakeChannel and produced a violation that does not exist.
		text := stripComments(strings.Join(p.body, "\n"))

		// Locks, positioned by where their SQL literal starts.
		type at struct {
			pos int
			val string
		}
		// FOR UPDATE is matched per FUNCTION, not per literal, because a query
		// is sometimes assembled from several: authorizeChannel appends
		// ` FOR UPDATE` to a string whose `FROM channels` lives in a different
		// literal, and reading literals in isolation missed the channels lock
		// that every write in this package takes first. Attributing it to
		// every FROM in the function over-counts when a function mixes a
		// locking and a non-locking read -- which is the safe direction for
		// this guard, since a lock it invents can only make the order look
		// stricter, never looser.
		forUpdate := regexp.MustCompile(`(?is)\bfor\s+update\b`).MatchString(text)

		var marks []at
		for _, loc := range lockSQLLiteral.FindAllStringSubmatchIndex(text, -1) {
			q := text[loc[2]:loc[3]]
			var tbls []string
			for _, re := range lockWrites {
				for _, mm := range re.FindAllStringSubmatch(q, -1) {
					tbls = append(tbls, strings.ToLower(mm[1]))
				}
			}
			if forUpdate {
				for _, mm := range lockSelectFor.FindAllStringSubmatch(q, -1) {
					tbls = append(tbls, strings.ToLower(mm[1]))
				}
			}
			for _, tbl := range tbls {
				if _, ok := lockRank[tbl]; ok {
					marks = append(marks, at{loc[0], "t:" + tbl})
				}
			}
		}
		// Calls, positioned likewise. Skip anything inside a SQL literal.
		inSQL := func(pos int) bool {
			for _, loc := range lockSQLLiteral.FindAllStringIndex(text, -1) {
				if pos >= loc[0] && pos < loc[1] {
					return true
				}
			}
			return false
		}
		// Receiver variable -> concrete type, for this function only. Scoped
		// per function because that is where these are built, and it is what
		// makes `mk.run(m)` resolvable when three types have a `run`.
		varType := map[string]string{}
		// The method's own receiver first, so `c.commit(m)` inside
		// channelMaker.run resolves to channelMaker.commit.
		if p.recvVar != "" && p.recvTyp != "" {
			varType[p.recvVar] = p.recvTyp
		}
		for _, mm := range lockVarDecl.FindAllStringSubmatch(text, -1) {
			varType[mm[1]] = mm[2]
		}
		// The declaration line names the function itself; reading it as a call
		// made every method look like it called something with its own bare
		// name, which for `run` was ambiguous and poisoned the expansion.
		declEnd := len(p.body[0]) + 1
		for _, loc := range lockCallSite.FindAllStringSubmatchIndex(text, -1) {
			if inSQL(loc[0]) || loc[0] < declEnd {
				continue
			}
			recv := ""
			if loc[2] >= 0 {
				recv = text[loc[2]:loc[3]]
			}
			nm := text[loc[4]:loc[5]]
			if nm == p.name {
				continue
			}
			// Prefer the receiver's type: `mk.run` is channelMaker.run, not
			// whichever `run` happened to be seen last.
			if typ, ok := varType[recv]; ok {
				marks = append(marks, at{loc[0], "c:" + typ + "." + nm})
				continue
			}
			marks = append(marks, at{loc[0], "c:" + nm})
		}
		sort.SliceStable(marks, func(i, j int) bool { return marks[i].pos < marks[j].pos })
		for _, m := range marks {
			fn.steps = append(fn.steps, m.val)
		}
		fns[p.name] = fn
	}

	// Resolve bare call names to qualified ones where unambiguous. A name
	// that is ambiguous AND was not resolved by its receiver above stays
	// unresolved; TestRealtimeLockOrder fails on a transaction that contains
	// one rather than quietly skipping the callee's locks, which is how this
	// guard silently covered nothing for MakeChannel, SendMessage and
	// MarkReadThrough in its first version.
	for _, fn := range fns {
		for i, s := range fn.steps {
			if !strings.HasPrefix(s, "c:") {
				continue
			}
			nm := strings.TrimPrefix(s, "c:")
			if _, ok := fns[nm]; ok {
				continue
			}
			if q, ok := byBare[nm]; ok {
				fn.steps[i] = "c:" + q
				continue
			}
			if ambiguous[nm] {
				fn.steps[i] = "?:" + nm
			}
		}
	}
	return fns
}

// expand returns the tables a transaction rooted at fn locks, in order, each
// counted at its first acquisition.
func expand(fns map[string]*lockFn, name string, seen map[string]bool, out *[]string, has map[string]bool, unresolved *[]string) {
	fn, ok := fns[name]
	if !ok || seen[name] {
		return
	}
	seen[name] = true
	defer delete(seen, name)
	for _, s := range fn.steps {
		switch {
		case strings.HasPrefix(s, "t:"):
			tbl := strings.TrimPrefix(s, "t:")
			if !has[tbl] {
				has[tbl] = true
				*out = append(*out, tbl)
			}
		case strings.HasPrefix(s, "c:"):
			expand(fns, strings.TrimPrefix(s, "c:"), seen, out, has, unresolved)
		case strings.HasPrefix(s, "?:"):
			// An ambiguous method call whose receiver could not be typed. Its
			// locks are invisible, so the sequence below it is a guess.
			*unresolved = append(*unresolved, name+" -> "+strings.TrimPrefix(s, "?:"))
		}
	}
}

func TestRealtimeLockOrder(t *testing.T) {
	fns := parseLockFns(t)

	// Transaction roots: a function that opens one. Everything a transaction
	// locks is reached from here, so this is where an order exists to check.
	var roots []string
	for name, fn := range fns {
		for _, s := range fn.steps {
			if s == "c:RetryTx" || s == "c:RetryTx2" {
				roots = append(roots, name)
				break
			}
		}
	}
	sort.Strings(roots)
	require.NotEmpty(t, roots, "found no transactions to check -- the parser is broken, not the code")

	// Transactions that MUST produce a sequence. The first version of this
	// test resolved `run` to nothing, because three types declare one, so
	// MakeChannel, SendMessage and MarkReadThrough expanded to an empty list
	// and were silently checked against nothing -- while the test passed,
	// because other roots still had sequences. A guard that can cover nothing
	// and look green is worse than no guard.
	mustCover := []string{
		"MakeChannel", "SendMessage", "MarkReadThrough",
		"UpdateChannel", "SetChannelArchived",
		"GrantChannelMember", "RevokeChannelMember",
	}

	seqs := map[string][]string{}
	var checked int
	for _, root := range roots {
		var seq []string
		var unresolved []string
		expand(fns, root, map[string]bool{}, &seq, map[string]bool{}, &unresolved)
		seqs[root] = seq
		require.Emptyf(t, unresolved,
			"%s(): could not resolve %v, so the locks below those calls are "+
				"invisible and this transaction's order is unchecked. Give the "+
				"receiver a concrete type at the call site, or rename the method.",
			root, unresolved)
		if len(seq) < 2 {
			continue
		}
		checked++
		if why, ok := lockOrderExceptions[root]; ok {
			t.Logf("exception: %s %v -- %s", root, seq, why)
			continue
		}
		for i := 1; i < len(seq); i++ {
			require.LessOrEqualf(t, lockRank[seq[i-1]], lockRank[seq[i]],
				"%s() takes row locks out of order: %v\n\n"+
					"%q (rank %d) is locked after %q (rank %d). Every transaction in "+
					"this package must take its locks in lockRank order, or two of them "+
					"can hold what the other is waiting for and Postgres kills one with a "+
					"deadlock -- which the RTRaceError retry loops do not absorb.\n\n"+
					"Reorder the writes. If the order is genuinely safe, add %q to "+
					"lockOrderExceptions with the reason it cannot deadlock.",
				root, seq, seq[i], lockRank[seq[i]], seq[i-1], lockRank[seq[i-1]], root)
		}
	}
	for _, name := range mustCover {
		require.Containsf(t, roots, name,
			"%q no longer opens a transaction. If it was renamed, update mustCover; "+
				"if it stopped being a transaction, say so here.", name)
		require.GreaterOrEqualf(t, len(seqs[name]), 2,
			"%s() expanded to %v -- fewer than two locks, so nothing about its order "+
				"was checked. This is what a broken parser looks like from the inside, "+
				"and it is exactly how this test covered nothing for three transactions "+
				"while passing. Fix call resolution rather than removing the entry.",
			name, seqs[name])
	}
	require.GreaterOrEqual(t, checked, len(mustCover),
		"only %d transactions had a checkable lock sequence", checked)

	// A stale exception makes the rule look more contested than it is.
	for name := range lockOrderExceptions {
		require.Containsf(t, roots, name,
			"lockOrderExceptions names %q, which no longer opens a transaction; remove it", name)
	}
}
