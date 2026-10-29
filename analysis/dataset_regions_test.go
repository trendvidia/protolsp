// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trendvidia/protolsp/positions"
)

func TestDetectDatasetRegions_StringWithBraceDoesNotFoolCounter(t *testing.T) {
	// A `{` inside a string literal must NOT advance the brace
	// depth. The region's closing `}` is the real one, not the
	// one inside the string.
	src := []byte(`@dataset a.A {
  { col_a = "tricky}{value" }
}
`)
	regions := DetectDatasetRegions(src, 0, positions.UTF16)
	require.Len(t, regions, 1)
	// Region should end at the LAST `}` on its own line, not at
	// the one inside the string.
	lastBrace := strings.LastIndex(string(src), "}")
	endByte, _ := byteIndexAt(src, regions[0].Range.End, positions.UTF16)
	assert.Equal(t, lastBrace+1, endByte,
		"closing `}` is the one outside any string literal")
}

func TestDetectDatasetRegions_TripleQuotedString(t *testing.T) {
	src := []byte(`@dataset a.A {
  { col_a = """tricky}{value with } and { inside""" }
}
`)
	regions := DetectDatasetRegions(src, 0, positions.UTF16)
	require.Len(t, regions, 1)
	// Even with multiple `{` `}` inside the triple-quoted string,
	// the matching close is the outer brace.
	lastBrace := strings.LastIndex(string(src), "}")
	endByte, _ := byteIndexAt(src, regions[0].Range.End, positions.UTF16)
	assert.Equal(t, lastBrace+1, endByte)
}

// TestScanTupleDatasetEnd_UnclosedHeaderExtendsToEOF is TC7
// from the audit roadmap: scanTupleDatasetEnd's "header has
// a newline before the closing paren" + "EOF mid-header"
// branches were unexercised.
//
// `@dataset Trade(id, qty` then a newline before any `)` is
// malformed PXF, but the scanner has to terminate gracefully
// — it returns len(source) so the caller treats the rest of
// the file as part of the region (an editor-side fixup
// problem, not a parser crash).
func TestScanTupleDatasetEnd_UnclosedHeaderExtendsToEOF(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			"newline before closing paren",
			"@dataset acme.Trade(id, qty\n(1, 2)\n",
		},
		{
			"EOF mid-header",
			"@dataset acme.Trade(id, qty",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := []byte(c.src)
			// openParen is the byte index of `(` after `@dataset name`.
			openParen := strings.IndexByte(c.src, '(')
			require.GreaterOrEqual(t, openParen, 0, "test fixture has an open paren")
			end := scanTupleDatasetEnd(src, openParen)
			assert.Equal(t, len(src), end,
				"unclosed tuple header extends the region to EOF — no crash on malformed input")
		})
	}
}
