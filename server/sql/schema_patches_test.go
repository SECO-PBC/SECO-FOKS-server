// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package sql

import (
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// A base schema marks patches as applied with INSERTs into schema_patches,
// and both shapes appear in this tree: one row per statement
// (`VALUES (5, NOW());`, as foks_realtime does) and several tuples in one
// statement (`VALUES (1, NOW()), (2, NOW()), (3, NOW());`, as
// foks_server_config does). So the statement is matched up to its `;`, and
// every tuple inside it is read -- matching only the first tuple after
// VALUES reports the rest as missing when they are not.
var (
	recordStmtRe  = regexp.MustCompile(`(?is)INSERT\s+INTO\s+schema_patches\b.*?VALUES(.*?);`)
	recordTupleRe = regexp.MustCompile(`\(\s*(\d+)\s*,`)
)

// TestBaseSchemaRecordsEveryPatch pins the rule that a base schema carries
// every patch already, and says so.
//
// A base schema is the CURRENT shape of its database: when a patch adds a
// column, the column is also declared in the CREATE TABLE, so a new database
// is born with it. That database must then record the patch as applied.
// If it does not, patch-db sees a lower highest-patch, re-applies the
// patch, and the ALTER fails on the column the base schema already made --
// `column "no_push" of relation "channels" already exists`. The deploy
// script treats a patch-db failure as fatal, so a fresh database cannot be
// brought up at all.
//
// It is an easy row to forget, because nothing else notices: the
// integration harness builds from the base schema and never runs patch-db,
// and a migrated database already has the row from when the patch ran. It
// was missed three times in a row (realtime p5 and p6 here, and p5 on the
// upstream copy of no-push channels) before this test existed.
func TestBaseSchemaRecordsEveryPatch(t *testing.T) {
	for db, patches := range Patches {
		base, ok := SQL[db]
		if !ok {
			t.Errorf("%s: has patches but no base schema in SQL", db)
			continue
		}

		recorded := map[int]bool{}
		for _, stmt := range recordStmtRe.FindAllStringSubmatch(base, -1) {
			for _, m := range recordTupleRe.FindAllStringSubmatch(stmt[1], -1) {
				n, err := strconv.Atoi(m[1])
				if err != nil {
					t.Fatalf("%s: unparseable patch id %q", db, m[1])
				}
				recorded[n] = true
			}
		}

		var missing, stray []int
		for n := range patches {
			if !recorded[n] {
				missing = append(missing, n)
			}
		}
		for n := range recorded {
			if _, ok := patches[n]; !ok {
				stray = append(stray, n)
			}
		}
		sort.Ints(missing)
		sort.Ints(stray)

		if len(missing) > 0 {
			t.Errorf("%s: base schema does not record patch(es) %v as applied. "+
				"A new database would re-run them and fail on objects the base "+
				"schema already created. Add `INSERT INTO schema_patches (id, ctime) "+
				"VALUES (N, NOW());` at the bottom of the base schema for each.",
				db, missing)
		}
		if len(stray) > 0 {
			t.Errorf("%s: base schema records patch(es) %v that have no patch file "+
				"in Patches -- either the patch was not embedded (server/sql/embed.go) "+
				"or the row is left over.", db, stray)
		}
	}
}
