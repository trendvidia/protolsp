// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// A pxf.BigFloat literal may carry a precision suffix, 0.1p53
// (protowire#406, protowire-go v1.12.0); any other field rejects one
// (#303).
const suffixSchema = `@proto """
syntax = "proto3";
package s.v1;
import "pxf/bignum.proto";
message M {
  pxf.BigFloat bf = 1;
  pxf.Decimal dec = 2;
  double d = 3;
  int32 i = 4;
  pxf.BigInt bi = 5;
}
"""
`

// The number token covers the suffix.
func TestBigFloatSuffix_SemanticToken(t *testing.T) {
	src := suffixSchema + "@type s.v1.M\nbf = 0.10000000000000001p53\n"
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply, didOpenCall(t, "file:///work/s.pxf", 1, src)))
	tokens := decodeSemanticTokens(t, semanticTokensFromHandle(t, srv, "file:///work/s.pxf").Data)
	line := uint32(strings.Count(suffixSchema, "\n") + 1)
	var nums []decodedToken
	for _, tk := range tokens {
		if tk.tokenType == protocol.SemanticTokenNumber && tk.line == line {
			nums = append(nums, tk)
		}
	}
	require.Len(t, nums, 1, "%v", tokens)
	assert.Equal(t, uint32(len("bf = ")), nums[0].char)
	assert.Equal(t, uint32(len("0.10000000000000001p53")), nums[0].length)
}

// Formatting keeps the suffix.
func TestBigFloatSuffix_Formatting(t *testing.T) {
	src := suffixSchema + "@type s.v1.M\nbf    =   0.10000000000000001p53\n"
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply, didOpenCall(t, "file:///work/f.pxf", 1, src)))
	edits := formattingFromHandle(t, srv, "file:///work/f.pxf")
	require.Len(t, edits, 1)
	assert.Contains(t, edits[0].NewText, "bf = 0.10000000000000001p53\n")
}

// In a body and in a @dataset cell, the suffix is accepted on a
// BigFloat and reported on any other field, with the decoder's message.
func TestBigFloatSuffix_Diagnostics(t *testing.T) {
	for _, tc := range []struct{ src, source, reject string }{
		{"@type s.v1.M\nbf = 0.10000000000000001p53\n", "protolsp/schema", ""},
		{"@type s.v1.M\nd = 1.5p53\n", "protolsp/schema", `precision suffix p53 is allowed only on a pxf.BigFloat, not on field "d"`},
		{"@type s.v1.M\ndec = 1.5p53\n", "protolsp/schema", `precision suffix p53 is allowed only on a pxf.BigFloat, not on field "dec"`},
		{"@dataset s.v1.M (bf)\n(0.1p53)\n(1p64)\n", "protolsp/dataset", ""},
		{"@dataset s.v1.M (dec)\n(1.5p53)\n", "protolsp/dataset", `column "dec": precision suffix p53 is allowed only on a pxf.BigFloat, not on field "dec"`},
		{"@dataset s.v1.M (d)\n(1.5p53)\n", "protolsp/dataset", `column "d": precision suffix p53 is allowed only on a pxf.BigFloat, not on field "d"`},
		{"@dataset s.v1.M (i)\n(1p53)\n", "protolsp/dataset", `column "i": precision suffix p53 is allowed only on a pxf.BigFloat, not on field "i"`},
		{"@dataset s.v1.M (bi)\n(1p53)\n", "protolsp/dataset", `column "bi": precision suffix p53 is allowed only on a pxf.BigFloat, not on field "bi"`},
		{"@dataset s.v1.M (bf)\n(1.5p0)\n", "protolsp/dataset", `column "bf": big float literal 1.5p0: precision suffix p0 is not a positive bit count`},
	} {
		t.Run(fmt.Sprintf("%q", tc.src), func(t *testing.T) {
			_, diags := openInline(t, suffixSchema+tc.src)
			assert.Empty(t, messages(diags, "protolsp/proto"))
			got := messages(diags, tc.source)
			if tc.reject == "" {
				assert.Empty(t, got, "%v", diags)
				return
			}
			assert.Equal(t, []string{tc.reject}, got)
		})
	}
}
