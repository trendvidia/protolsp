// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

const keyedRowSource = `@proto """
syntax = "proto3";
package k.v1;
import "pxf/annotations.proto";
message Item { string id = 1; int32 n = 2; }
message Row {
  repeated Item items = 1 [(pxf.key) = "id"];
  Item one = 2;
}
"""
`

// keyedAt renders each "protolsp/keyed" diagnostic as its 1-based line
// and column, the source text its range covers, and its message, so a
// test pins both where the squiggle goes and what it says.
func keyedAt(t *testing.T, src string, diags []protocol.Diagnostic) []string {
	t.Helper()
	lines := strings.Split(src, "\n")
	var out []string
	for _, d := range diags {
		if optValue(d.Source) != "protolsp/keyed" {
			continue
		}
		require.Equal(t, d.Range.Start.Line, d.Range.End.Line, "one-line range")
		line := lines[d.Range.Start.Line]
		out = append(out, fmt.Sprintf("%d:%d:%s: %s", d.Range.Start.Line+1, d.Range.Start.Character+1, line[d.Range.Start.Character:d.Range.End.Character], messageText(d.Message)))
	}
	return out
}

// A keyed violation in a @dataset cell is a diagnostic at the violating
// entry, every one of them in source order, as in a body (#33). The
// decoder's own error for the row, which stops at the first violation
// and sits on the row, is not shown as well.
func TestDatasetKeyed_TypedDataset(t *testing.T) {
	src := keyedRowSource + `@dataset k.v1.Row (items, one)
({ a {} a {} }, {})
({ "" {} }, {})
({ b { id = "c" } }, { "q" {} })
`
	_, diags := openInline(t, src)
	assert.Equal(t, []string{
		`12:9:a: duplicate key "a" in keyed field "items"`,
		`13:4:"": empty entry name in keyed field "items": the empty string is not a valid key`,
		`14:13:"c": key field "id" = "c" conflicts with entry name "b" in keyed field "items"`,
		`14:24:"q": quoted entry name "q" is only valid inside a keyed repeated field's block (draft -01 §3.13)`,
	}, keyedAt(t, src, diags))
	assert.Empty(t, messages(diags, "protolsp/dataset"), "the decoder's keyed error is not reported again")
	assert.Empty(t, messages(diags, "protolsp/proto"))
}

// A typeless @dataset bound to an anonymous @proto is checked against
// that row type. An anonymous @proto imports nothing, so it has no keyed
// field: the violation it can hold is a quoted entry name.
func TestDatasetKeyed_TypelessDataset(t *testing.T) {
	src := "@proto { message Sub { string x = 1; } Sub s = 1; }\n" +
		"@dataset (s)\n" +
		"({ x = \"ok\" })\n" +
		"({ \"q\" {} })\n"
	_, diags := openInline(t, src)
	assert.Equal(t, []string{
		`4:4:"q": quoted entry name "q" is only valid inside a keyed repeated field's block (draft -01 §3.13)`,
	}, keyedAt(t, src, diags))
	assert.Empty(t, messages(diags, "protolsp/dataset"), "the decoder's keyed error is not reported again")
	assert.Empty(t, messages(diags, "protolsp/proto"))
}

// A row without a keyed violation still shows the decoder's error.
func TestDatasetKeyed_OtherDecoderErrorsStay(t *testing.T) {
	src := keyedRowSource + `@dataset k.v1.Row (items)
({ a { n = 99999999999 } })
`
	_, diags := openInline(t, src)
	assert.Empty(t, keyedAt(t, src, diags))
	assert.Equal(t, []string{`column "items": invalid int32: 99999999999`}, messages(diags, "protolsp/dataset"))
}
