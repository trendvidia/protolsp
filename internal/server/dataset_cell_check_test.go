// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// cellCheckSchema has a field of every kind whose accepted literals
// Measured against protowire's `pxf validate`.
const cellCheckSchema = `
syntax = "proto3";
package c.v1;
import "google/protobuf/wrappers.proto";
import "google/protobuf/timestamp.proto";
import "pxf/secret.proto";
import "pxf/bignum.proto";
import "pxf/annotations.proto";

message Sub { string x = 1; }

message R {
  double d = 1;
  float f = 2;
  int32 i = 3;
  string s = 4;
  bytes b = 5;
  google.protobuf.StringValue sv = 6;
  google.protobuf.Int64Value iv = 7;
  google.protobuf.DoubleValue dv = 8;
  google.protobuf.BoolValue bv = 9;
  pxf.Secret pw = 10;
  pxf.BigInt bi = 11;
  pxf.Decimal dec = 12;
  pxf.BigFloat bf = 13;
  google.protobuf.Timestamp ts = 14;
  google.protobuf.BytesValue byv = 15;
  repeated string tags = 16;
  map<string, string> m = 17;
  Sub sub = 18;
  repeated Sub keyed = 19 [(pxf.key) = "x"];
  optional int32 oi = 20;
}
`

func buildCellCheckType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	c := &protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(map[string]string{
			"c.proto":               cellCheckSchema,
			"pxf/secret.proto":      "syntax = \"proto3\";\npackage pxf;\nmessage Secret { string value = 1; string hint = 2; string fingerprint = 3; }\n",
			"pxf/annotations.proto": "syntax = \"proto3\";\npackage pxf;\nimport \"google/protobuf/descriptor.proto\";\nextend google.protobuf.FieldOptions { string key = 1316; }\n",
			"pxf/bignum.proto":      "syntax = \"proto3\";\npackage pxf;\nmessage BigInt { bytes abs = 1; bool negative = 2; }\nmessage Decimal { bytes unscaled = 1; int32 scale = 2; bool negative = 3; }\nmessage BigFloat { bytes mantissa = 1; int32 exponent = 2; uint32 prec = 3; bool negative = 4; }\n",
		}),
	})}
	files, err := c.Compile(context.Background(), "c.proto")
	require.NoError(t, err)
	return dynamicpb.NewMessageType(files[0].Messages().ByName("R"))
}

// Each cell is accepted or rejected as the decoder accepts or rejects
// it. The verdicts are protowire's `pxf validate` on protowire-go
// v1.10.0, measured 2026-10-27, and on v1.11.0 for null on an
// optional scalar; a rejection gives the message the check reports.
func TestDatasetCellCheck_MatchesTheDecoder(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCellCheckType(t))
	srv.Lookups().Swap(lookup, nil)

	for i, tc := range []struct{ col, cell, reject string }{
		// Accepted by the decoder; the check used to reject the first ten.
		{"d", "1", ""},
		{"f", "2", ""},
		{"sv", `"x"`, ""},
		{"iv", "5", ""},
		{"dv", "5", ""},
		{"dv", "2.5", ""},
		{"bv", "true", ""},
		{"pw", `"x"`, ""},
		{"bi", "12", ""},
		{"dec", "3", ""},
		{"dec", "1.5", ""},
		{"bf", "2.5", ""},
		{"d", "1.5", ""},
		{"d", "inf", ""},
		{"ts", "2026-01-01T00:00:00Z", ""},
		{"tags", `["a"]`, ""},
		{"m", `{ "k": "v" }`, ""},
		{"sub", `{ x = "a" }`, ""},
		{"sub", "null", ""},
		{"sv", "null", ""},
		{"pw", "null", ""},
		{"pw", `{ value = "x" }`, ""},
		// Accepted since protowire-go v1.11.0 (protowire-go#182).
		{"oi", "null", ""},
		{"oi", "5", ""},
		// Rejected by the decoder; the check used to accept the first three.
		{"s", `b"aGk="`, `column "s" expects string; got bytes`},
		{"b", `"hi"`, `column "b" expects bytes; got string`},
		{"byv", `"x"`, `column "byv" expects bytes or a block; got string`},
		{"i", "1.0", `column "i" expects int32; got float`},
		{"s", "null", `column "s" expects string; got null`},
		{"s", "foo", `column "s" expects string; got identifier`},
		{"s", "2026-01-01T00:00:00Z", `column "s" expects string; got timestamp`},
		{"s", "1s", `column "s" expects string; got duration`},
		{"s", `{ x = 1 }`, `column "s" expects string; got block`},
		{"s", `["a"]`, `column "s" expects string; got list`},
		{"tags", `"x"`, `column "tags" expects a list; got string`},
		{"tags", "null", `column "tags" expects a list; got null`},
		{"m", `"x"`, `column "m" expects a map block; got string`},
		{"m", "null", `column "m" expects a map block; got null`},
		{"sub", `"x"`, `column "sub" expects a block; got string`},
		{"sub", `["a"]`, `column "sub" expects a block; got list`},
		{"sv", "5", `column "sv" expects string or a block; got int`},
		{"pw", "5", `column "pw" expects string or a block; got int`},
	} {
		t.Run(fmt.Sprintf("%s<-%s", tc.col, tc.cell), func(t *testing.T) {
			u := fmt.Sprintf("file:///work/c%d.pxf", i)
			src := fmt.Sprintf("@dataset c.v1.R (%s)\n(%s)\n", tc.col, tc.cell)
			require.NoError(t, srv.Handle(context.Background(), noopReply, didOpenCall(t, u, 1, src)))
			doc := srv.Documents().Get(uri.URI(u))
			require.NoError(t, doc.ParseErr)
			notes := fc.calls()
			diags := findDatasetSource(notes[len(notes)-1].params.Diagnostics)
			if tc.reject == "" {
				assert.Empty(t, diags)
				return
			}
			require.Len(t, diags, 1)
			assert.Equal(t, tc.reject, diags[0].Message)
			assert.Equal(t, uint32(1), diags[0].Range.Start.Line)
			assert.Equal(t, uint32(1), diags[0].Range.Start.Character, "the range starts at the cell")
		})
	}
}

