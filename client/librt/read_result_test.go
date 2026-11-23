// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// Fork-only (docs/client-analytics.md). A ThreadReadResult built anywhere but
// its constructors could leave Source at zero, which the app reads as
// "unknown" and silently drops from analytics. Go can't forbid the literal,
// so this test does.
func TestThreadReadResultConstructors(t *testing.T) {
	allowed := map[string]bool{
		"cachedRead": true, "emptyRead": true, "serverRead": true, "staleRead": true,
	}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == "ThreadReadResult" {
					if fn == nil || !allowed[fn.Name.Name] {
						t.Errorf("%s: ThreadReadResult{} outside its constructors; use cachedRead/serverRead/staleRead/emptyRead",
							fset.Position(lit.Pos()))
					}
				}
				return true
			})
		}
	}
}

func TestServerReadSource(t *testing.T) {
	msgs := func(seqs ...int) []ThreadMessage {
		var ret []ThreadMessage
		for _, s := range seqs {
			ret = append(ret, ThreadMessage{Seq: proto.RTMsgSeq(s)})
		}
		return ret
	}
	var ch proto.RTChannelID

	// The cache supplied some of the page: warm.
	require.Equal(t, lcl.RTThreadReadSource_Warm,
		serverRead(msgs(5, 4, 3), msgs(3, 2, 1), ch, false).Source)
	// The cache held messages, but none made the page: cold.
	require.Equal(t, lcl.RTThreadReadSource_Cold,
		serverRead(msgs(200, 199), msgs(50, 49), ch, false).Source)
	// Nothing cached: cold.
	require.Equal(t, lcl.RTThreadReadSource_Cold,
		serverRead(msgs(2, 1), nil, ch, true).Source)

	require.Equal(t, lcl.RTThreadReadSource_Cached, cachedRead(msgs(1), ch).Source)
	require.Equal(t, lcl.RTThreadReadSource_Cached, emptyRead().Source)
	require.Equal(t, lcl.RTThreadReadSource_Offline, staleRead(msgs(1), ch, nil).Source)
}
