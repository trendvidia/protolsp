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

// containsAll asserts every want label appears in got. Phase 4
// completion returns a fixed candidate set per scope; the test
// only checks the items we care about so adding a new keyword
// later doesn't ripple through every test.
func containsAll(t *testing.T, got, want []string, msg string) {
	t.Helper()
	gotSet := make(map[string]struct{}, len(got))
	for _, g := range got {
		gotSet[g] = struct{}{}
	}
	for _, w := range want {
		_, ok := gotSet[w]
		assert.True(t, ok, "%s: missing %q in %v", msg, w, got)
	}
}

func containsNone(t *testing.T, got, banned []string, msg string) {
	t.Helper()
	gotSet := make(map[string]struct{}, len(got))
	for _, g := range got {
		gotSet[g] = struct{}{}
	}
	for _, b := range banned {
		_, ok := gotSet[b]
		assert.False(t, ok, "%s: unexpected %q in %v", msg, b, got)
	}
}

// File-scope completion: at the top level of a .proto, suggest the
// declarations that can appear there. The fixture is intentionally
// empty so the cursor sits in pure file scope — no message body,
// no service body. Phase 4a does not differentiate "before syntax"
// vs. "after syntax"; both report the same set.
func TestProtoCompletion_FileScope(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/empty.proto"
	// A syntax line is the realistic "barely started" case — the
	// parser produces a valid FileNode and the cursor on the next
	// (blank) line is in file scope.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, "syntax = \"proto3\";\n\n")))

	list := completionFromHandle(t, srv, u, 1, 0)
	require.NotNil(t, list, "file-scope completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{
		"syntax", "package", "import", "option",
		"message", "enum", "service", "extend",
	}, "file scope offers top-level keywords")
	// Field-type names should NOT appear in file scope — there's
	// nowhere to put them.
	containsNone(t, labels, []string{"string", "int32", "bool"},
		"file scope does not offer scalar types")
}

const completionMessageFixture = `syntax = "proto3";

package acme.demo.v1;

message Greeting {
  string name = 1;
}

enum Tone {
  TONE_UNSPECIFIED = 0;
  TONE_FRIENDLY = 1;
}

message Letter {

}
`

// Message-scope completion: inside `message Letter { … }` the
// server should offer (a) keywords that open a declaration in a
// message body, (b) the built-in scalar types, (c) message + enum
// names declared elsewhere in this file. The cursor sits on a
// blank line between the opening + closing braces.
func TestProtoCompletion_MessageScope(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/letter.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, completionMessageFixture)))

	// Line 14 is the blank line inside message Letter. Cursor at
	// column 2 (typical indent depth).
	list := completionFromHandle(t, srv, u, 14, 2)
	require.NotNil(t, list, "message scope completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{
		"message", "enum", "option", "map",
		"oneof", "reserved", "extensions",
		"optional", "repeated",
	}, "message scope offers field/decl keywords")
	containsAll(t, labels, []string{
		"bool", "bytes", "string", "int32", "int64",
		"uint32", "sint64", "fixed32", "sfixed64",
		"double", "float",
	}, "message scope offers all built-in scalar types")
	containsAll(t, labels, []string{"Greeting", "Tone"},
		"message scope offers in-file message + enum names")
}

const completionEnumFixture = `syntax = "proto3";
package acme.demo.v1;

enum Tone {
  TONE_UNSPECIFIED = 0;

}
`

// Enum-scope completion: inside `enum Tone { … }` we expect a
// minimal keyword set (option, reserved). No field types, no
// message/enum suggestions — enum bodies hold values + options.
func TestProtoCompletion_EnumScope(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/tone.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, completionEnumFixture)))

	// Line 5 is the blank line inside enum Tone.
	list := completionFromHandle(t, srv, u, 5, 2)
	require.NotNil(t, list, "enum scope completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"option", "reserved"},
		"enum scope offers option + reserved")
	containsNone(t, labels, []string{"string", "int32", "message", "rpc"},
		"enum scope does not offer field types or other-scope keywords")
}

const completionServiceFixture = `syntax = "proto3";
package acme.demo.v1;

message Empty {}

service Greeter {

}
`

