// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Cross-repo verification for appviewer#331 (the last acceptance item):
// pointed at AppViewer's *published* structural document schema,
// protolsp folds `children`, outlines it by node id, and flags a
// duplicate id — proven over a real AppViewer-style Screen document.
//
// Unlike keyed_test.go (which hand-builds a descriptor with the
// (pxf.key) option as raw option bytes), this test compiles AppViewer's
// actual document.proto from disk through schema.OpenProtoFiles — the
// same protocompile path a workspace's `proto_files` source uses. That
// exercises the real seam: the schema's `import "pxf/annotations.proto"`
// must resolve, the (pxf.key)="id" annotation on Node.children must
// survive compilation as a known extension, and protolsp must then treat
// `children` as keyed.
//
// The schema is VENDORED into testdata (appviewer/ui/v1/document.proto)
// so the test is hermetic — no runtime dependency on an appviewer
// checkout. Its `import "pxf/annotations.proto"` resolves against
// protolsp's own bundled proto/ tree (not a vendored fixture copy), so
// this test doubles as the regression guard for #254: if the bundled
// annotations.proto were missing (pxf.key) = 1316, the (pxf.key)="id"
// annotation on Node.children would compile as an unknown extension and
// the keyed assertions below would fail.
package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/schema"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// appviewerSchemaRoot is the vendored fixture import root: it holds
// appviewer/ui/v1/document.proto. Its `import "pxf/annotations.proto"`
// resolves against the bundledProtoRoot import root below.
const appviewerSchemaRoot = "testdata/appviewer_schema"

// bundledProtoRoot is protolsp's own proto/ tree (relative to this
// package), which ships pxf/annotations.proto. Adding it as a second
// import root is what lets the vendored schema resolve its annotations
// import against the bundled copy — the seam #254 fixed.
const bundledProtoRoot = "../../proto"

// A real AppViewer Screen document (composite/screens shape): `@type`
// opts into appviewer.ui.v1.Screen, and `children` is authored as a
// block of named blocks whose names are node ids. Only structural fields
// the schema declares (name, root, type, children) appear, so a valid
// document produces zero diagnostics of any kind.
const appviewerScreenValid = `@type appviewer.ui.v1.Screen

name = "main"
root {
  type = "VBox"
  children {
    greeting {
      type = "Label"
    }
    counter_row {
      type = "HBox"
      children {
        inc {
          type = "Button"
        }
      }
    }
  }
}
`

// newServerWithAppViewerSchema compiles the vendored AppViewer schema
// from disk (the real protocompile path) and installs the resulting
// Lookup on a fresh server. The returned CompileErrors are surfaced to
// the caller: a non-empty slice is itself the finding — it would mean
// AppViewer's published schema doesn't compile as-is.
func newServerWithAppViewerSchema(t *testing.T) (*Server, *fakeConn, schema.Lookup) {
	t.Helper()
	absRoot, err := filepath.Abs(appviewerSchemaRoot)
	require.NoError(t, err)
	absBundled, err := filepath.Abs(bundledProtoRoot)
	require.NoError(t, err)

	lookup, closer, compileErrs, err := schema.OpenProtoFiles(context.Background(),
		schema.ProtoFilesConfig{Sources: []schema.ProtoFilesSource{
			{Paths: []string{absRoot, absBundled}},
		}}, nil)
	require.NoError(t, err, "compiling AppViewer's schema must not fail at setup")
	require.Empty(t, compileErrs,
		"AppViewer's published document.proto must compile as-is (imports resolve, "+
			"(pxf.key) annotation is known): %v", compileErrs)
	require.NotNil(t, lookup, "vendored schema must produce a Lookup")
	t.Cleanup(func() { _ = closer.Close() })

	srv, fc := newServerWithFakeConn(t)
	srv.Lookups().Swap(lookup, nil)
	return srv, fc, lookup
}

// TestAppViewerSchema_NodeChildrenResolvesAsKeyed asserts the schema
// contract at the descriptor level: @type resolves to the Screen
// message, and Node.children carries a valid (pxf.key)="id" annotation
// after compilation — the precondition for every keyed editor feature.
func TestAppViewerSchema_NodeChildrenResolvesAsKeyed(t *testing.T) {
	_, _, lookup := newServerWithAppViewerSchema(t)

	screen, err := lookup.FindMessageByName(protoreflect.FullName("appviewer.ui.v1.Screen"))
	require.NoError(t, err, "@type target appviewer.ui.v1.Screen must resolve")
	require.NotNil(t, screen)

	node, err := lookup.FindMessageByName(protoreflect.FullName("appviewer.ui.v1.Node"))
	require.NoError(t, err)
	require.NotNil(t, node)

	children := node.Descriptor().Fields().ByName("children")
	require.NotNil(t, children, "Node has a children field")
	keyFd := pxf.KeyField(children)
	require.NotNil(t, keyFd,
		"Node.children must be keyed after compilation — the (pxf.key) extension "+
			"must survive as a known option")
	assert.Equal(t, protoreflect.Name("id"), keyFd.Name(),
		"children is keyed by the node id, per the published schema")
}

