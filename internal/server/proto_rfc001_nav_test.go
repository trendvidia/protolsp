// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// RFC-001 completion + navigation: the `type` /
// `function` / `annotation` keywords in file-scope completion,
// declared annotation names after `@`, and hover / go-to-definition
// wiring from `@name` use sites to their `annotation` declarations.

package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// File-scope completion must offer the RFC-001 declaration keywords
// alongside the classic top-level set.
func TestProtoCompletion_FileScope_RFC001Keywords(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-keywords.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, "syntax = \"proto3\";\n\n")))

	list := completionFromHandle(t, srv, u, 1, 0)
	require.NotNil(t, list, "file-scope completion returns a list")
	containsAll(t, labelsFromList(list), []string{"type", "function", "annotation"},
		"file scope offers the RFC-001 declaration keywords")
}

// annotationCompletionFixture declares two annotations and parks the
// cursor line on a bare `@` — the moment annotation-use completion
// should fire. The trailing `message M {}` keeps the parse realistic
// (the parser tries to bind the `@` to it).
const annotationCompletionFixture = `syntax = "proto3";

package test.rfc001.nav;

annotation required;
annotation description(text: string);

@
message M {}
`

// A cursor right after `@` offers exactly the declared annotation
// names — not scope keywords, not type names.
func TestProtoCompletion_AnnotationUse_InFileDecls(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/ann-completion.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, annotationCompletionFixture)))

	// Line 7 is the bare `@`; cursor sits right after the sigil.
	list := completionFromHandle(t, srv, u, 7, 1)
	require.NotNil(t, list, "@ completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"required", "description"},
		"@ offers the annotations declared in this file")
	containsNone(t, labels, []string{"message", "option", "string", "int32"},
		"@ suppresses keywords and type names")
}

// setupRFC001CrossFileWorkspace stages a two-file workspace where
// bar.proto declares an annotation and foo.proto imports it and uses
// it package-qualified. Mirrors setupCrossFileWorkspace's layout.
func setupRFC001CrossFileWorkspace(t *testing.T) (fooPath, barPath string, srv *Server) {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "buf.yaml"), `version: v2
modules:
  - path: proto
`)
	barPath = filepath.Join(root, "proto", "acme", "bar.proto")
	mustWrite(t, barPath, `syntax = "proto3";

package acme.demo.v1;

// Marks values audited by compliance.
annotation checked(by: string);

// Stable user identifier.
type UserID = string;

function is_uuid(s: string);
`)
	fooPath = filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@acme.demo.v1.checked(acme.demo.v1.is_uuid)
message Foo {
  string id = 1;
  acme.demo.v1.UserID uid = 2;
}
`)

	srv, _ = newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	return fooPath, barPath, srv
}

// A bare `@` in a file that imports an annotation-declaring file
// offers the imported name package-qualified; once the user has
// typed the qualifier (`@acme.demo.v1.`), the bare name completes.
func TestProtoCompletion_AnnotationUse_ImportedDecls(t *testing.T) {
	fooPath, _, srv := setupRFC001CrossFileWorkspace(t)
	dir := "file://" + filepath.ToSlash(filepath.Dir(fooPath))

	unqualified := dir + "/scratch-unqualified.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, unqualified, 1, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@
message M {}
`)))
	list := completionFromHandle(t, srv, unqualified, 6, 1)
	require.NotNil(t, list, "@ completion returns a list")
	containsAll(t, labelsFromList(list), []string{"acme.demo.v1.checked"},
		"bare @ offers imported annotations package-qualified")

	qualified := dir + "/scratch-qualified.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, qualified, 1, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@acme.demo.v1.
message M {}
`)))
	list = completionFromHandle(t, srv, qualified, 6, 14)
	require.NotNil(t, list, "@<package>. completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"checked"},
		"a typed qualifier completes the bare imported names")
	containsNone(t, labels, []string{"acme.demo.v1.checked"},
		"the already-typed qualifier is not repeated in labels")
}

// Hover on an annotation use site renders the declaration card, from
// the leading use on a message, the trailing use on a field, and the
// `@` sigil itself. rfc001Proto (proto_rfc001_test.go) declares
// `annotation description(text: string);` on line 5 and uses it on
// lines 7 and 9.
func TestProtoHover_AnnotationUseSite(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-hover.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001Proto)))

	positions := []struct {
		name       string
		line, char uint32
	}{
		{"leading use name on message", 7, 3},
		{"@ sigil of leading use", 7, 0},
		{"trailing use name on field", 9, 22},
	}
	for _, p := range positions {
		t.Run(p.name, func(t *testing.T) {
			hover := hoverFromHandle(t, srv, u, p.line, p.char)
			require.NotNil(t, hover, "use site yields a hover")
			assert.Contains(t, hover.Contents.Value, "**annotation description(text: string)**",
				"card headline carries the declaration signature")
			assert.Contains(t, hover.Contents.Value, "Defined in",
				"card carries the provenance line")
		})
	}
}

// Hover on the annotation declaration's own name renders the same
// card, with the leading doc comment attached.
func TestProtoHover_AnnotationDeclarationName(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-decl-hover.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, `syntax = "proto3";

