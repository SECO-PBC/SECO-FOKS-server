package lib

// Every test the channel-mutation spec cites must exist.
//
// docs/rt-channel-mutation.md carries a path inventory and a test plan that
// name the test covering each path. That naming is the only thing telling a
// reader -- or the app team planning against it -- which behaviour is actually
// pinned, and it had drifted twice over: the spec cited eight tests that were
// never written, and named two, TestCannotArchiveGeneral and
// TestUnarchiveRestoresToInbox, that never existed under those names at all.
// A path inventory that lists a test nobody wrote claims coverage that is not
// there, which is worse than an empty cell.
//
// Documentation drift is the one failure mode in this change that review kept
// catching and nothing else did -- five times. This makes the mechanically
// checkable half of it fail the build instead. It cannot tell whether a test
// actually covers what the row claims; it can tell whether the test exists,
// which is where the drift was.
//
// Needs no database: it reads the spec and the package's own test files.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// specPath is the document this guards, relative to this package.
const specPath = "../../docs/rt-channel-mutation.md"

// specTestNamePattern matches a Go test name as the spec writes it: in a table
// cell, a bullet, or backticks.
//
// Any length and underscores allowed, because the first version required at
// least three characters after the capital and no underscore -- so TestFoo and
// TestFoo_Bar, both valid Go test names, could be cited for tests that do not
// exist and this guard would not notice. A guard with a hole exactly where a
// name is short is not much of a guard.
var specTestNamePattern = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*\b`)

// specTestNameExempt lists names that look like tests but are not, or are
// tests that deliberately do not exist yet. An entry here is a claim someone
// checked, so say why.
var specTestNameExempt = map[string]string{
	// Prose, not test names.
	"TestUser":  "the integration harness's user type, named in passing",
	"TestHooks": "the MakeChannelTestHooks struct, named in passing",

	// Named in the inventory but not written, each with a reason. Remove the
	// entry when the test lands, or the row when the plan changes.
	"TestGetChannelReportsArchived": "rtGetChannel is still NotImplementedError upstream and here; " +
		"there is nothing to assert until it is built",
	"TestArchivedPrivateStorageStillReachable": "would pin §6 Q5 (an archived private channel's KV files " +
		"stay reachable), but that store is the kv-store package's and this suite has no fixture for it",
	"TestArchiveDoesNotBurnInboxVersions": "superseded by the invariant suite, which asserts the same " +
		"property after every mutation test rather than in one of them",
}

func TestSpecCitesOnlyRealTests(t *testing.T) {
	raw, err := os.ReadFile(specPath)
	require.NoErrorf(t, err, "cannot read %s; if the spec moved, update specPath", specPath)

	// Every test function declared in this package and in server/realtime.
	declared := map[string]bool{}
	for _, dir := range []string{".", "../../server/realtime"} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			require.NoError(t, err)
			for _, m := range regexp.MustCompile(`func (Test[A-Za-z0-9_]+)\(`).
				FindAllStringSubmatch(string(body), -1) {
				declared[m[1]] = true
			}
		}
	}
	require.NotEmpty(t, declared, "found no test functions at all; the scan is broken")

	var missing []string
	seen := map[string]bool{}
	for _, m := range specTestNamePattern.FindAllString(string(raw), -1) {
		if seen[m] || declared[m] {
			continue
		}
		seen[m] = true
		if _, ok := specTestNameExempt[m]; ok {
			continue
		}
		missing = append(missing, m)
	}
	sort.Strings(missing)
	require.Emptyf(t, missing,
		"%s names %d test(s) that do not exist: %v\n\n"+
			"The inventory's test column is what tells a reader which behaviour is "+
			"actually pinned, so a name with no test behind it claims coverage that "+
			"is not there. Either write the test, or move the name into "+
			"specTestNameExempt with the reason it is not written.",
		specPath, len(missing), missing)

	// A stale exemption is the same lie in the other direction: it says a test
	// was considered and skipped when it has since been written.
	for name, why := range specTestNameExempt {
		if declared[name] {
			t.Errorf("specTestNameExempt excuses %q (%q), but that test now exists; remove the entry",
				name, why)
		}
	}
}
