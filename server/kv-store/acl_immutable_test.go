// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestChannelTagIsWriteOnce guards H1 of docs/kv-channel-acl.md: a node's
// channel tag is set once, when the node is created, and no code path ever
// changes it. That is what makes the tag trustworthy -- a member with write
// access must not be able to relabel a node into or out of a channel, and
// the design deletes that whole class of bug by never offering the mutation
// rather than by authorizing it.
//
// Today the property holds because channel_id is written only by the three
// creation INSERTs, and because a replay of any of them compares the tag
// along with every other persisted field instead of keeping whichever landed
// first. There is no rotation path in this package at all: nothing writes a
// second `dir` version, so there is nothing yet that could carry a tag
// forward or drop it.
//
// That last point is exactly why this test exists. When rotation is
// implemented it MUST copy channel_id from the row it supersedes, and take
// it from the previous row rather than from the request. This test fails the
// build the moment a statement writes channel_id outside the allowlist, so
// whoever writes that code is told, rather than discovering it as a silent
// privacy regression years later.
func TestChannelTagIsWriteOnce(t *testing.T) {
	// The only statements permitted to write channel_id: the three creation
	// INSERTs, each of which sets it from the request exactly once, after
	// authorizeKVNodeCreate has proved membership.
	allowed := map[string]int{
		"dir.go":  1, // putDir
		"file.go": 2, // putSmallFileOrSymlink, insLargeFile
	}

	// An UPDATE touching channel_id is never allowed, anywhere. The span
	// between UPDATE and channel_id is bounded by length rather than by a
	// backtick, so SQL assembled from concatenated fragments or written in
	// double quotes is still caught.
	updateRe := regexp.MustCompile(`(?is)UPDATE\s+(?:"?)(dir|large_file|small_file_or_symlink)\b.{0,400}?\bSET\b.{0,400}?\bchannel_id\s*=`)
	insertRe := regexp.MustCompile(`(?is)INSERT\s+INTO\s+(?:"?)(dir|large_file|small_file_or_symlink)\b`)
	// Comments are stripped before any of this runs: the prose in this
	// package discusses these tables and this column constantly, and a
	// comment cannot write a row. Without stripping, the guard cries wolf on
	// the very comments that explain it.
	lineComment := regexp.MustCompile(`(?m)^\s*//.*$`)

	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		buf, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		src := lineComment.ReplaceAllString(string(buf), "")

		if loc := updateRe.FindString(src); loc != "" {
			t.Errorf("%s: an UPDATE writes channel_id, which must never happen:\n\t%s\n"+
				"A node's channel tag is write-once (docs/kv-channel-acl.md §7 H1). "+
				"Moving data between channels is copy-then-delete by somebody who can "+
				"read both, not a relabel.", name, strings.Join(strings.Fields(loc), " "))
		}

		// Count INSERTs that name channel_id in their column list. The window
		// is bounded by length rather than by the next backtick, so a
		// statement built from concatenated fragments is still counted.
		for _, m := range insertRe.FindAllStringIndex(src, -1) {
			end := m[0] + 600
			if end > len(src) {
				end = len(src)
			}
			stmt := src[m[0]:end]
			if i := strings.Index(stmt, ";"); i >= 0 {
				stmt = stmt[:i]
			}
			if strings.Contains(stmt, "channel_id") {
				found[name]++
			}
		}
	}

	for name, want := range allowed {
		if found[name] != want {
			t.Errorf("%s: %d INSERTs write channel_id, expected %d. "+
				"A new creation path must gate on authorizeKVNodeCreate and "+
				"compare the tag in its replay check; then update this test.",
				name, found[name], want)
		}
	}
	for name, got := range found {
		if _, ok := allowed[name]; !ok {
			t.Errorf("%s: writes channel_id but is not an allowed creation site (%d INSERTs). "+
				"See docs/kv-channel-acl.md §7 H1.", name, got)
		}
	}
}