package test.rfc001.nav;

// Attaches reviewer provenance.
annotation reviewed(by: string);
`)))

	// Line 5 col 13 is inside the `reviewed` name ident.
	hover := hoverFromHandle(t, srv, u, 5, 13)
	require.NotNil(t, hover, "declaration name yields a hover")
	assert.Contains(t, hover.Contents.Value, "**annotation reviewed(by: string)**")
	assert.Contains(t, hover.Contents.Value, "Attaches reviewer provenance.",
		"the leading doc comment flows into the card")
}

// Go-to-definition from a use site jumps to the declaration's name
// token; from the declaration's own name it stays put.
func TestProtoDefinition_AnnotationUse_InFile(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/rfc001-def.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001Proto)))

	// `description` in `annotation description(text: string);`
	// occupies line 5 chars 11–22.
	wantRange := rng(5, 11, 5, 22)

	positions := []struct {
		name       string
		line, char uint32
	}{
		{"leading use name on message", 7, 3},
		{"@ sigil of leading use", 7, 0},
		{"trailing use name on field", 9, 22},
		{"declaration name jumps to itself", 5, 13},
	}
	for _, p := range positions {
		t.Run(p.name, func(t *testing.T) {
			locs := definitionFromHandle(t, srv, u, p.line, p.char)
			require.Len(t, locs, 1, "definition resolves to one location")
			assert.Equal(t, u, string(locs[0].URI))
			assert.Equal(t, wantRange, locs[0].Range,
				"definition lands on the declaration's name token")
		})
	}
}

// Hover + definition resolve a package-qualified use through the
// import to the declaring file.
func TestProtoNavigation_AnnotationUse_CrossFile(t *testing.T) {
	fooPath, barPath, srv := setupRFC001CrossFileWorkspace(t)
	fooURI := "file://" + filepath.ToSlash(fooPath)

	// Line 6 of foo.proto is `@acme.demo.v1.checked("sec team")`;
	// char 16 sits inside `checked`.
	hover := hoverFromHandle(t, srv, fooURI, 6, 16)
	require.NotNil(t, hover, "qualified use resolves cross-file via import")
	assert.Contains(t, hover.Contents.Value, "**annotation checked(by: string)**")
	assert.Contains(t, hover.Contents.Value, "Marks values audited by compliance.",
		"the imported file's leading comment flows through")

	locs := definitionFromHandle(t, srv, fooURI, 6, 16)
	require.Len(t, locs, 1, "definition resolves to one location")
	assert.Equal(t, "file://"+filepath.ToSlash(barPath), string(locs[0].URI),
		"definition jumps into the imported file")
	// `checked` in `annotation checked(by: string);` occupies
	// line 5 chars 11–18 of bar.proto.
	assert.Equal(t, rng(5, 11, 5, 18), locs[0].Range)
}

// Bare annotation/function references don't leak across packages
// (the RFC-001 twin of strict scoping): a bare `@audited` written in
// acme.app.v1 must NOT bind an annotation declared in an
// other.lib.v1 import — protoc-style scoping can't reach it.
func TestProtoNavigation_RFC001BareCrossPackage_DoesNotResolve(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(root, "proto", "other", "bar.proto"), `syntax = "proto3";

package other.lib.v1;

annotation audited(by: string);

function is_uuid(s: string);
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.app.v1;

import "other/bar.proto";

@audited(is_uuid)
message Foo {
  string id = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 6: `@audited(is_uuid)` — char 2 sits in "audited",
	// char 10 in "is_uuid".
	assert.Nil(t, hoverFromHandle(t, srv, fooURI, 6, 2),
		"bare annotation use doesn't bind across packages")
	assert.Empty(t, definitionFromHandle(t, srv, fooURI, 6, 2),
		"no definition for a bare cross-package annotation use")
	assert.Nil(t, hoverFromHandle(t, srv, fooURI, 6, 10),
		"bare function reference doesn't bind across packages")
	assert.Empty(t, definitionFromHandle(t, srv, fooURI, 6, 10),
		"no definition for a bare cross-package function reference")
}

