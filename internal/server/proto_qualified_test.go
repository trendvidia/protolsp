// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"
)

func TestProtoDefinition_QualifiedTypeReference_ResolvesViaImport(t *testing.T) {
	// foo.proto in package acme.demo.v1 imports another file in a
	// DIFFERENT package, then references its type using the fully-
	// qualified name. The resolver must walk imports + filter by
	// the named package.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	barPath := filepath.Join(root, "proto", "other", "bar.proto")
	mustWrite(t, barPath, `syntax = "proto3";

package other.lib.v1;

message Bar {
  string x = 1;
}
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "other/bar.proto";

message Foo {
  other.lib.v1.Bar field = 1;
}
`)

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	expectedBarURI := uri.URI("file://" + filepath.ToSlash(barPath))

	// Line 7 (0-indexed): `  other.lib.v1.Bar field = 1;`. The
	// cursor on the trailing "Bar" component (character 15 lands
	// in "Bar") should resolve through the compound ident path.
	locs := definitionFromHandle(t, srv, fooURI, 7, 15)
	require.Len(t, locs, 1, "fully-qualified cross-package ref resolves")
	assert.Equal(t, expectedBarURI, locs[0].URI,
		"go-to-def lands on bar.proto")
}

func TestProtoHover_QualifiedTypeReference_ResolvesViaImport(t *testing.T) {
	// Same workspace shape as the definition test; hover should
	// render Bar's card from bar.proto.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	barPath := filepath.Join(root, "proto", "other", "bar.proto")
	mustWrite(t, barPath, `syntax = "proto3";

package other.lib.v1;

// Documentation for Bar.
message Bar {
  string x = 1;
}
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "other/bar.proto";

message Foo {
  other.lib.v1.Bar field = 1;
}
`)

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	hover := hoverFromHandle(t, srv, fooURI, 7, 15)
	require.NotNil(t, hover, "qualified cross-package hover should resolve")
	assert.Contains(t, hoverBody(hover), "**message Bar**")
	assert.Contains(t, hoverBody(hover), "Documentation for Bar")
}

