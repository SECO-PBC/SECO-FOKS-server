// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// External test package on purpose: it lets the test read the base schemas
// through shared.SplitSQLStatements — the same comment-stripping statement
// splitter init-db and patch-db execute them with — instead of a second,
// approximate parser. (In-package it would cycle: shared imports sql.)
package sql_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/foks-proj/go-foks/server/shared"
	sqlpkg "github.com/foks-proj/go-foks/server/sql"
)

var (
	insertRe = regexp.MustCompile(`(?i)^\s*INSERT\s+INTO\s+schema_patches\b`)
	valuesRe = regexp.MustCompile(`(?i)\bVALUES\b`)
	// A tuple's opening paren must follow VALUES or the close of the previous
	// tuple, so a digit argument inside a function call — e.g.
	// make_timestamp(2025, …) — is never read as a patch id. Both shapes in
	// this tree parse: one row per statement (`VALUES (5, NOW())`) and several
	// tuples in one statement (`VALUES (1, NOW()), (2, NOW())`). The id is
	// capped at 9 digits so Atoi cannot overflow.
	tupleRe = regexp.MustCompile(`(?i)(?:\bVALUES|\))\s*,?\s*\(\s*(\d{1,9})\s*,`)

	patchFileRe = regexp.MustCompile(`^p(\d{1,9})\.sql$`)
)

// recordedPatchIDs reads the patch ids a base schema marks as applied,
// statement by statement as the executor sees them: comments are stripped
// first, so a commented-out INSERT counts for neither side.
func recordedPatchIDs(t *testing.T, db, base string) map[int]bool {
	recorded := map[int]bool{}
	for _, stmt := range shared.SplitSQLStatements(base) {
		if !insertRe.MatchString(stmt) {
			continue
		}
		if !valuesRe.MatchString(stmt) {
			t.Errorf("%s: an INSERT INTO schema_patches without a literal VALUES "+
				"clause is not a form this test can read; use "+
				"`INSERT INTO schema_patches (id, ctime) VALUES (N, NOW());`", db)
			continue
		}
		tuples := tupleRe.FindAllStringSubmatch(stmt, -1)
		if len(tuples) == 0 {
			t.Errorf("%s: an INSERT INTO schema_patches whose VALUES yields no "+
				"(id, …) tuples — rewrite it in the plain literal form", db)
			continue
		}
		for _, m := range tuples {
			n, _ := strconv.Atoi(m[1]) // \d{1,9}: cannot fail
			recorded[n] = true
		}
	}
	return recorded
}

// TestBaseSchemaRecordsEveryPatch pins one direction of the base-schema rule:
// a base schema that carries a patch's DDL must also record the patch as
// applied.
//
// A base schema is the CURRENT shape of its database: when a patch adds a
// column, the column is also declared in the CREATE TABLE, so a new database
// is born with it and must record the patch as applied. patch-db keeps a SET
// of applied ids (SELECT id FROM schema_patches; server/shared/patch.go) and
// re-runs every registered patch whose id is absent — there is no
// highest-id watermark, each missing row re-runs exactly that patch. On a
// fresh database the re-run fails on the object the base schema already
// created, and the deploy script treats that as fatal.
//
// The row is easy to forget because nothing else notices: the integration
// harness builds from the base schema and never runs patch-db, and a
// migrated database already has the row from when the patch ran. It was
// missed three times (realtime p5 and p6 here, and p5 on the upstream copy
// of no-push channels) before this test existed.
//
// Two limits, both deliberate:
//   - The rows stay hand-written rather than derived at init time
//     (runMakeTablesOne could stamp every id in Patches), because derivation
//     is only sound if every registered patch is guaranteed folded into the
//     base schema. A patch that is embedded but not yet folded is legal —
//     patch-db catches a fresh database up — and auto-stamping it would mark
//     it applied while its DDL never ran.
//   - For the same reason this test checks only the RECORD, not the folding:
//     it cannot see whether the base CREATE TABLEs actually contain what
//     pN.sql adds. That half of the rule (SKILL: add-sql-patch,
//     "Consistency rule") remains a review obligation.
func TestBaseSchemaRecordsEveryPatch(t *testing.T) {
	for db := range sqlpkg.Patches {
		if _, ok := sqlpkg.SQL[db]; !ok {
			t.Errorf("%s: has patches but no base schema in SQL", db)
		}
	}

	// Every base schema is scanned — patch-less ones too, so a stray record
	// row cannot hide in a database that has no patches yet.
	for db, base := range sqlpkg.SQL {
		patches := sqlpkg.Patches[db] // nil for a patch-less schema: ranges empty
		recorded := recordedPatchIDs(t, db, base)

		var missing, stray []int
		for _, n := range slices.Sorted(maps.Keys(patches)) {
			if !recorded[n] {
				missing = append(missing, n)
			}
		}
		for _, n := range slices.Sorted(maps.Keys(recorded)) {
			if _, ok := patches[n]; !ok {
				stray = append(stray, n)
			}
		}

		if len(missing) > 0 {
			t.Errorf("%s: base schema does not record patch(es) %v as applied — "+
				"a fresh database would re-run them and fail on objects the base "+
				"schema already created. Add `INSERT INTO schema_patches (id, ctime) "+
				"VALUES (N, NOW());` at the bottom of the base schema AND confirm "+
				"the base CREATE TABLEs already contain everything pN.sql adds; "+
				"recording a patch that is not folded in leaves every fresh "+
				"database without it, silently.",
				db, missing)
		}
		if len(stray) > 0 {
			t.Errorf("%s: base schema records patch(es) %v that are not registered "+
				"in Patches (server/sql/embed.go) — either the patch was never "+
				"embedded or the row is left over.", db, stray)
		}
	}

	// Disk ↔ map: every patches/<db>/pN.sql must be registered in Patches and
	// vice versa. embed.go is the step that gets forgotten — a patch file on
	// disk that never makes the map is invisible to every deployment.
	dirs, err := os.ReadDir("patches")
	if err != nil {
		t.Fatalf("reading patches/: %v", err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		db := d.Name()
		files, err := os.ReadDir(filepath.Join("patches", db))
		if err != nil {
			t.Errorf("reading patches/%s: %v", db, err)
			continue
		}
		onDisk := map[int]bool{}
		for _, f := range files {
			if m := patchFileRe.FindStringSubmatch(f.Name()); m != nil {
				n, _ := strconv.Atoi(m[1]) // \d{1,9}: cannot fail
				onDisk[n] = true
			}
		}
		for _, n := range slices.Sorted(maps.Keys(onDisk)) {
			if _, ok := sqlpkg.Patches[db][n]; !ok {
				t.Errorf("patches/%s/p%d.sql exists on disk but is not registered "+
					"in Patches (server/sql/embed.go) — no deployment will ever "+
					"run it.", db, n)
			}
		}
		for _, n := range slices.Sorted(maps.Keys(sqlpkg.Patches[db])) {
			if !onDisk[n] {
				t.Errorf("Patches[%q] registers patch %d but patches/%s/p%d.sql "+
					"does not exist on disk.", db, n, db, n)
			}
		}
	}
	for db := range sqlpkg.Patches {
		if _, err := os.Stat(filepath.Join("patches", db)); err != nil {
			t.Errorf("Patches[%q] is registered but patches/%s/ does not exist: %v",
				db, db, err)
		}
	}
}