// The protoc-accepted twin: bare annotation/function references bind
// declarations in ANOTHER file of the SAME package — covered by the
// scope chain, no leniency involved.
func TestProtoNavigation_RFC001BareSamePackageCrossFile_Resolves(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	siblingPath := filepath.Join(root, "proto", "acme", "sibling.proto")
	mustWrite(t, siblingPath, `syntax = "proto3";

package acme.app.v1;

// Compliance marker.
annotation audited(by: string);

function is_uuid(s: string);
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.app.v1;

import "acme/sibling.proto";

@audited(is_uuid)
message Foo {
  string id = 1;
}
`)
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	siblingURI := "file://" + filepath.ToSlash(siblingPath)

	hover := hoverFromHandle(t, srv, fooURI, 6, 2)
	require.NotNil(t, hover, "same-package bare annotation use resolves")
	assert.Contains(t, hover.Contents.Value, "**annotation audited(by: string)**")
	assert.Contains(t, hover.Contents.Value, "Compliance marker.")

	locs := definitionFromHandle(t, srv, fooURI, 6, 2)
	require.Len(t, locs, 1)
	assert.Equal(t, siblingURI, string(locs[0].URI))
	// `audited` in `annotation audited(by: string);` — line 5.
	assert.Equal(t, rng(5, 11, 5, 18), locs[0].Range)

	hover = hoverFromHandle(t, srv, fooURI, 6, 10)
	require.NotNil(t, hover, "same-package bare function reference resolves")
	assert.Contains(t, hover.Contents.Value, "**function is_uuid(s: string)**")
	locs = definitionFromHandle(t, srv, fooURI, 6, 10)
	require.Len(t, locs, 1)
	assert.Equal(t, siblingURI, string(locs[0].URI))
}

// A qualifier also binds under enclosing-scope interpretations
// : `@demo.v1.checked` written in acme.app.v1 names
// acme.demo.v1.checked via the shared `acme` scope — protoc's rule.
func TestProtoNavigation_RFC001ScopeRelativeQualifier_Resolves(t *testing.T) {
	fooPath, barPath, srv := setupRFC001CrossFileWorkspace(t)
	dir := "file://" + filepath.ToSlash(filepath.Dir(fooPath))

	scratch := dir + "/scratch-relative.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, scratch, 1, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@demo.v1.checked(demo.v1.is_uuid)
message M {}
`)))

	// Line 6: `@demo.v1.checked(demo.v1.is_uuid)` — char 10 sits in
	// "checked", char 27 in "is_uuid".
	hover := hoverFromHandle(t, srv, scratch, 6, 10)
	require.NotNil(t, hover, "scope-relative annotation qualifier resolves")
	assert.Contains(t, hover.Contents.Value, "**annotation checked(by: string)**")

	locs := definitionFromHandle(t, srv, scratch, 6, 10)
	require.Len(t, locs, 1)
	assert.Equal(t, "file://"+filepath.ToSlash(barPath), string(locs[0].URI))

	hover = hoverFromHandle(t, srv, scratch, 6, 27)
	require.NotNil(t, hover, "scope-relative function qualifier resolves")
	assert.Contains(t, hover.Contents.Value, "**function is_uuid(s: string)**")
}

// `@` completion honors scope-relative package qualifiers too
// : `@demo.v1.` written in acme.app.v1 completes the
// annotations of acme.demo.v1.
func TestProtoCompletion_AnnotationUse_ScopeRelativeQualifier(t *testing.T) {
	fooPath, _, srv := setupRFC001CrossFileWorkspace(t)
	dir := "file://" + filepath.ToSlash(filepath.Dir(fooPath))

	scratch := dir + "/scratch-relative-completion.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, scratch, 1, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@demo.v1.
message M {}
`)))

	// Line 6: `@demo.v1.` — char 9 sits right after the second dot.
	list := completionFromHandle(t, srv, scratch, 6, 9)
	require.NotNil(t, list, "@<scope-relative package>. completion returns a list")
	containsAll(t, labelsFromList(list), []string{"checked"},
		"the scope-relative qualifier completes the package's annotations")
}

// ─── Phase C: type / function navigation ──────────────────

