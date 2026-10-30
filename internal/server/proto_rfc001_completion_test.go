// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// RFC-001 completion parity for `type` aliases and `function`
// declarations: aliases join the field-type candidate set
// (bare in-file, package-qualified when imported, bare after a typed
// package qualifier), function/annotation parameter-type slots get
// the same type universe a field type does, and a cursor inside an
// annotation use's argument parens offers declared function names.

package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Message-scope completion offers in-file `type` aliases by bare
// name, alongside the messages + enums it already carried.
func TestProtoCompletion_MessageScope_TypeAliases(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/alias-completion.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, typeFuncFixture)))

	// Line 22 is `  PrimaryEmail address = 1;` inside message Contact.
	list := completionFromHandle(t, srv, u, 22, 0)
	require.NotNil(t, list, "message scope completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"Email", "PrimaryEmail", "Money"},
		"message scope offers in-file type aliases bare, next to messages")
}

// Message-scope completion qualifies imported aliases with their
// package, same posture as imported messages; dot completion after
// the typed qualifier then offers the bare name.
func TestProtoCompletion_ImportedTypeAliases(t *testing.T) {
	fooPath, _, srv := setupRFC001CrossFileWorkspace(t)
	fooURI := "file://" + filepath.ToSlash(fooPath)

	// Line 7 is `  string id = 1;` inside message Foo — message scope.
	list := completionFromHandle(t, srv, fooURI, 7, 0)
	require.NotNil(t, list, "message scope completion returns a list")
	containsAll(t, labelsFromList(list), []string{"acme.demo.v1.UserID"},
		"imported aliases are offered package-qualified")

	// Line 9 is `  acme.demo.v1.UserID uid = 2;`; char 15 sits one
	// past the last `.` — dot completion with qualifier acme.demo.v1.
	list = completionFromHandle(t, srv, fooURI, 9, 15)
	require.NotNil(t, list, "dot completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"UserID"},
		"a typed package qualifier completes imported aliases bare")
	containsNone(t, labels, []string{"acme.demo.v1.UserID"},
		"the already-typed qualifier is not repeated in labels")
}

// A cursor in the parameter-type slot of a `function` or `annotation`
// declaration offers the field-type universe — scalars, in-file
// types, aliases — and none of the file-scope keywords the position
// used to fall back to.
func TestProtoCompletion_RFC001ParamTypes(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/param-completion.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, typeFuncFixture)))

	positions := []struct {
		name       string
		line, char uint32
	}{
		// Line 11 `function is_email(s: string);`, char 23 in `string`.
		{"function param type", 11, 23},
		// Line 14 `annotation typed(m: Money);`, char 22 in `Money`.
		{"annotation param type", 14, 22},
	}
	for _, p := range positions {
		t.Run(p.name, func(t *testing.T) {
			list := completionFromHandle(t, srv, u, p.line, p.char)
			require.NotNil(t, list, "param-type completion returns a list")
			labels := labelsFromList(list)
			containsAll(t, labels, []string{"string", "int32", "Money", "Email", "PrimaryEmail"},
				"param types offer scalars + in-file types + aliases")
			containsNone(t, labels, []string{"message", "option", "syntax", "import", "rpc"},
				"param types suppress declaration keywords")
		})
	}
}

// The mid-edit shape — a param whose type hasn't been typed yet —
// still classifies as a param-type position via the declaration's
// parens span.
func TestProtoCompletion_RFC001ParamTypes_EmptySlot(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/param-midedit.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, `syntax = "proto3";

package test.rfc001.params;

type Email = string;

function f(s: );
`)))

	// Line 6 `function f(s: );`, char 14 — after the `: `, before `)`.
	list := completionFromHandle(t, srv, u, 6, 14)
	require.NotNil(t, list, "empty param-type slot returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"string", "Email"},
		"empty param-type slot offers scalars + aliases")
	containsNone(t, labels, []string{"message", "syntax"},
		"empty param-type slot suppresses keywords")
}

// A cursor inside an annotation use's argument parens offers the
// declared function names — and only those, not the scope fallback.
func TestProtoCompletion_AnnotationArgs_InFileFunctions(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/arg-completion.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, typeFuncFixture)))

	// Line 20 `@check(is_email)`, char 9 inside `is_email`.
	list := completionFromHandle(t, srv, u, 20, 9)
	require.NotNil(t, list, "annotation-arg completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"is_email"},
		"annotation args offer the declared function names")
	containsNone(t, labels, []string{"message", "string", "Email", "check"},
		"annotation args suppress keywords, types, and annotation names")
}

// Empty parens — the moment right after typing `@check(` — offer the
// same function candidates.
func TestProtoCompletion_AnnotationArgs_EmptyParens(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/arg-empty.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, `syntax = "proto3";

package test.rfc001.args;

function is_email(s: string);
function is_uuid(s: string);

annotation check(fn: string);

@check()
message M {}
`)))

	// Line 9 `@check()`, char 7 — between the parens.
	list := completionFromHandle(t, srv, u, 9, 7)
	require.NotNil(t, list, "empty-parens completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"is_email", "is_uuid"},
		"@name( offers every declared function")
	containsNone(t, labels, []string{"message", "string"},
		"@name( suppresses the scope fallback")
}

// Imported functions complete package-qualified from bare argument
// positions, and bare once the qualifier has been typed — mirroring
// `@` completion for annotation names.
func TestProtoCompletion_AnnotationArgs_ImportedFunctions(t *testing.T) {
	fooPath, _, srv := setupRFC001CrossFileWorkspace(t)
	dir := "file://" + filepath.ToSlash(filepath.Dir(fooPath))

	unqualified := dir + "/scratch-arg-unqualified.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, unqualified, 1, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@acme.demo.v1.checked()
message M {}
`)))
	// Line 6 `@acme.demo.v1.checked()`, char 22 — between the parens.
	list := completionFromHandle(t, srv, unqualified, 6, 22)
	require.NotNil(t, list, "annotation-arg completion returns a list")
	containsAll(t, labelsFromList(list), []string{"acme.demo.v1.is_uuid"},
		"bare argument positions offer imported functions package-qualified")

	qualified := dir + "/scratch-arg-qualified.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, qualified, 1, `syntax = "proto3";

package acme.app.v1;

import "acme/bar.proto";

@acme.demo.v1.checked(acme.demo.v1.)
message M {}
`)))
	// Line 6, char 35 — right after the argument's trailing dot.
	list = completionFromHandle(t, srv, qualified, 6, 35)
	require.NotNil(t, list, "qualified-arg completion returns a list")
	labels := labelsFromList(list)
	containsAll(t, labels, []string{"is_uuid"},
		"a typed qualifier completes imported functions bare")
	containsNone(t, labels, []string{"acme.demo.v1.is_uuid"},
		"the already-typed qualifier is not repeated in labels")
}
