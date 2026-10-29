// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile"
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const decoderCheckSchema = `
syntax = "proto3";
package c.v1;
import "google/protobuf/wrappers.proto";
import "google/protobuf/timestamp.proto";
import "google/protobuf/duration.proto";
import "pxf/secret.proto";
import "pxf/bignum.proto";
import "pxf/annotations.proto";

enum Color { COLOR_UNSPECIFIED = 0; RED = 1; }
message Sub { string x = 1; }
message Item { string id = 1; int32 count = 2; }

message R {
  double d = 1;
  float f = 2;
  int32 i = 3;
  string s = 4;
  bytes b = 5;
  bool bo = 6;
  Color c = 7;
  google.protobuf.StringValue sv = 8;
  google.protobuf.Int64Value iv = 9;
  google.protobuf.DoubleValue dv = 10;
  google.protobuf.BoolValue bv = 11;
  pxf.Secret pw = 12;
  pxf.BigInt bi = 13;
  pxf.Decimal dec = 14;
  pxf.BigFloat bf = 15;
  google.protobuf.Timestamp ts = 16;
  google.protobuf.Duration du = 17;
  Sub sub = 18;
  repeated string tags = 19;
  repeated int32 nums = 20;
  repeated double ds = 21;
  repeated Color colors = 22;
  repeated google.protobuf.Timestamp tss = 23;
  repeated pxf.Secret pws = 24;
  repeated Item plain = 25;
  repeated Item items = 26 [(pxf.key) = "id"];
  map<string, string> m = 27;
  map<string, Sub> ms = 28;
  map<string, google.protobuf.Timestamp> mt = 29;
  map<string, google.protobuf.StringValue> msv = 30;
  map<string, Color> mc = 31;
  map<string, pxf.Secret> mpw = 32;
  map<string, double> md = 33;
  map<int32, string> mi = 34;
}
`

func decoderCheckRowType(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	c := &protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(map[string]string{
			"c.proto":               decoderCheckSchema,
			"pxf/secret.proto":      "syntax = \"proto3\";\npackage pxf;\nmessage Secret { string value = 1; string hint = 2; string fingerprint = 3; }\n",
			"pxf/bignum.proto":      "syntax = \"proto3\";\npackage pxf;\nmessage BigInt { bytes abs = 1; bool negative = 2; }\nmessage Decimal { bytes unscaled = 1; int32 scale = 2; bool negative = 3; }\nmessage BigFloat { bytes mantissa = 1; int32 exponent = 2; uint32 prec = 3; bool negative = 4; }\n",
			"pxf/annotations.proto": "syntax = \"proto3\";\npackage pxf;\nimport \"google/protobuf/descriptor.proto\";\nextend google.protobuf.FieldOptions { string key = 1316; }\n",
		}),
	})}
	files, err := c.Compile(context.Background(), "c.proto")
	require.NoError(t, err)
	desc := files[0].Messages().ByName("R")
	require.True(t, pxf.IsKeyed(desc.Fields().ByName("items")), "the (pxf.key) fixture is keyed")
	return desc
}

// bindAndCheck parses a one-cell dataset and returns whether the
// decoder binds the row and whether the cell check accepts it.
func bindAndCheck(t *testing.T, desc protoreflect.MessageDescriptor, col, cell string) (decoderOK, checkOK bool) {
	t.Helper()
	doc, err := pxf.Parse([]byte(fmt.Sprintf("@dataset c.v1.R (%s)\n(%s)\n", col, cell)))
	require.NoError(t, err)
	require.Len(t, doc.Datasets, 1)
	ds := &doc.Datasets[0]
	require.Len(t, ds.Rows, 1)
	bindErr := pxf.BindRow(dynamicpb.NewMessage(desc), ds.Columns, ds.Rows[0])
	return bindErr == nil, len(validateRows(ds, desc)) == 0
}