// typeFuncFixture exercises every RFC-001 type/function reference
// site Phase C wires: alias-as-field-type, alias value chains,
// annotation parameter types, and function references from parsed
// annotation arguments.
const typeFuncFixture = `syntax = "proto3";

package test.rfc001.types;

// A validated email address.
type Email = string;

// Preferred contact address.
type PrimaryEmail = Email;

// True when s parses as RFC-5322.
function is_email(s: string);

annotation check(fn: string);
annotation typed(m: Money);

message Money {
  string currency = 1;
}

@check(is_email)
message Contact {
  PrimaryEmail address = 1;
}
`

func TestProtoHover_RFC001TypeFunctionSites_InFile(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/typefunc.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, typeFuncFixture)))

	cases := []struct {
		name       string
		line, char uint32
		want       []string
	}{
		{"alias value referencing another alias", 8, 21,
			[]string{"**type Email = string**", "A validated email address."}},
		{"field type referencing an alias", 22, 4,
			[]string{"**type PrimaryEmail = Email**", "Preferred contact address."}},
		{"function ref in annotation arg", 20, 9,
			[]string{"**function is_email(s: string)**", "True when s parses as RFC-5322."}},
		{"annotation param type referencing a message", 14, 21,
			[]string{"**message Money**"}},
		{"type declaration's own name", 5, 7,
			[]string{"**type Email = string**", "A validated email address."}},
		{"function declaration's own name", 11, 12,
			[]string{"**function is_email(s: string)**"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hover := hoverFromHandle(t, srv, u, c.line, c.char)
			require.NotNil(t, hover, "position yields a hover")
			for _, w := range c.want {
				assert.Contains(t, hover.Contents.Value, w)
			}
		})
	}

	// Builtin scalar in a function param type stays hover-silent,
	// matching field-type behavior.
	assert.Nil(t, hoverFromHandle(t, srv, u, 11, 23),
		"builtin scalar param type yields no hover")
}

func TestProtoDefinition_RFC001TypeFunctionSites_InFile(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/typefunc-def.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, typeFuncFixture)))

	cases := []struct {
		name       string
		line, char uint32
		want       protocol.Range
	}{
		{"alias value jumps to the aliased type decl", 8, 21, rng(5, 5, 5, 10)},
		{"field type jumps to the alias decl", 22, 4, rng(8, 5, 8, 17)},
		{"function ref jumps to the function decl", 20, 9, rng(11, 9, 11, 17)},
		{"annotation param type jumps to the message", 14, 21, rng(16, 8, 16, 13)},
		{"type decl name jumps to itself", 5, 7, rng(5, 5, 5, 10)},
		{"function decl name jumps to itself", 11, 12, rng(11, 9, 11, 17)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			locs := definitionFromHandle(t, srv, u, c.line, c.char)
			require.Len(t, locs, 1, "definition resolves to one location")
			assert.Equal(t, u, string(locs[0].URI))
			assert.Equal(t, c.want, locs[0].Range)
		})
	}
}

// Package-qualified alias + function references resolve through the
// import to the declaring file.
func TestProtoNavigation_RFC001TypeFunction_CrossFile(t *testing.T) {
	fooPath, barPath, srv := setupRFC001CrossFileWorkspace(t)
	fooURI := "file://" + filepath.ToSlash(fooPath)
	barURI := "file://" + filepath.ToSlash(barPath)

	// Line 9 of foo.proto is `  acme.demo.v1.UserID uid = 2;`;
	// char 17 sits inside `UserID`.
	hover := hoverFromHandle(t, srv, fooURI, 9, 17)
	require.NotNil(t, hover, "qualified alias field type resolves cross-file")
	assert.Contains(t, hover.Contents.Value, "**type UserID = string**")
	assert.Contains(t, hover.Contents.Value, "Stable user identifier.")

	locs := definitionFromHandle(t, srv, fooURI, 9, 17)
	require.Len(t, locs, 1)
	assert.Equal(t, barURI, string(locs[0].URI))
	// `UserID` in `type UserID = string;` on line 8 of bar.proto.
	assert.Equal(t, rng(8, 5, 8, 11), locs[0].Range)

	// Line 6 char 37 sits inside `is_uuid` of the qualified function
	// reference in the annotation argument.
	hover = hoverFromHandle(t, srv, fooURI, 6, 37)
	require.NotNil(t, hover, "qualified function ref resolves cross-file")
	assert.Contains(t, hover.Contents.Value, "**function is_uuid(s: string)**")

	locs = definitionFromHandle(t, srv, fooURI, 6, 37)
	require.Len(t, locs, 1)
	assert.Equal(t, barURI, string(locs[0].URI))
	// `is_uuid` in `function is_uuid(s: string);` on line 10.
	assert.Equal(t, rng(10, 9, 10, 16), locs[0].Range)
}
