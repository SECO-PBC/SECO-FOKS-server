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

	lockFuncDecl = regexp.MustCompile(`^func\s+(?:\(\s*\w+\s+\*?([A-Za-z0-9_]+)\s*\)\s*)?([A-Za-z0-9_]+)\s*\(`)
	// A call to something in this package: bare `foo(` or `x.foo(`. Over-matches
	// (it catches stdlib and method calls on other types too), which is safe --
	// a name that is not a function in this package simply resolves to nothing.
	lockCallSite = regexp.MustCompile(`(?:^|[^\w.])(?:[a-zA-Z_][\w]*\.)?([a-zA-Z_][\w]*)\s*\(`)
)

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
		name string
		body []string
	}
	var all []pending

	for _, e := range entries {
		nm := e.Name()
		if e.IsDir() || !strings.HasSuffix(nm, ".go") || strings.HasSuffix(nm, "_test.go") {
			continue
		}
		body, err := os.ReadFile(nm)
		require.NoError(t, err)
		cur := ""
		var lines []string
		flush := func() {
			if cur != "" {
				all = append(all, pending{cur, lines})
			}
			lines = nil
		}
		for _, ln := range strings.Split(string(body), "\n") {
			if m := lockFuncDecl.FindStringSubmatch(ln); m != nil {
				flush()
				if m[1] != "" {
					cur = m[1] + "." + m[2]
				} else {
					cur = m[2]
				}
				bare := m[2]
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
		text := strings.Join(p.body, "\n")

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
		for _, loc := range lockCallSite.FindAllStringSubmatchIndex(text, -1) {
			if inSQL(loc[0]) {
				continue
			}
			nm := text[loc[2]:loc[3]]
			if nm == p.name {
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

	// Resolve bare call names to qualified ones where unambiguous.
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
			}
		}
	}
	return fns
}

// expand returns the tables a transaction rooted at fn locks, in order, each
// counted at its first acquisition.
func expand(fns map[string]*lockFn, name string, seen map[string]bool, out *[]string, has map[string]bool) {
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
			expand(fns, strings.TrimPrefix(s, "c:"), seen, out, has)
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

	var checked int
	for _, root := range roots {
		var seq []string
		expand(fns, root, map[string]bool{}, &seq, map[string]bool{})
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
	require.GreaterOrEqual(t, checked, 5,
		"only %d transactions had a checkable lock sequence; the parser has probably "+
			"stopped resolving calls, which would make this test pass by seeing nothing", checked)

	// A stale exception makes the rule look more contested than it is.
	for name := range lockOrderExceptions {
		require.Containsf(t, roots, name,
			"lockOrderExceptions names %q, which no longer opens a transaction; remove it", name)
	}
}
