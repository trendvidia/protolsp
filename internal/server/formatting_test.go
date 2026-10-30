// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// formattingFromHandle drives the formatting dispatcher and returns
// the decoded TextEdit slice (nil = "no edits available").
func formattingFromHandle(t *testing.T, srv *Server, u string) []protocol.TextEdit {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentFormatting,
		&protocol.DocumentFormattingParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
			Options:      protocol.FormattingOptions{InsertSpaces: true, TabSize: 2},
		})
	require.NoError(t, err)

	var captured []protocol.TextEdit
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := testCodec.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, testCodec.Unmarshal(b, &captured))
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return captured
}

func TestFormatting_AlreadyCanonicalReturnsEmptySlice(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	edits := formattingFromHandle(t, srv, "file:///work/g.pxf")
	require.NotNil(t, edits, "canonical input → non-nil slice (zero edits)")
	assert.Empty(t, edits, "canonical input should produce no edits")
}

func TestFormatting_RewritesNonCanonicalSource(t *testing.T) {
	// Excessive blank lines + non-standard spacing around `=` —
	// protowire-go's FormatDocument re-emits canonical layout.
	const messy = "@type acme.demo.v1.Greeting\n\n\n\nname    =     \"world\"\n"
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/m.pxf", 1, messy)))

	edits := formattingFromHandle(t, srv, "file:///work/m.pxf")
	require.Len(t, edits, 1, "non-canonical input should produce one whole-document edit")
	assert.Equal(t, uint32(0), edits[0].Range.Start.Line)
	assert.Equal(t, uint32(0), edits[0].Range.Start.Character)
	assert.NotEqual(t, messy, edits[0].NewText, "edit should rewrite the source")
	// Sanity-check the canonical form: single space around `=`, no
	// run of blank lines between @type and the first field.
	assert.Contains(t, edits[0].NewText, `name = "world"`)
	assert.NotContains(t, edits[0].NewText, "\n\n\n\n")
}

func TestFormatting_ReturnsNilWhenDocumentNotOpen(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	edits := formattingFromHandle(t, srv, "file:///work/missing.pxf")
	assert.Nil(t, edits)
}

func TestFormatting_ReturnsNilOnParseError(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	// Parse error: assignment with no key.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/bad.pxf", 1, "= 1\n")))

	edits := formattingFromHandle(t, srv, "file:///work/bad.pxf")
	assert.Nil(t, edits, "broken parse → no edits (don't clobber mid-edit buffer)")
}

// ---------------------------------------------------------------------------
// Proto formatting — protocompile's printer, Legacy (buf-compatible) preset
// ---------------------------------------------------------------------------

// formatProtoDoc opens src as a .proto document and returns the
// formatting result.
func formatProtoDoc(t *testing.T, src string) []protocol.TextEdit {
	t.Helper()
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/foo.proto"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))
	return formattingFromHandle(t, srv, u)
}

// TestProtoFormatting_MatchesBufFormat pins parity with `buf format`
// on a file with no v1.2 syntax. want is buf 1.74.0's output for src
// verbatim, so a printer change that moves away from buf's layout
// fails here rather than as churn in users' files.
func TestProtoFormatting_MatchesBufFormat(t *testing.T) {
	const src = `syntax="proto3";
package   demo.v1;
import "google/protobuf/timestamp.proto";
option go_package="example.com/demo/v1;demov1";

// Order is one order.
message Order{
  string id=1;   // trailing
    google.protobuf.Timestamp created_at = 2 [deprecated=true];
  repeated  string tags=3;
  enum Status{STATUS_UNSPECIFIED=0;STATUS_OPEN=1;}
  Status status=4;
}
service Orders{rpc Get(Order)returns(Order);}
`
	const want = `syntax = "proto3";
package demo.v1;

import "google/protobuf/timestamp.proto";

option go_package = "example.com/demo/v1;demov1";

// Order is one order.
message Order {
  string id = 1; // trailing
  google.protobuf.Timestamp created_at = 2 [deprecated = true];
  repeated string tags = 3;
  enum Status {
    STATUS_UNSPECIFIED = 0;
    STATUS_OPEN = 1;
  }
  Status status = 4;
}
service Orders {
  rpc Get(Order) returns (Order);
}
`
	edits := formatProtoDoc(t, src)
	require.Len(t, edits, 1, "non-canonical source produces a single whole-doc edit")
	assert.Equal(t, want, edits[0].NewText)
	assert.Equal(t, uint32(0), edits[0].Range.Start.Line)
	assert.Equal(t, uint32(0), edits[0].Range.Start.Character)
	assert.Equal(t, uint32(14), edits[0].Range.End.Line, "range ends past the last line")

	assert.Empty(t, formatProtoDoc(t, want), "buf's own output is already canonical")
}

