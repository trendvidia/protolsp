// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// RFC-001 IR-artifact features (#220): function-call navigation
// inside opaque engine-expression fragments, the descriptor_path →
// source-range mapping primitive, and annotation-validation
// diagnostics resolving through imports.

package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// rfc001OpaqueCallProto exercises the opaque-fragment path: `rule`
// is the PSE pseudo-type `expression`, so the use's argument is
// captured as raw tokens (AnnotationUseArg.Value() is zero) and only
// the compile pass knows `is_email` resolved to the declared
// function.
const rfc001OpaqueCallProto = `syntax = "proto3";

package t;

annotation check(rule: expression);

// Validates RFC-5322 addresses.
function is_email(s: string);

@check(is_email(this))
message M {
  string value = 1;
}
`

// Go-to-definition on the callee name inside an opaque fragment
// jumps to the `function` declaration; the engine-expression tokens
// around it stay non-navigable.
func TestProtoDefinition_OpaqueFragmentFunctionCall(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-opaque.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))

	// Line 9 is `@check(is_email(this))`; `is_email` spans
	// chars 7–15. Line 7 declares the function, name at chars 9–17.
	locs := definitionFromHandle(t, srv, u, 9, 9)
	require.Len(t, locs, 1, "callee name inside the fragment resolves")
	assert.Equal(t, u, string(locs[0].URI))
	assert.Equal(t, rng(7, 9, 7, 17), locs[0].Range,
		"definition lands on the function declaration's name token")

	// `this` (chars 16–20) is engine vocabulary, not a function
	// reference — no definition.
	assert.Empty(t, definitionFromHandle(t, srv, u, 9, 17),
		"engine-expression tokens around the call don't navigate")
}

// Hover on the callee name renders the function declaration card,
// doc comment included.
func TestProtoHover_OpaqueFragmentFunctionCall(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-opaque-hover.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))

	hover := hoverFromHandle(t, srv, u, 9, 7)
	require.NotNil(t, hover, "callee name inside the fragment yields a hover")
	assert.Contains(t, hover.Contents.Value, "**function is_email(s: string)**")
	assert.Contains(t, hover.Contents.Value, "Validates RFC-5322 addresses.",
		"the declaration's leading doc comment flows into the card")
}

// setupRFC001OpaqueCrossFile stages a workspace where the annotation
// (expression-typed param) and the function live in an imported
// file, and the open file calls the function package-qualified from
// inside an opaque fragment.
func setupRFC001OpaqueCrossFile(t *testing.T) (fooURI, barURI string, srv *Server) {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "buf.yaml"), `version: v2
modules:
  - path: proto
`)
	barPath := filepath.Join(root, "proto", "acme", "bar.proto")
	mustWrite(t, barPath, `syntax = "proto3";

package acme.demo.v1;

annotation verify(rule: expression);

function is_uuid(s: string);
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@acme.demo.v1.verify(acme.demo.v1.is_uuid(this))
message Foo {
  string id = 1;
}
`)

	srv, _ = newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI = "file://" + filepath.ToSlash(fooPath)
	barURI = "file://" + filepath.ToSlash(barPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))
	return fooURI, barURI, srv
}

// A qualified call inside an opaque fragment resolves through the
// import: the IR resolved the callee cross-file, and the FQN maps
// back to the declaring file's AST for the jump target.
func TestProtoNavigation_OpaqueFragmentFunctionCall_CrossFile(t *testing.T) {
	fooURI, barURI, srv := setupRFC001OpaqueCrossFile(t)

	// Line 6 of foo.proto is
	// `@acme.demo.v1.verify(acme.demo.v1.is_uuid(this))`;
	// the callee name run spans chars 21–41. bar.proto declares
	// `function is_uuid(s: string);` on line 6, name at chars 9–16.
	locs := definitionFromHandle(t, srv, fooURI, 6, 35)
	require.Len(t, locs, 1, "qualified callee resolves cross-file via import")
	assert.Equal(t, barURI, string(locs[0].URI))
	assert.Equal(t, rng(6, 9, 6, 16), locs[0].Range)

	hover := hoverFromHandle(t, srv, fooURI, 6, 35)
	require.NotNil(t, hover)
	assert.Contains(t, hover.Contents.Value, "**function is_uuid(s: string)**")
}