// TestAppViewerSchema_ChildrenOutlineByNodeID is the acceptance core:
// protolsp folds the `children` block and outlines each entry by its node
// id (the block name), nested against the element message. `children`
// sits under `root`, so this also proves the outline recurses through a
// singular message field (Screen.root : Node) before reaching the keyed
// collection.
func TestAppViewerSchema_ChildrenOutlineByNodeID(t *testing.T) {
	srv, _, _ := newServerWithAppViewerSchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/main.pxf", 1, appviewerScreenValid)))

	symbols := documentSymbolsFromHandle(t, srv, "file:///work/main.pxf")

	root := findSymbol(symbols, "root")
	require.NotNil(t, root, "Screen.root outlines as a block symbol")

	children := findSymbol(root.Children, "children")
	require.NotNil(t, children, "the keyed field block is a symbol under root")
	require.Len(t, children.Children, 2, "each keyed entry is a named child symbol")

	greeting := findSymbol(children.Children, "greeting")
	require.NotNil(t, greeting, "entry outlines by its node id, not by a field name")
	assert.Equal(t, protocol.SymbolKindObject, greeting.Kind)
	assert.Equal(t, "appviewer.ui.v1.Node", greeting.Detail,
		"entry detail is the element message type")
	// The entry body outlines against the Node message: its `type` field
	// is a child symbol.
	assert.NotNil(t, findSymbol(greeting.Children, "type"),
		"entry body outlines against the Node message")

	counterRow := findSymbol(children.Children, "counter_row")
	require.NotNil(t, counterRow, "second keyed entry outlines by its node id")
	// The nested keyed collection drills in recursively: counter_row's
	// own children block outlines its `inc` entry.
	nested := findSymbol(counterRow.Children, "children")
	require.NotNil(t, nested, "a keyed entry's own children block outlines recursively")
	assert.NotNil(t, findSymbol(nested.Children, "inc"),
		"the deeper keyed entry outlines by its node id too")
}

// TestAppViewerSchema_ValidDocumentHasNoKeyedDiagnostics guards that a
// well-formed AppViewer document — distinct node ids, only structural
// fields — produces no keyed diagnostics and no spurious unknown-field
// noise from the keyed entries.
func TestAppViewerSchema_ValidDocumentHasNoKeyedDiagnostics(t *testing.T) {
	srv, fc, _ := newServerWithAppViewerSchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/main.pxf", 1, appviewerScreenValid)))

	for _, d := range lastDiagnostics(t, fc) {
		assert.NotEqual(t, "protolsp/keyed", d.Source,
			"valid keyed doc must not raise keyed diagnostics: %q", d.Message)
		assert.NotContains(t, d.Message, "unknown field",
			"keyed entry names must not be flagged as unknown fields: %q", d.Message)
	}
}

// TestAppViewerSchema_DuplicateNodeIDDiagnostic is the other acceptance
// core: two children sharing a node id are a duplicate-key violation,
// flagged on the second occurrence with source "protolsp/keyed".
func TestAppViewerSchema_DuplicateNodeIDDiagnostic(t *testing.T) {
	const src = `@type appviewer.ui.v1.Screen

name = "main"
root {
  type = "VBox"
  children {
    greeting {
      type = "Label"
    }
    greeting {
      type = "Button"
    }
  }
}
`
	srv, fc, _ := newServerWithAppViewerSchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/main.pxf", 1, src)))

	d := findDiagnostic(lastDiagnostics(t, fc), "protolsp/keyed")
	require.NotNil(t, d, "two children with the same node id must produce a keyed diagnostic")
	assert.Contains(t, d.Message, "duplicate key")
	assert.Contains(t, d.Message, "greeting", "the diagnostic names the conflicting node id")
	// The squiggle lands on the second `greeting` (0-indexed line 9).
	assert.Equal(t, uint32(9), d.Range.Start.Line)
}