func TestProtoDefinition_QualifiedRef_WrongPackage_DoesNotResolve(t *testing.T) {
	// Two imported files declare a `Bar`, but only one is in the
	// referenced package. The qualifier MUST steer resolution to
	// the right file — without package filtering, the resolver
	// would walk imports in source order and return whichever
	// Bar happened to come first.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(root, "proto", "a", "bar.proto"), `syntax = "proto3";
package wrong.pkg.v1;
message Bar { string fromA = 1; }
`)
	rightPath := filepath.Join(root, "proto", "b", "bar.proto")
	mustWrite(t, rightPath, `syntax = "proto3";
package right.pkg.v1;
message Bar { string fromB = 1; }
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "a/bar.proto";
import "b/bar.proto";

message Foo {
  right.pkg.v1.Bar field = 1;
}
`)

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 8: `  right.pkg.v1.Bar field = 1;` — cursor on "Bar".
	locs := definitionFromHandle(t, srv, fooURI, 8, 16)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file://"+filepath.ToSlash(rightPath)), locs[0].URI,
		"qualifier `right.pkg.v1` steers resolution past the wrong-package file")
}

// TestProtoIndex_PartialSuccess_BrokenFileDoesNotKillGoodFile is
// the audit's "partial-success" gap: a workspace whose proto
// index contains one syntactically broken file must still resolve
// symbols defined in the OTHER, syntactically valid files. A
// regression where the index aborts on the first parse error
// would silently kill hover / definition for every file in the
// workspace once any single .proto goes bad mid-edit — a common
// state while a user is typing.
func TestProtoIndex_PartialSuccess_BrokenFileDoesNotKillGoodFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")

	// Good file — fully valid, declares MyType.
	goodPath := filepath.Join(root, "proto", "good.proto")
	mustWrite(t, goodPath, `syntax = "proto3";

package good.v1;

// MyType is the symbol we'll resolve through definition.
message MyType {
  string field = 1;
}

message Holder {
  MyType target = 1;
}
`)

	// Broken file — unterminated message body. protoindex's parse
	// pass MUST tolerate this without aborting the whole index.
	mustWrite(t, filepath.Join(root, "proto", "broken.proto"), `syntax = "proto3";

package broken.v1;

message Broken {
  string oops = 1
// missing semicolon, unclosed brace — deliberate syntax error
`)

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	goodURI := "file://" + filepath.ToSlash(goodPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, goodURI, 1, mustRead(t, goodPath))))

	// Definition request on MyType from inside Holder. With a
	// healthy index this lands on the MyType declaration line.
	// Without partial-success tolerance, the broken file's parse
	// failure earlier in the index walk would have prevented this
	// lookup from succeeding.
	//
	// Line 10 (0-indexed): `  MyType target = 1;`. Character 4
	// lands inside "MyType".
	locs := definitionFromHandle(t, srv, goodURI, 10, 4)
	require.Len(t, locs, 1,
		"good.proto's MyType must still resolve despite broken.proto in the same workspace")
	assert.Equal(t, uri.URI("file://"+filepath.ToSlash(goodPath)), locs[0].URI)
	// Line 5 (0-indexed): `message MyType {` — the message head.
	assert.Equal(t, uint32(5), locs[0].Range.Start.Line,
		"definition anchors on the message declaration line")
}

func TestProtoDefinition_QualifiedRef_BareLeafStillWorks(t *testing.T) {
	// When the qualified name's package matches the current file's
	// own package, the in-file declaration should still win.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

message Bar { string x = 1; }

message Foo {
  acme.demo.v1.Bar self = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 7: `  acme.demo.v1.Bar self = 1;` — cursor on "Bar".
	locs := definitionFromHandle(t, srv, fooURI, 7, 17)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file://"+filepath.ToSlash(fooPath)), locs[0].URI,
		"intra-package qualified ref resolves to the current file's declaration")
	assert.Equal(t, uint32(4), locs[0].Range.Start.Line,
		"the in-file Bar declaration lives on line 4 (0-indexed)")
}

// nestedBarProto declares nested types inside Outer. Line map
// (0-indexed): 4 `message Outer {`, 6 `  message Inner {`,
// 9 `  enum Mode {`.
const nestedBarProto = `syntax = "proto3";

package other.lib.v1;

message Outer {
  // Nested payload message.
  message Inner {
    string id = 1;
  }
  enum Mode {
    MODE_UNSPECIFIED = 0;
  }
}
`

func TestProtoNavigation_OuterQualifiedNestedType_CrossPackage(t *testing.T) {
	// A nested message/enum qualified by its enclosing message
	// (`pkg.Outer.Inner`) resolves through the local protoindex — the
	// qualifier is the package PLUS enclosing-message segments, which
	// the old qualifier-equals-package check filtered out.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	barPath := filepath.Join(root, "proto", "other", "bar.proto")
	mustWrite(t, barPath, nestedBarProto)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "other/bar.proto";

message Foo {
  other.lib.v1.Outer.Inner in = 1;
  .other.lib.v1.Outer.Mode mode = 2;
}
`)

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	expectedBarURI := uri.URI("file://" + filepath.ToSlash(barPath))

	// Line 7: `  other.lib.v1.Outer.Inner in = 1;` — cursor on "Inner".
	locs := definitionFromHandle(t, srv, fooURI, 7, 22)
	require.Len(t, locs, 1, "outer-qualified nested message resolves")
	assert.Equal(t, expectedBarURI, locs[0].URI)
	assert.Equal(t, uint32(6), locs[0].Range.Start.Line,
		"definition lands on the nested Inner declaration")

	hover := hoverFromHandle(t, srv, fooURI, 7, 22)
	require.NotNil(t, hover, "outer-qualified nested message hovers")
	assert.Contains(t, hoverBody(hover), "**message Inner**")
	assert.Contains(t, hoverBody(hover), "Nested payload message.")

	// Line 8: `  .other.lib.v1.Outer.Mode mode = 2;` — the fully-
	// qualified leading-dot form, cursor on "Mode" (a nested enum).
	locs = definitionFromHandle(t, srv, fooURI, 8, 23)
	require.Len(t, locs, 1, "fully-qualified nested enum resolves")
	assert.Equal(t, expectedBarURI, locs[0].URI)
	assert.Equal(t, uint32(9), locs[0].Range.Start.Line,
		"definition lands on the nested Mode declaration")
}

func TestProtoNavigation_OuterQualifiedNestedType_InPackage(t *testing.T) {
	// The in-package `Outer.Inner` form — no package prefix — must
	// resolve against the current file's own nesting.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

message Outer {
  message Inner {
    string id = 1;
  }
}

message Foo {
  Outer.Inner f = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 11: `  Outer.Inner f = 1;` — cursor on "Inner".
	locs := definitionFromHandle(t, srv, fooURI, 11, 9)
	require.Len(t, locs, 1, "in-package Outer.Inner resolves in-file")
	assert.Equal(t, uri.URI(fooURI), locs[0].URI)
	assert.Equal(t, uint32(5), locs[0].Range.Start.Line,
		"definition lands on the nested Inner declaration")

	hover := hoverFromHandle(t, srv, fooURI, 11, 9)
	require.NotNil(t, hover)
	assert.Contains(t, hoverBody(hover), "**message Inner**")
}

func TestProtoNavigation_SiblingNestedTypeFromEnclosingScope(t *testing.T) {
	// `Middle.Inner` referenced from a field of
	// Outer, where Middle nests in Outer. The reference resolves via
	// the enclosing scope .pkg.Outer — the message-level scope the
	// earlier resolver didn't model.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

message Outer {
  message Middle {
    // Deeply nested payload.
    message Inner {
      string id = 1;
    }
  }
  Middle.Inner f = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 11: `  Middle.Inner f = 1;` — cursor on "Inner".
	locs := definitionFromHandle(t, srv, fooURI, 11, 10)
	require.Len(t, locs, 1, "sibling nested type resolves through the enclosing scope")
	assert.Equal(t, uri.URI(fooURI), locs[0].URI)
	assert.Equal(t, uint32(7), locs[0].Range.Start.Line,
		"definition lands on the doubly nested Inner declaration")

	hover := hoverFromHandle(t, srv, fooURI, 11, 10)
	require.NotNil(t, hover)
	assert.Contains(t, hoverBody(hover), "**message Inner**")
	assert.Contains(t, hoverBody(hover), "Deeply nested payload.")
}

func TestProtoNavigation_BareNamePrefersNearestEnclosingScope(t *testing.T) {
	// Two messages named Dup in different scopes. A bare `Dup`
	// referenced from inside B must resolve to B's own Dup, not the
	// pre-order-first A.Dup.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

message A {
  message Dup {
    string a = 1;
  }
}

message B {
  message Dup {
    string b = 1;
  }
  Dup f = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 14: `  Dup f = 1;` — cursor on "Dup".
	locs := definitionFromHandle(t, srv, fooURI, 14, 3)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI(fooURI), locs[0].URI)
	assert.Equal(t, uint32(11), locs[0].Range.Start.Line,
		"bare name binds the Dup nested in the enclosing B, not the pre-order-first A.Dup")
}

func TestProtoNavigation_StrictMatching_ProtocRejectedFormsDoNotResolve(t *testing.T) {
	// The inverse of TestProtoDefinition_QualifiedRef_WrongPackage
	// _DoesNotResolve: references protoc rejects yield no hover and no
	// definition —
	//   - a bare name whose declaration lives in a DIFFERENT package,
	//   - a package-qualified leaf whose declaration is nested inside
	//     another message (protoc requires the full Outer.Inner path).
	// The earlier lenient fallbacks resolved both.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(root, "proto", "other", "bar.proto"), `syntax = "proto3";

package other.lib.v1;

message Bar {
  string x = 1;
}

message Outer {
  message Inner {
    string id = 1;
  }
}
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "other/bar.proto";

message Foo {
  Bar bare = 1;
  other.lib.v1.Inner leaf = 2;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 7: `  Bar bare = 1;` — bare cross-package reference.
	assert.Empty(t, definitionFromHandle(t, srv, fooURI, 7, 3),
		"a bare name doesn't bind across packages")
	assert.Nil(t, hoverFromHandle(t, srv, fooURI, 7, 3),
		"no hover for a reference protoc rejects")

	// Line 8: `  other.lib.v1.Inner leaf = 2;` — the leaf exists in
	// that package but only nested inside Outer.
	assert.Empty(t, definitionFromHandle(t, srv, fooURI, 8, 16),
		"a package-qualified leaf doesn't match at nesting depth")
	assert.Nil(t, hoverFromHandle(t, srv, fooURI, 8, 16))
}

func TestProtoNavigation_BareSamePackageCrossFile_StillResolves(t *testing.T) {
	// The protoc-accepted twin of the strict-matching test: a bare
	// name declared top-level in ANOTHER file of the SAME package must
	// keep resolving under strict scoping (the scope chain covers it — no
	// leniency involved).
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	siblingPath := filepath.Join(root, "proto", "acme", "sibling.proto")
	mustWrite(t, siblingPath, `syntax = "proto3";

package acme.demo.v1;

// Sibling bar.
message Bar {
  string x = 1;
}
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "acme/sibling.proto";

message Foo {
  Bar b = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 7: `  Bar b = 1;` — cursor on "Bar".
	locs := definitionFromHandle(t, srv, fooURI, 7, 3)
	require.Len(t, locs, 1, "same-package cross-file bare name resolves")
	assert.Equal(t, uri.URI("file://"+filepath.ToSlash(siblingPath)), locs[0].URI)
	assert.Equal(t, uint32(5), locs[0].Range.Start.Line)

	hover := hoverFromHandle(t, srv, fooURI, 7, 3)
	require.NotNil(t, hover)
	assert.Contains(t, hoverBody(hover), "**message Bar**")
	assert.Contains(t, hoverBody(hover), "Sibling bar.")
}

func TestProtoNavigation_GroupDeclaredInExtendResolves(t *testing.T) {
	// A proto2 group declared inside an extend block defines a
	// message in the enclosing scope. The extend body is transparent
	// to the rooted walk, so `Holder.Extra` resolves.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto2";

package p;

message Extendee {
  extensions 100 to 199;
}

message Holder {
  extend Extendee {
    optional group Extra = 100 {
      optional string x = 1;
    }
  }
  optional Holder.Extra f = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 14: `  optional Holder.Extra f = 1;` — cursor on "Extra".
	locs := definitionFromHandle(t, srv, fooURI, 14, 19)
	require.Len(t, locs, 1, "extend-declared group type resolves")
	assert.Equal(t, uri.URI(fooURI), locs[0].URI)
	assert.Equal(t, uint32(10), locs[0].Range.Start.Line,
		"definition lands on the group declaration")

	hover := hoverFromHandle(t, srv, fooURI, 14, 19)
	require.NotNil(t, hover, "extend-declared group type hovers")
	assert.Contains(t, hoverBody(hover), "**message Extra**")
}

func TestProtoNavigation_OuterQualifiedNestedType_MissDegrades(t *testing.T) {
	// A nested path whose leaf doesn't exist keeps the graceful nil:
	// no hover, no definition, no error.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(root, "proto", "other", "bar.proto"), nestedBarProto)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "other/bar.proto";

message Foo {
  other.lib.v1.Outer.Missing m = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 7: cursor on "Missing".
	assert.Empty(t, definitionFromHandle(t, srv, fooURI, 7, 22),
		"unknown nested leaf → no definition")
	assert.Nil(t, hoverFromHandle(t, srv, fooURI, 7, 22),
		"unknown nested leaf → no hover")
}