// RFC001SourceRange maps the descriptor_path keys a protowire
// runtime attaches to violations back to editor ranges — the
// runtime-error → source mapping primitive.
func TestRFC001SourceRange_DescriptorPathKeys(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-srcmap.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))

	docURI := protocol.DocumentURI(u)

	// The annotation-use anchor: the `@` of the use on line 9.
	r, ok := srv.RFC001SourceRange(docURI, "t.M[t.check#0]")
	require.True(t, ok, "annotation-use key resolves")
	assert.Equal(t, rng(9, 0, 9, 1), r, "range covers the `@` anchor token")

	// The FUNCTION_CALL anchor: the callee ident inside the fragment.
	r, ok = srv.RFC001SourceRange(docURI, "t.M[t.check#0]/arg#0/call#0")
	require.True(t, ok, "function-call key resolves")
	assert.Equal(t, rng(9, 7, 9, 15), r, "range covers the callee ident")

	// Misses: unknown ordinal, malformed key, untracked document.
	_, ok = srv.RFC001SourceRange(docURI, "t.M[t.check#1]")
	assert.False(t, ok, "unknown ordinal misses")
	_, ok = srv.RFC001SourceRange(docURI, "not a descriptor path [")
	assert.False(t, ok, "malformed key misses")
	_, ok = srv.RFC001SourceRange(protocol.DocumentURI("file:///work/absent.proto"), "t.M[t.check#0]")
	assert.False(t, ok, "unopened document misses")
}

// rfc001AliasMapProto exercises the descriptor-path shapes that
// changed across protocompile v0.18.0/v0.19.0 (#260): a two-link
// type-alias chain feeding a plain field, a map whose value type is
// the alias, and a map whose key type is the alias.
const rfc001AliasMapProto = `syntax = "proto3";

package t;

annotation check(rule: string);

type B = string @check("base");
type A = t.B @check("derived");

message M {
  A x = 1 @check("site");
  map<string, A> vals = 2;
  map<A, int32> keys = 3;
}
`

// Alias-chain ordinals follow RFC-001 §6.4 evaluation order
// (protocompile v0.18.0, #102): the base-most link's rule is ordinal
// 0, derived links follow, field-site uses come last. Each ordinal
// must anchor at the rule's own @-token — base ordinals point into
// the alias declarations, not the field.
func TestRFC001SourceRange_AliasChainOrdinalsBaseFirst(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-alias-chain.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001AliasMapProto)))
	docURI := protocol.DocumentURI(u)

	// Line 6 declares `type B = string @check("base")` (@ at col 16),
	// line 7 `type A = t.B @check("derived")` (@ at col 13), line 10
	// the field-site use (@ at col 10).
	r, ok := srv.RFC001SourceRange(docURI, "t.M.x[t.check#0]")
	require.True(t, ok, "base-most alias rule is ordinal 0")
	assert.Equal(t, rng(6, 16, 6, 17), r, "ordinal 0 anchors at B's @check")

	r, ok = srv.RFC001SourceRange(docURI, "t.M.x[t.check#1]")
	require.True(t, ok, "derived alias rule is ordinal 1")
	assert.Equal(t, rng(7, 13, 7, 14), r, "ordinal 1 anchors at A's @check")

	r, ok = srv.RFC001SourceRange(docURI, "t.M.x[t.check#2]")
	require.True(t, ok, "field-site rule comes after the chain")
	assert.Equal(t, rng(10, 10, 10, 11), r, "ordinal 2 anchors at the field's @check")

	_, ok = srv.RFC001SourceRange(docURI, "t.M.x[t.check#3]")
	assert.False(t, ok, "the chain contributes exactly three rules")

	// The bare-path TYPE_REFINEMENT entry anchors at the field's
	// declared type ident (`A` on line 10, col 2).
	r, ok = srv.RFC001SourceRange(docURI, "t.M.x")
	require.True(t, ok, "bare TYPE_REFINEMENT path resolves")
	assert.Equal(t, rng(10, 2, 10, 3), r)
}

