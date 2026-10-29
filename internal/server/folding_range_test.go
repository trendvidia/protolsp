// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// foldingRangesFromHandle drives the foldingRange dispatcher through
// jsonrpc2 and round-trips the response through JSON so tests observe
// the same shape a real client would.
func foldingRangesFromHandle(t *testing.T, srv *Server, u string) []protocol.FoldingRange {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentFoldingRange,
		&protocol.FoldingRangeParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
			},
		})
	require.NoError(t, err)

	var captured []protocol.FoldingRange
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &captured))
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return captured
}

// hasFold reports whether ranges contains a fold with the given
// start/end lines and kind.
func hasFold(ranges []protocol.FoldingRange, start, end uint32, kind protocol.FoldingRangeKind) bool {
	for _, r := range ranges {
		if r.StartLine == start && r.EndLine == end && r.Kind == kind {
			return true
		}
	}
	return false
}

// pxfFolding lays out nested blocks, a `state` block, and a single-line
// inline block (which must NOT fold). Line numbers (0-indexed):
//
//	 0 @type acme.demo.v1.Greeting
//	 1
//	 2 children {
//	 3   first {
//	 4     name = "a"
//	 5   }
//	 6   second {
//	 7     name = "b"
//	 8   }
//	 9 }
//	10
//	11 state {
//	12   ready = true
//	13 }
//	14
//	15 inline = { a = 1 }
const pxfFolding = `@type acme.demo.v1.Greeting

children {
  first {
    name = "a"
  }
  second {
    name = "b"
  }
}

state {
  ready = true
}

inline = { a = 1 }
`

func TestFoldingRange_PxfBlocks(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/f.pxf", 1, pxfFolding)))

	ranges := foldingRangesFromHandle(t, srv, "file:///work/f.pxf")
	require.NotEmpty(t, ranges)

	assert.True(t, hasFold(ranges, 2, 9, ""), "children block folds 2..9: %+v", ranges)
	assert.True(t, hasFold(ranges, 3, 5, ""), "nested first block folds 3..5: %+v", ranges)
	assert.True(t, hasFold(ranges, 6, 8, ""), "nested second block folds 6..8: %+v", ranges)
	assert.True(t, hasFold(ranges, 11, 13, ""), "state block folds 11..13: %+v", ranges)

	// The single-line `inline = { a = 1 }` (line 15) is not foldable.
	for _, r := range ranges {
		assert.NotEqual(t, uint32(15), r.StartLine, "single-line inline block must not fold: %+v", r)
	}
}

// pxfDirectiveFolding places a multi-line directive block ahead of the
// body. Line numbers (0-indexed):
//
//	0 @header chameleon.v1.LayerHeader {
//	1   id = "x"
//	2 }
//	3 @type acme.demo.v1.Greeting
//	4
//	5 name = "world"
const pxfDirectiveFolding = `@header chameleon.v1.LayerHeader {
  id = "x"
}
@type acme.demo.v1.Greeting

name = "world"
`

func TestFoldingRange_PxfMultiLineDirective(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/d.pxf", 1, pxfDirectiveFolding)))

	ranges := foldingRangesFromHandle(t, srv, "file:///work/d.pxf")
	assert.True(t, hasFold(ranges, 0, 2, ""), "multi-line @header directive folds 0..2: %+v", ranges)
}

func TestFoldingRange_ReturnsNilWhenDocumentNotOpen(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	ranges := foldingRangesFromHandle(t, srv, "file:///work/missing.pxf")
	assert.Nil(t, ranges, "no open document → no folds")
}

// protoFolding exercises every proto block kind, the import group, and a
// multi-line comment block. Line numbers (0-indexed):
//
//	 0 syntax = "proto3";
//	 1
//	 2 package acme.demo.v1;
//	 3
//	 4 import "google/protobuf/timestamp.proto";
//	 5 import "google/protobuf/duration.proto";
//	 6
//	 7 // A greeting message.
//	 8 // Second line of the doc comment.
//	 9 message Greeting {
//	10   string name = 1;
//	11   oneof kind {
//	12     string a = 2;
//	13     int32 b = 3;
//	14   }
//	15 }
//	16
//	17 enum Color {
//	18   RED = 0;
//	19   GREEN = 1;
//	20 }
//	21
//	22 service Greeter {
//	23   rpc Hello(Greeting) returns (Greeting);
//	24 }
const protoFolding = `syntax = "proto3";

package acme.demo.v1;

import "google/protobuf/timestamp.proto";
import "google/protobuf/duration.proto";

// A greeting message.
// Second line of the doc comment.
message Greeting {
  string name = 1;
  oneof kind {
    string a = 2;
    int32 b = 3;
  }
}

enum Color {
  RED = 0;
  GREEN = 1;
}

service Greeter {
  rpc Hello(Greeting) returns (Greeting);
}
`

func TestFoldingRange_ProtoBlocks(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/greet.proto", 1, protoFolding)))

	ranges := foldingRangesFromHandle(t, srv, "file:///work/greet.proto")
	require.NotEmpty(t, ranges)

	assert.True(t, hasFold(ranges, 4, 5, protocol.ImportsFoldingRange), "import group folds 4..5: %+v", ranges)
	assert.True(t, hasFold(ranges, 7, 8, protocol.CommentFoldingRange), "doc comment block folds 7..8: %+v", ranges)
	assert.True(t, hasFold(ranges, 9, 15, ""), "message body folds 9..15: %+v", ranges)
	assert.True(t, hasFold(ranges, 11, 14, ""), "oneof body folds 11..14: %+v", ranges)
	assert.True(t, hasFold(ranges, 17, 20, ""), "enum body folds 17..20: %+v", ranges)
	assert.True(t, hasFold(ranges, 22, 24, ""), "service body folds 22..24: %+v", ranges)

	// A bodyless rpc (`rpc Hello(...) returns (...);`) has nothing to
	// fold on line 23.
	for _, r := range ranges {
		if r.Kind == "" {
			assert.NotEqual(t, uint32(23), r.StartLine, "bodyless rpc must not fold: %+v", r)
		}
	}
}

func TestFoldingRange_ProtoSingleImportNotFolded(t *testing.T) {
	const src = `syntax = "proto3";

import "google/protobuf/timestamp.proto";

message M {
  string name = 1;
}
`
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/one.proto", 1, src)))

	ranges := foldingRangesFromHandle(t, srv, "file:///work/one.proto")
	for _, r := range ranges {
		assert.NotEqual(t, protocol.ImportsFoldingRange, r.Kind,
			"a lone import is not a foldable group: %+v", r)
	}
}