// A dotted column is checked against the leaf field its path names. A
// column the binder rejects is reported once, at the directive, worded
// as the binder words it.
func TestDatasetCellCheck_DottedColumns(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCellCheckType(t))
	srv.Lookups().Swap(lookup, nil)

	for i, tc := range []struct {
		name, cols, row string
		want            []string
	}{
		{"leaf checked", "sub.x, pw.value, pw.hint", `"a", "s", "h"`, nil},
		{"leaf mismatch", "sub.x", "1", []string{`column "sub.x" expects string; got int`}},
		{"unknown first segment", "nope.x", `"a"`, []string{`unknown column "nope.x" on row type "c.v1.R"`}},
		{"unknown later segment", "sub.nope", `"a"`, []string{`column "sub.nope": c.v1.Sub has no field "nope"`}},
		{"through a repeated field", "tags.x", `"a"`, []string{`column "tags.x": "tags" is a repeated field, and a column path cannot go through one`}},
		{"through a map field", "m.x", `"a"`, []string{`column "m.x": "m" is a map field, and a column path cannot go through one`}},
		{"through a scalar", "s.x", `"a"`, []string{`column "s.x": "s" is not a message field`}},
		{"into a literal type", "ts.seconds", "1", []string{`column "ts.seconds": "ts" is a google.protobuf.Timestamp, which is written as a literal; a column path cannot go into it`}},
		{"into a wrapper", "sv.value", `"a"`, []string{`column "sv.value": "sv" is a google.protobuf.StringValue, which is written as a literal; a column path cannot go into it`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := fmt.Sprintf("file:///work/dot%d.pxf", i)
			src := fmt.Sprintf("@dataset c.v1.R (%s)\n(%s)\n", tc.cols, tc.row)
			require.NoError(t, srv.Handle(context.Background(), noopReply, didOpenCall(t, u, 1, src)))
			require.NoError(t, srv.Documents().Get(uri.URI(u)).ParseErr)
			notes := fc.calls()
			var got []string
			for _, d := range findDatasetSource(notes[len(notes)-1].params.Diagnostics) {
				got = append(got, d.Message)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// A list element or a map entry the field does not take is reported at
// the element or entry, saying what goes there; a keyed field also
// takes its block of named entries.
func TestDatasetCellCheck_InsideListsAndMaps(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCellCheckType(t))
	srv.Lookups().Swap(lookup, nil)

	for i, tc := range []struct {
		col, cell, want string
		char            uint32
	}{
		{"tags", `["a", 1]`, `column "tags": a list element expects string; got int`, 7},
		{"tags", `["a", null]`, `column "tags": a list element expects string; got null`, 7},
		{"keyed", `[{ x = "a" }, "b"]`, `column "keyed": a list element expects a block; got string`, 15},
		{"keyed", `"b"`, `column "keyed" expects a list or a block; got string`, 1},
		{"keyed", `{ a { } }`, "", 0},
		{"m", `{ "k": "v" j = "w" }`, `column "m": a map entry is written "key: value"`, 12},
		{"m", `{ "k": 1 }`, `column "m": a map value expects string; got int`, 8},
	} {
		t.Run(fmt.Sprintf("%s<-%s", tc.col, tc.cell), func(t *testing.T) {
			u := fmt.Sprintf("file:///work/in%d.pxf", i)
			src := fmt.Sprintf("@dataset c.v1.R (%s)\n(%s)\n", tc.col, tc.cell)
			require.NoError(t, srv.Handle(context.Background(), noopReply, didOpenCall(t, u, 1, src)))
			require.NoError(t, srv.Documents().Get(uri.URI(u)).ParseErr)
			notes := fc.calls()
			diags := findDatasetSource(notes[len(notes)-1].params.Diagnostics)
			if tc.want == "" {
				assert.Empty(t, diags)
				return
			}
			require.Len(t, diags, 1)
			assert.Equal(t, tc.want, diags[0].Message)
			assert.Equal(t, uint32(1), diags[0].Range.Start.Line)
			assert.Equal(t, tc.char, diags[0].Range.Start.Character)
		})
	}
}