// Service-scope completion: inside `service Greeter { … }` we
// expect rpc / option / stream. No field types here either.
func TestProtoCompletion_ServiceScope(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greeter.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, completionServiceFixture)))

	// Line 6 is the blank line inside service Greeter.
	list := completionFromHandle(t, srv, u, 6, 2)
	require.NotNil(t, list, "service scope completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"rpc", "option", "stream"},
		"service scope offers rpc/option/stream")
	containsNone(t, labels, []string{"string", "message", "reserved"},
		"service scope does not offer field types or message-scope keywords")
}

// Dot-completion fixture: a message with two nested messages and an
// enum. The `Hint:` lines carry dotted references the tests target
// with cursor positions — they're invalid as proto bodies in
// isolation but protocompile keeps the top-level Outer / Other
// declarations parseable, which is all the qualifier resolver
// needs.
const dotInFileFixture = `syntax = "proto3";
package acme.demo.v1;

message Outer {
  message Inner1 {
    message Deepest {}
  }
  message Inner2 {}
  enum Color {
    COLOR_UNSPECIFIED = 0;
    COLOR_RED = 1;
  }
}

message Other {
  Outer.Inner1 child = 1;
  Outer.Inner1.Deepest deeper = 2;
}
`

// In-file dot completion: cursor after `Outer.` (sitting on the
// "Inner1" leaf that the client will then filter on) resolves
// Outer in the current file and emits its nested messages + enums.
func TestProtoCompletion_DotCompletion_InFileNested(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/dot.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, dotInFileFixture)))

	// Line 15 (0-indexed): "  Outer.Inner1 child = 1;"
	// Character 8 lands one past the `.` — start of the "Inner1"
	// identifier. protoDotCompletionPrefix walks back past
	// "Inner1" (empty leaf if cursor is exactly at 8) → dot at 7
	// → identifier "Outer" → qualifier "Outer".
	list := completionFromHandle(t, srv, u, 15, 8)
	require.NotNil(t, list, "dot completion returns a list when qualifier resolves")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"Inner1", "Inner2", "Color"},
		"Outer.<c> surfaces Outer's nested messages + enum")
	// Top-level types of the file should NOT appear here — dot
	// completion replaces the scope fallback when it resolves.
	containsNone(t, labels, []string{"Outer", "Other", "message", "string", "int32"},
		"dot completion suppresses scope-fallback keywords + types")
}

// Multi-segment qualifier: `Outer.Inner1.<c>` resolves to the
// nested Inner1 and emits Inner1's children (just Deepest).
func TestProtoCompletion_DotCompletion_InFileMultiSegment(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/dot.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, dotInFileFixture)))

	// Line 16 (0-indexed): "  Outer.Inner1.Deepest deeper = 2;"
	// Character 15 lands one past the second `.` — start of
	// "Deepest". Qualifier resolves to "Outer.Inner1".
	list := completionFromHandle(t, srv, u, 16, 15)
	require.NotNil(t, list, "multi-segment qualifier resolves")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"Deepest"},
		"Outer.Inner1.<c> surfaces Inner1's nested types")
}

// Unresolved qualifier falls through to scope-based completion —
// the user gets the message-body keywords + types they'd get
// without a dot prefix at all. This is the "I typed something
// that doesn't refer to anything" recovery path.
func TestProtoCompletion_DotCompletion_FallsThroughOnUnresolved(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	// Use a fixture where the cursor lands after `NoSuchType.` —
	// a qualifier that doesn't resolve in-file or cross-file.
	const src = `syntax = "proto3";
package acme.demo.v1;

message Holder {
  NoSuchType. value = 1;
}
`
	const u = "file:///work/unresolved.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))

	// Line 4 (0-indexed): "  NoSuchType. value = 1;"
	// Character 13 = right after the `.`. Qualifier "NoSuchType"
	// doesn't resolve → fall through → message-scope completion.
	list := completionFromHandle(t, srv, u, 4, 13)
	require.NotNil(t, list, "fall-through produces scope-based completion")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"message", "string"},
		"fall-through surfaces message-scope keywords + scalar types")
}