// The cell check gives the decoder's verdict on every cell below,
// checked against protowire-go's own pxf.BindRow rather than recorded
// from a run of it, so a protowire bump that moves a verdict fails
// here (#290). ok is the decoder's verdict as of protowire-go v1.10.0.
func TestCellCheck_AgreesWithTheDecoder(t *testing.T) {
	desc := decoderCheckRowType(t)
	for _, tc := range []struct {
		col, cell string
		ok        bool
	}{
		// Scalars, identifiers included: an identifier is an enum name,
		// or inf / nan in a float, and nothing else.
		{"d", "1", true}, {"d", "inf", true}, {"d", "nan", true}, {"d", "-inf", true}, {"f", "nan", true},
		{"d", "foo", false}, {"i", "foo", false}, {"i", "inf", false}, {"bo", "foo", false},
		{"s", "foo", false}, {"b", `"hi"`, false}, {"s", `b"aGk="`, false}, {"s", "null", false},
		{"c", "RED", true}, {"c", "1", true}, {"c", `"RED"`, false}, {"c", "1.0", false}, {"c", "null", false},
		// Message types in their literal forms.
		{"sv", `"x"`, true}, {"sv", "foo", false}, {"iv", "5", true}, {"iv", "foo", false},
		{"dv", "inf", true}, {"dv", "foo", false}, {"bv", "true", true}, {"bv", "yes", false},
		{"pw", `"x"`, true}, {"pw", "foo", false}, {"bi", "12", true}, {"bi", "foo", false},
		{"dec", "3", true}, {"dec", "1.5", true}, {"dec", "inf", false}, {"bf", "nan", false},
		{"ts", "2026-01-01T00:00:00Z", true}, {"ts", "foo", false}, {"du", "1s", true}, {"du", "foo", false},
		{"sub", "{}", true}, {"sub", "null", true}, {"sub", "foo", false}, {"sub", `"x"`, false}, {"sub", `["a"]`, false},
		// Lists: each element as a singular value of the element type,
		// and never null.
		{"tags", `["a", "b"]`, true}, {"tags", "[]", true}, {"tags", `"a"`, false}, {"tags", "null", false},
		{"tags", "[1]", false}, {"tags", "[null]", false}, {"tags", "[foo]", false},
		{"tags", `[["a"]]`, false}, {"tags", `[{ x = 1 }]`, false}, {"tags", `{ "a": "b" }`, false},
		{"nums", "[1, 2]", true}, {"nums", "[1.5]", false}, {"nums", `["1"]`, false},
		{"ds", "[1, 2.5, inf]", true}, {"ds", "[foo]", false},
		{"colors", "[RED, 1]", true}, {"colors", `["RED"]`, false},
		{"tss", "[2026-01-01T00:00:00Z]", true}, {"tss", "[{ seconds = 1 }]", true}, {"tss", `["x"]`, false}, {"tss", "[null]", false},
		{"pws", `["a"]`, true}, {"pws", `[{ value = "a" }]`, true}, {"pws", "[1]", false},
		{"plain", `[{ id = "a" }]`, true}, {"plain", `{ a { count = 1 } }`, false},
		{"plain", "[1]", false}, {"plain", `["a"]`, false}, {"plain", "[null]", false}, {"plain", "[foo]", false},
		// A keyed field takes its block of named entries as well.
		{"items", `{ a { count = 1 } }`, true}, {"items", "{}", true}, {"items", `[{ id = "a" }]`, true}, {"items", "[]", true},
		{"items", "[1]", false}, {"items", "[null]", false},
		// Maps: "key: value" entries, each value as a singular value of
		// the value type, and never null.
		{"m", `{ "k": "v" }`, true}, {"m", `{ k: "v" }`, true}, {"m", "{}", true}, {"m", `"x"`, false}, {"m", "null", false},
		{"m", `{ k = "v" }`, false}, {"m", `{ "k": 1 }`, false}, {"m", `{ "k": null }`, false}, {"m", `{ "k": ["a"] }`, false},
		{"ms", `{ "k": { x = "a" } }`, true}, {"ms", `{ "k": {} }`, true}, {"ms", `{ "k": "a" }`, false},
		{"ms", `{ "k": null }`, false}, {"ms", `{ k { x = "a" } }`, false},
		{"mt", `{ "k": 2026-01-01T00:00:00Z }`, true}, {"mt", `{ "k": "x" }`, false},
		{"msv", `{ "k": "x" }`, true}, {"msv", `{ "k": { value = "x" } }`, true}, {"msv", `{ "k": 1 }`, false},
		{"mc", `{ "k": RED }`, true}, {"mc", `{ "k": 1 }`, true}, {"mc", `{ "k": "RED" }`, false},
		{"mpw", `{ "k": "s" }`, true}, {"mpw", `{ "k": 1 }`, false},
		{"md", `{ "k": inf }`, true}, {"md", `{ "k": foo }`, false}, {"mi", `{ 1: "a" }`, true},
		// What the decoder decides from the value rather than its kind:
		// a name the enum lacks, a map key its key type cannot parse, an
		// int out of range, and what a block holds (#293).
		{"c", "BLUE", false}, {"mi", `{ x: "a" }`, false}, {"i", "99999999999", false},
		{"sub", `{ x = "a" }`, true}, {"sub", `{ x = 1 }`, false}, {"sub", `{ nope = 1 }`, false},
		{"items", `{ a = 1 }`, false}, {"plain", `[{ nope = 1 }]`, false}, {"ms", `{ "k": { nope = 1 } }`, false},
	} {
		t.Run(fmt.Sprintf("%s<-%s", tc.col, tc.cell), func(t *testing.T) {
			decoderOK, checkOK := bindAndCheck(t, desc, tc.col, tc.cell)
			require.Equal(t, tc.ok, decoderOK, "the decoder's verdict moved; re-measure the case")
			assert.Equal(t, decoderOK, checkOK, "the cell check disagrees with the decoder")
		})
	}
}

// The decoder's error goes on the cell it names, in the binder's
// words with the column quoted. A row the kind check already faults
// is not bound as well, so a cell is never reported twice.
func TestCellCheck_DecoderErrorPlacement(t *testing.T) {
	desc := decoderCheckRowType(t)
	doc, err := pxf.Parse([]byte("@dataset c.v1.R (s, sub)\n(\"ok\", { nope = 1 })\n(1, { nope = 1 })\n"))
	require.NoError(t, err)
	diags := validateRows(&doc.Datasets[0], desc)
	require.Len(t, diags, 2)
	assert.Equal(t, `column "sub": unknown field "nope" in c.v1.Sub`, diags[0].Message)
	assert.Equal(t, protocol.Position{Line: 1, Character: 7}, diags[0].Range.Start, "on the block cell, not the row")
	assert.Equal(t, `column "s" expects string; got int`, diags[1].Message)
}

// The decoder names types by their full names; the synthetic name of
// an anonymous @proto is protolsp's own and is not shown.
func TestHideAnonNames(t *testing.T) {
	assert.Equal(t, `unknown field "nope" in In`, hideAnonNames(`unknown field "nope" in `+anonPrefix+`0.In`))
	assert.Equal(t, `unknown field "nope" in the anonymous @proto row type`, hideAnonNames(`unknown field "nope" in `+anonPrefix+`12`))
	assert.Equal(t, `unknown field "nope" in c.v1.Sub`, hideAnonNames(`unknown field "nope" in c.v1.Sub`))
}
