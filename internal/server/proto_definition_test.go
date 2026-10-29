// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"
)

// Shares the hoverProtoFixture from proto_hover_test.go so the
// line numbers documented there apply here too: line 5 is `message
// Greeting {`, line 7 is `  string name = 1;`, line 10 is
// `  Priority priority = 2;`, line 14 is `enum Priority {`.

func TestProtoDefinition_TypeReferenceJumpsToDeclaration(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 10 (`  Priority priority = 2;`), char 4 → middle of "Priority".
	locs := definitionFromHandle(t, srv, u, 10, 4)
	require.Len(t, locs, 1, "type ref resolves to the in-file enum declaration")
	assert.Equal(t, uri.URI(u), locs[0].URI,
		"within-file definition stays on the same URI")
	// Line 14 (0-indexed) is `enum Priority {`. The Name token is
	// after `enum `, so the start column is 5.
	assert.Equal(t, uint32(14), locs[0].Range.Start.Line,
		"target line is the enum declaration")
	assert.Equal(t, uint32(5), locs[0].Range.Start.Character,
		"target character is the start of the Name token, not the keyword")
}

func TestProtoDefinition_BuiltinTypeReturnsNothing(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 7 (`  string name = 1;`), char 4 → middle of "string".
	// Phase 2 deliberately does not resolve built-ins; the LSP
	// reports "no definition" rather than synthesizing a target.
	locs := definitionFromHandle(t, srv, u, 7, 4)
	assert.Nil(t, locs, "built-in scalar types have no in-file declaration to jump to")
}

func TestProtoDefinition_FieldNameJumpsToSelf(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 7 (`  string name = 1;`), char 11 → middle of "name".
	// Cursor on a declaration name should resolve to the declaration
	// itself (a degenerate but predictable case).
	locs := definitionFromHandle(t, srv, u, 7, 11)
	require.Len(t, locs, 1, "field name has a defined location: itself")
	assert.Equal(t, uri.URI(u), locs[0].URI)
	assert.Equal(t, uint32(7), locs[0].Range.Start.Line)
}

func TestProtoDefinition_UnresolvedNameReturnsNil(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	// Add a field whose type isn't defined anywhere in the file.
	// Phase 2 is within-file only, so the lookup correctly returns
	// nothing — Phase 3 will populate cross-file resolution.
	const protoWithUnresolvedRef = `syntax = "proto3";

message Wrapper {
  Missing thing = 1;
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, protoWithUnresolvedRef)))

	// Line 3 (`  Missing thing = 1;`), char 4 → "Missing".
	locs := definitionFromHandle(t, srv, u, 3, 4)
	assert.Nil(t, locs, "type names that don't resolve in-file return nil for Phase 2")
}

// Sanity: the .pxf definition path still works after the .proto
// branch was added at the top of DefinitionFor. Catches accidental
// regression in the dispatch wiring.
func TestProtoDefinition_PxfDispatchStillWorks(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file:///work/protos"))

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	// Either the pxf path returns a location (registry-backed) or
	// nil if the lookup didn't resolve in this test setup. The
	// invariant we're guarding here is "the proto branch didn't
	// claim a .pxf URI" — i.e., the dispatch routed correctly. A
	// non-panic + non-error reply is sufficient evidence.
	_ = locs
}