// Cross-file dot completion: foo.proto imports bar.proto (package
// acme.demo.v1, message Bar). Cursor after `acme.demo.v1.` should
// surface Bar (and any other top-level type in that package).
func TestProtoCompletion_DotCompletion_CrossFilePackage(t *testing.T) {
	root, _, _, srv := setupCrossFileWorkspace(t)
	// Overwrite foo.proto with a dot-completion target line so we
	// have a known cursor position to drive against.
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "acme/bar.proto";

message Foo {
  acme.demo.v1.Bar reference = 1;
}
`)
	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 2, mustRead(t, fooPath))))

	// Line 7 (0-indexed): "  acme.demo.v1.Bar reference = 1;"
	// Character 15 = one past the last `.` (start of "Bar").
	// Qualifier "acme.demo.v1" matches bar.proto's package →
	// emit bar.proto's top-level types.
	list := completionFromHandle(t, srv, fooURI, 7, 15)
	require.NotNil(t, list, "cross-file dot completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"Bar"},
		"qualifier matching an imported file's package surfaces its top-level types")
}

// Nested in-file types complete under their scoped path: the
// #241-strict resolver only binds `Outer.Inner1` from outside Outer,
// so that's the label completion must offer — the pre-#240 bare
// `Inner1` inserted a reference that resolved to nothing.
func TestProtoCompletion_InFileNestedTypesScopedLabels(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/dot.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, dotInFileFixture)))

	// Line 15 col 2: start of the field line inside message Other —
	// no dot before the cursor, so this is scope-fallback completion.
	list := completionFromHandle(t, srv, u, 15, 2)
	require.NotNil(t, list)
	labels := labelsFromList(list)
	containsAll(t, labels, []string{
		"Outer", "Other",
		"Outer.Inner1", "Outer.Inner1.Deepest", "Outer.Inner2", "Outer.Color",
	}, "nested types complete under their rooted scope path")
	containsNone(t, labels, []string{"Inner1", "Inner2", "Deepest", "Color"},
		"bare nested names are gone — the resolver wouldn't bind them here")
}

// Imported nested types complete under package + enclosing-message
// path. The pre-#240 `pkg.leaf` shape for nested types named
// something the resolver rejects.
func TestProtoCompletion_ImportedNestedTypesFullPath(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(root, "proto", "other", "bar.proto"), `syntax = "proto3";

package other.lib.v1;

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
  string placeholder = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 7 col 2: inside Foo's body, scope-fallback completion.
	list := completionFromHandle(t, srv, fooURI, 7, 2)
	require.NotNil(t, list)
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"other.lib.v1.Outer", "other.lib.v1.Outer.Inner"},
		"imported nested types complete with package + enclosing-message path")
	containsNone(t, labels, []string{"other.lib.v1.Inner", "Inner"},
		"the package-plus-leaf shape for a nested type is gone")
}

// In-scope nested types complete under their minimal name:
// inside message B, its own nested Dup completes bare, while A's
// shadowed Dup keeps the full path — each label binds exactly its
// declaration.
func TestProtoCompletion_InFileNestedTypesMinimalLabels(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const src = `syntax = "proto3";
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
  message Solo {}

}
`
	const u = "file:///work/minimal.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))

	// Line 14: the blank line inside message B.
	list := completionFromHandle(t, srv, u, 14, 2)
	require.NotNil(t, list)
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"A", "B", "Dup", "Solo", "A.Dup"},
		"in-scope nested types complete bare; the shadowed A.Dup keeps its path")
	containsNone(t, labels, []string{"B.Dup", "B.Solo"},
		"no redundant rooted labels for in-scope types")
}