// TestProtoFormatting_KeepsV12Annotations is the case buf could not
// handle (#9): a file using @validate, @required and a function
// declaration. Formatting must return edits, and keep every
// annotation use and every comment.
func TestProtoFormatting_KeepsV12Annotations(t *testing.T) {
	const src = `syntax = "proto3";
package daysong.v1alpha1;

// TempoRange min <= max.
function tempo_ordered(t: TempoRange);

message StyleDescriptor {
  // 0.0-1.0.
  float energy = 1   @validate(this >= 0.0 && this <= 1.0, code = "descriptor.energy.range", message = "energy must be within [0, 1]");
  TempoRange tempo = 3 @required;
  Mode mode = 5 @required @validate(this in [1, 2], code = "descriptor.mode.known", message = "mode must be match or regulate");
  enum Mode{MODE_UNSPECIFIED=0;MODE_MATCH=1;MODE_REGULATE=2;}
}

// Beats per minute, 40-220, min <= max.
@validate(tempo_ordered(this), code = "tempo.min_le_max", message = "min_bpm must not exceed max_bpm")
message TempoRange {
  int32 min_bpm = 1 @validate(this >= 40 && this <= 220, code = "tempo.min.range", message = "min_bpm out of range");
  int32 max_bpm = 2;
}
`
	// Long annotation uses wrap at 100 columns, one argument per
	// line (protocompile#281); short ones stay on the field's line.
	const want = `syntax = "proto3";
package daysong.v1alpha1;

// TempoRange min <= max.
function tempo_ordered(t: TempoRange);

message StyleDescriptor {
  // 0.0-1.0.
  float energy = 1
    @validate(
      this >= 0.0 && this <= 1.0,
      code = "descriptor.energy.range",
      message = "energy must be within [0, 1]"
    );
  TempoRange tempo = 3 @required;
  Mode mode = 5
    @required
    @validate(
      this in [1, 2],
      code = "descriptor.mode.known",
      message = "mode must be match or regulate"
    );
  enum Mode {
    MODE_UNSPECIFIED = 0;
    MODE_MATCH = 1;
    MODE_REGULATE = 2;
  }
}

// Beats per minute, 40-220, min <= max.
@validate(
  tempo_ordered(this),
  code = "tempo.min_le_max",
  message = "min_bpm must not exceed max_bpm"
)
message TempoRange {
  int32 min_bpm = 1
    @validate(
      this >= 40 && this <= 220,
      code = "tempo.min.range",
      message = "min_bpm out of range"
    );
  int32 max_bpm = 2;
}
`
	edits := formatProtoDoc(t, src)
	require.Len(t, edits, 1, "v1.2 source is formatted, not skipped")
	got := edits[0].NewText
	assert.Equal(t, want, got)
	assert.Equal(t, strings.Count(src, "@validate"), strings.Count(got, "@validate"))
	assert.Equal(t, strings.Count(src, "@required"), strings.Count(got, "@required"))
	assert.Equal(t, strings.Count(src, "//"), strings.Count(got, "//"))

	assert.Empty(t, formatProtoDoc(t, got), "formatting is idempotent")
}

func TestProtoFormatting_AlreadyCanonicalReturnsEmptySlice(t *testing.T) {
	edits := formatProtoDoc(t, "syntax = \"proto3\";\n")
	require.NotNil(t, edits,
		"canonical source returns an empty slice, not nil (signals formatter ran, no edits)")
	assert.Empty(t, edits)
}

func TestProtoFormatting_NilOnParseError(t *testing.T) {
	// Mid-edit source: rewriting from a partial AST would lose the
	// user's text, so the client gets "no edits".
	assert.Nil(t, formatProtoDoc(t, "syntax = \"proto3\";\nmessage Foo {\n  string name = \n"))
}

// TestProtoFormatting_KeepsCommentAfterBodySemicolon: the printer
// drops the empty `;` after a body. Before protocompile#273 the
// comment trailing it went too, and the content guard refused to
// format the file; now the comment moves to its own line.
func TestProtoFormatting_KeepsCommentAfterBodySemicolon(t *testing.T) {
	const src = `syntax = "proto3";

message Foo {
  oneof choice {
    string a = 1;
  }; // kept
}
`
	edits := formatProtoDoc(t, src)
	require.Len(t, edits, 1)
	assert.Equal(t, `syntax = "proto3";

message Foo {
  oneof choice {
    string a = 1;
  }
  // kept
}
`, edits[0].NewText)
}

func TestFormatting_AdvertisesCapability(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodInitialize,
		&protocol.InitializeParams{})
	require.NoError(t, err)
	var caps map[string]interface{}
	reply := func(_ context.Context, result interface{}, _ error) error {
		b, _ := testCodec.Marshal(result)
		var probe struct {
			Capabilities map[string]interface{} `json:"capabilities"`
		}
		require.NoError(t, testCodec.Unmarshal(b, &probe))
		caps = probe.Capabilities
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	assert.Equal(t, true, caps["documentFormattingProvider"])
}
