// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Request positions in .proto documents honor the negotiated
// position encoding (the input-side twin of the diagnostic ranges). A cursor
// after a non-ASCII character on the same line converts to a
// different byte offset under utf-8 (character = bytes) than under
// utf-16 (character = code units); these tests pin hover and
// go-to-definition under both negotiated modes.

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
)

// encodingProtoFixture puts five 4-byte / 2-code-unit emoji inside a
// block comment BEFORE a field's type reference. The 10-unit spread
// between the byte column and the UTF-16 column of "Priority"
// exceeds the identifier's length, so a position that lands on the
// ident in the correct encoding lands entirely outside it when
// misinterpreted — each test below fails against a conversion
// hardcoded to the other encoding.
//
// Line 5 (0-indexed): `  /*😀😀😀😀😀*/ Priority priority = 1;`
//
//	bytes:  "Priority" spans columns 27–34
//	utf-16: "Priority" spans columns 17–24
const encodingProtoFixture = `syntax = "proto3";

package acme.demo.v1;

message Greeting {
  /*😀😀😀😀😀*/ Priority priority = 1;
}

// Render priority for greetings.
enum Priority {
  PRIORITY_UNSPECIFIED = 0;
}
`

func TestProtoHover_NonASCIIPrefix_UTF8Encoding(t *testing.T) {
	srv, _, _ := initializeWithPositionEncodings(t, []string{"utf-8"})
	require.Equal(t, positions.UTF8, srv.PositionEncoding())
	const u = "file:///work/enc.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, encodingProtoFixture)))

	// Character 29 is a byte column inside "Priority" (bytes 27–34).
	// A UTF-16 misread lands on the field NAME "priority" instead.
	hover := hoverFromHandle(t, srv, u, 5, 29)
	require.NotNil(t, hover, "utf-8 byte column after emoji must resolve the type ident")
	assert.Contains(t, hover.Contents.Value, "**enum Priority**",
		"cursor is on the type reference, not the field name")
}

func TestProtoHover_NonASCIIPrefix_UTF16Encoding(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	require.Equal(t, positions.UTF16, srv.PositionEncoding())
	const u = "file:///work/enc.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, encodingProtoFixture)))

	// Character 19 is a UTF-16 column inside "Priority" (units
	// 17–24). A byte misread lands mid-emoji and resolves nothing.
	hover := hoverFromHandle(t, srv, u, 5, 19)
	require.NotNil(t, hover, "utf-16 code-unit column after emoji must resolve the type ident")
	assert.Contains(t, hover.Contents.Value, "**enum Priority**")
}

func TestProtoDefinition_NonASCIIPrefix_UTF8Encoding(t *testing.T) {
	srv, _, _ := initializeWithPositionEncodings(t, []string{"utf-8"})
	require.Equal(t, positions.UTF8, srv.PositionEncoding())
	const u = "file:///work/enc.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, encodingProtoFixture)))

	locs := definitionFromHandle(t, srv, u, 5, 29)
	require.Len(t, locs, 1, "utf-8 byte column after emoji must resolve the type ident")
	assert.Equal(t, uint32(9), locs[0].Range.Start.Line,
		"type reference jumps to the enum Priority declaration")
}

func TestProtoDefinition_NonASCIIPrefix_UTF16Encoding(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	require.Equal(t, positions.UTF16, srv.PositionEncoding())
	const u = "file:///work/enc.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, encodingProtoFixture)))

	locs := definitionFromHandle(t, srv, u, 5, 19)
	require.Len(t, locs, 1, "utf-16 code-unit column after emoji must resolve the type ident")
	assert.Equal(t, uint32(9), locs[0].Range.Start.Line,
		"type reference jumps to the enum Priority declaration")
}

// expressionDiagFixture carries a broken CEL expression preceded by
// an emoji block comment on the same line (the output-side
// twin of the request-position tests above). The concatenated
// literal forces the whole-token diagnostic range, whose start is
// the opening quote: byte column 25, UTF-16 column 23.
const expressionDiagFixture = `syntax = "proto3";

message Account {
  double balance = 1 [(buf.validate.field).cel = {
    id: "enc",
    message: "broken",
    /*😀*/ expression: "this " ">= "
  }];
}
`

// celDiagnosticFor plucks the buf/validate/cel diagnostic out of the
// last publish for the URI (the compile pass adds its own findings
// for the unresolved buf.validate extension; those aren't under test).
func celDiagnosticFor(t *testing.T, fc *fakeConn, u string) *protocol.Diagnostic {
	t.Helper()
	notes := fc.calls()
	require.NotEmpty(t, notes, "didOpen must publish diagnostics")
	last := notes[len(notes)-1]
	require.Equal(t, uri.URI(u), last.params.URI)
	for i := range last.params.Diagnostics {
		if last.params.Diagnostics[i].Source == "buf/validate/cel" {
			return &last.params.Diagnostics[i]
		}
	}
	t.Fatalf("no buf/validate/cel diagnostic published: %+v", last.params.Diagnostics)
	return nil
}

func TestProtoDiagnostics_ExpressionRange_NonASCIIPrefix_UTF8Encoding(t *testing.T) {
	srv, fc, _ := initializeWithPositionEncodings(t, []string{"utf-8"})
	require.Equal(t, positions.UTF8, srv.PositionEncoding())
	const u = "file:///work/expr-enc.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, expressionDiagFixture)))

	d := celDiagnosticFor(t, fc, u)
	assert.Equal(t, uint32(6), d.Range.Start.Line)
	assert.Equal(t, uint32(25), d.Range.Start.Character,
		"utf-8 negotiated: diagnostic column counts bytes")
}

func TestProtoDiagnostics_ExpressionRange_NonASCIIPrefix_UTF16Encoding(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	require.Equal(t, positions.UTF16, srv.PositionEncoding())
	const u = "file:///work/expr-enc.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, expressionDiagFixture)))

	d := celDiagnosticFor(t, fc, u)
	assert.Equal(t, uint32(6), d.Range.Start.Line)
	assert.Equal(t, uint32(23), d.Range.Start.Character,
		"utf-16 negotiated: diagnostic column counts code units")
}