// A qualifier that is a strict prefix of a reachable package offers
// the next package segment, so the user can walk
// `acme.` → demo → v1 → types.
func TestProtoCompletion_DotCompletion_PackageSegmentContinuation(t *testing.T) {
	root, _, _, srv := setupCrossFileWorkspace(t)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "acme/bar.proto";

message Foo {
  acme.Bar x = 1;
  demo.Bar y = 1;
}
`)
	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 2, mustRead(t, fooPath))))

	// Line 7: `  acme.Bar x = 1;` — char 7 is one past the dot;
	// qualifier "acme" is a strict prefix of acme.demo.v1.
	list := completionFromHandle(t, srv, fooURI, 7, 7)
	require.NotNil(t, list, "package-prefix qualifier yields continuations")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"demo"},
		"acme.<c> offers the next package segment")
	containsNone(t, labels, []string{"Bar", "message", "string"},
		"continuations suppress the scope fallback")

	// Line 8: `  demo.Bar y = 1;` — the scope-relative form: `demo`
	// written in acme.demo.v1 names acme.demo, whose next segment
	// toward the package is v1.
	list = completionFromHandle(t, srv, fooURI, 8, 7)
	require.NotNil(t, list, "scope-relative package prefix yields continuations")
	containsAll(t, labelsFromList(list), []string{"v1"},
		"demo.<c> from acme.demo.v1 offers v1")
}

// Dot completion resolves the qualifier through the cursor's
// enclosing scopes (the completion twin of #239): `Middle.`
// written inside Outer names Outer.Middle, so Middle's nested types
// are the completions.
func TestProtoCompletion_DotCompletion_SiblingNestedScope(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const src = `syntax = "proto3";
package acme.demo.v1;

message Outer {
  message Middle {
    message Inner {}
  }
  Middle.Inner f = 1;
}
`
	const u = "file:///work/sibling.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))

	// Line 7: `  Middle.Inner f = 1;` — character 9 is one past the
	// dot, so the qualifier is "Middle", written inside Outer.
	list := completionFromHandle(t, srv, u, 7, 9)
	require.NotNil(t, list, "scope-relative qualifier resolves")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"Inner"},
		"Middle.<c> inside Outer surfaces Outer.Middle's nested types")
	containsNone(t, labels, []string{"message", "string"},
		"dot completion suppresses the scope fallback")
}

// A package qualifier also honors the scope chain: `demo.v1.`
// written in package acme.demo.v1 names acme.demo.v1 (protoc
// resolves `demo.v1.Bar` there), so that package's types complete —
// from the import AND the current file, since packages span files.
func TestProtoCompletion_DotCompletion_ScopeRelativePackage(t *testing.T) {
	root, _, _, srv := setupCrossFileWorkspace(t)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "acme/bar.proto";

message Foo {
  demo.v1.Bar ref = 1;
}
`)
	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 2, mustRead(t, fooPath))))

	// Line 7: `  demo.v1.Bar ref = 1;` — character 10 is one past the
	// second dot; the qualifier is "demo.v1".
	list := completionFromHandle(t, srv, fooURI, 7, 10)
	require.NotNil(t, list, "scope-relative package qualifier resolves")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"Bar", "Foo"},
		"demo.v1.<c> from package acme.demo.v1 surfaces the package's types across files")
}

// Cross-file completion: foo.proto imports bar.proto from the same
// package; inside foo.proto's message body, completion should
// surface `Bar` qualified as `acme.demo.v1.Bar`. The phase 3
// cross-file index makes the import reachable; phase 4 surfaces
// the symbols it indexed.
func TestProtoCompletion_ImportedTypesQualifiedByPackage(t *testing.T) {
	_, fooPath, _, srv := setupCrossFileWorkspace(t)
	fooURI := "file://" + filepath.ToSlash(fooPath)

	// foo.proto's message body starts on line 6 (`message Foo {`)
	// and runs through line 8. Line 7 has `  Bar reference = 1;`;
	// cursor at line 8 column 0 sits at the closing brace which
	// would still resolve to MessageNode (the brace is part of
	// the message's span). To be safer, position the cursor on a
	// blank line we add inside the body by extending the fixture
	// is overkill; line 7 col 0 (start of the field line) is
	// inside the message body.
	list := completionFromHandle(t, srv, fooURI, 7, 0)
	require.NotNil(t, list, "cross-file completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"acme.demo.v1.Bar"},
		"imported message names are qualified by their package")
	// The current file also defines Foo — it should appear bare
	// (in-file types aren't package-qualified).
	containsAll(t, labels, []string{"Foo"},
		"in-file types appear bare in the same package")
}