// Map alias rules key per protocompile v0.19.0 (#112): value-type
// alias rules live on the map field's own path — the pre-v0.19
// synthetic `…Entry.value[…]` paths no longer exist — while key-type
// alias rules stay on the synthetic entry's key field.
func TestRFC001SourceRange_MapAliasKeys(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-map-alias.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001AliasMapProto)))
	docURI := protocol.DocumentURI(u)

	// Value alias: chain rules on the map field itself, base first.
	r, ok := srv.RFC001SourceRange(docURI, "t.M.vals[t.check#0]")
	require.True(t, ok, "value-alias rule keys on the map field")
	assert.Equal(t, rng(6, 16, 6, 17), r, "ordinal 0 anchors at B's @check")
	r, ok = srv.RFC001SourceRange(docURI, "t.M.vals[t.check#1]")
	require.True(t, ok)
	assert.Equal(t, rng(7, 13, 7, 14), r, "ordinal 1 anchors at A's @check")

	// The pre-v0.19 emission onto the synthetic entry's value field
	// is gone — a producer still sending old-shape keys gets a miss,
	// never a silent mis-join.
	_, ok = srv.RFC001SourceRange(docURI, "t.M.ValsEntry.value[t.check#0]")
	assert.False(t, ok, "v0.18-era Entry.value paths no longer resolve")

	// Key alias: rules stay on the synthetic entry's key field.
	r, ok = srv.RFC001SourceRange(docURI, "t.M.KeysEntry.key[t.check#0]")
	require.True(t, ok, "key-alias rule stays on the entry's key field")
	assert.Equal(t, rng(6, 16, 6, 17), r)
	_, ok = srv.RFC001SourceRange(docURI, "t.M.keys[t.check#0]")
	assert.False(t, ok, "key-alias rules must not key on the map field")

	// TYPE_REFINEMENT anchors: the value-type ident inside map<K, V>
	// for the value alias (line 11, col 14), the key-type ident for
	// the key alias (line 12, col 6).
	r, ok = srv.RFC001SourceRange(docURI, "t.M.vals")
	require.True(t, ok)
	assert.Equal(t, rng(11, 14, 11, 15), r, "value refinement anchors at the value-type name")
	r, ok = srv.RFC001SourceRange(docURI, "t.M.KeysEntry.key")
	require.True(t, ok)
	assert.Equal(t, rng(12, 6, 12, 7), r, "key refinement anchors at the key-type name")
}

// A classic proto file lowers no RFC-001 constructs: no source map,
// every key misses, and nothing else about the analyze pass changes.
func TestRFC001SourceRange_ClassicProtoHasNoMap(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/classic.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, "syntax = \"proto3\";\n\npackage t;\n\nmessage M {\n  string value = 1;\n}\n")))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, notes[0].params.Diagnostics, "classic file stays clean")

	_, ok := srv.RFC001SourceRange(protocol.DocumentURI(u), "t.M[t.check#0]")
	assert.False(t, ok)
}

// setupRFC001AnnotationImportWorkspace stages an on-disk file
// declaring an annotation; each test case opens an importing file
// with in-memory content and asserts the published diagnostics.
func setupRFC001AnnotationImportWorkspace(t *testing.T) (dir string, srv *Server, fc *fakeConn) {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "buf.yaml"), `version: v2
modules:
  - path: proto
`)
	mustWrite(t, filepath.Join(root, "proto", "acme", "anns.proto"), `syntax = "proto3";

package acme.demo.v1;

annotation description(text: string);
`)

	srv, fc = newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()
	return "file://" + filepath.ToSlash(filepath.Join(root, "proto", "acme")), srv, fc
}

// Annotation validation resolves through imports (#220): a valid
// qualified use of an imported annotation publishes clean, and
// invalid uses diagnose in the importing file with positions there.
func TestDidOpen_AnnotationDiagnostics_CrossFileImport(t *testing.T) {
	const header = `syntax = "proto3";

package acme.app.v1;

import "acme/anns.proto";

`
	cases := []struct {
		name    string
		use     string // line 6 of the document
		wantMsg string // "" → expect no diagnostics at all
		wantRng protocol.Range
	}{
		{
			name: "valid use is clean",
			use:  `@acme.demo.v1.description("user record")`,
		},
		{
			name:    "wrong arg type diagnoses at the argument",
			use:     `@acme.demo.v1.description(42)`,
			wantMsg: "argument \"text\" for `acme.demo.v1.description` expects string, got number literal",
			wantRng: rng(6, 26, 6, 28),
		},
		{
			name:    "missing required arg diagnoses at the use",
			use:     `@acme.demo.v1.description`,
			wantMsg: "missing required argument \"text\" for `acme.demo.v1.description`",
			wantRng: rng(6, 0, 6, 25),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, srv, fc := setupRFC001AnnotationImportWorkspace(t)
			u := dir + "/user.proto"
			src := header + c.use + "\nmessage User {\n  string email = 1;\n}\n"
			require.NoError(t, srv.Handle(context.Background(), noopReply,
				didOpenCall(t, u, 1, src)))

			var diags []protocol.Diagnostic
			for _, n := range fc.calls() {
				if string(n.params.URI) == u {
					diags = n.params.Diagnostics
				}
			}

			if c.wantMsg == "" {
				assert.Empty(t, diags,
					"a valid use of an imported annotation must not squiggle")
				return
			}
			var match *protocol.Diagnostic
			for i := range diags {
				if strings.Contains(diags[i].Message, c.wantMsg) {
					match = &diags[i]
					break
				}
			}
			require.NotNil(t, match, "no diagnostic contains %q; got %+v", c.wantMsg, diags)
			assert.Equal(t, c.wantRng, match.Range)
			assert.Equal(t, "protolsp/proto", match.Source,
				"annotation findings share the proto source so editors group them")
		})
	}
}
