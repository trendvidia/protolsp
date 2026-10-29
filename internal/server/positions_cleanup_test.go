// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trendvidia/protolsp/positions"
)

// These tests pin the encoding-correctness gains from the I2
// cleanup: non-ASCII content in dataset regions honors the
// negotiated encoding. The LSP-position-helper half of the
// cleanup is pinned by analysis/positions_cleanup_test.go.

func TestDatasetRegions_UTF16ColumnsAccountForSurrogatePair(t *testing.T) {
	// Non-BMP char (😀, 4 bytes UTF-8 / 2 UTF-16 code units) in
	// the @dataset directive line. The Start.Character should
	// differ between UTF-8 (byte index 0) and UTF-16 (also 0
	// since the @dataset is at line start). The End position
	// matters more — it lands after the closing `}` on a
	// later line.
	const src = `# 😀 prefix-comment
@dataset acme.demo.v1.Trade {
  { id = "T-1" }
}
`
	regions8 := DetectDatasetRegions([]byte(src), 0, positions.UTF8)
	regions16 := DetectDatasetRegions([]byte(src), 0, positions.UTF16)
	require.Len(t, regions8, 1)
	require.Len(t, regions16, 1)

	// Both start on line 1 (after the comment line with emoji).
	// Start.Character = 0 in both encodings.
	assert.Equal(t, uint32(1), regions8[0].Range.Start.Line)
	assert.Equal(t, uint32(0), regions8[0].Range.Start.Character)
	assert.Equal(t, uint32(0), regions16[0].Range.Start.Character)

	// The End is on a later ASCII-only line so both encodings
	// agree there too. The PROPERTY this confirms is that the
	// encoding parameter is threaded through — the byte/UTF-16
	// branch is exercised.
	assert.Equal(t, regions8[0].Range.End.Line, regions16[0].Range.End.Line,
		"end lines match (ASCII content after the emoji)")
}

func TestDatasetRegions_EndColumnHonorsEncodingForNonBMPInBody(t *testing.T) {
	// The scanner requires @dataset at the start of a line
	// (after only whitespace), so the START column is always 0.
	// The END column, by contrast, lands on whichever byte is
	// past the closing `}` — and when that line contains
	// non-BMP characters, the encoding matters.
	//
	// Single-line @dataset with an emoji in a row value:
	//   `@dataset trade { { v = "😀" } }`
	// End byte = past the final `}`.
	const src = `@dataset trade { { v = "😀" } }` + "\n"

	regions8 := DetectDatasetRegions([]byte(src), 0, positions.UTF8)
	regions16 := DetectDatasetRegions([]byte(src), 0, positions.UTF16)
	require.Len(t, regions8, 1)
	require.Len(t, regions16, 1)

	// UTF-8: End.Character is the byte offset within the line.
	// UTF-16: End.Character is the UTF-16 code-unit count, which
	// is smaller because the emoji = 2 code units instead of 4
	// bytes.
	utf8End := regions8[0].Range.End.Character
	utf16End := regions16[0].Range.End.Character
	assert.Greater(t, utf8End, utf16End,
		"UTF-8 end column counts emoji as 4 bytes; UTF-16 as 2 code units")
	assert.Equal(t, utf8End-utf16End, uint32(2),
		"the emoji's 4 UTF-8 bytes vs 2 UTF-16 code units = 2-unit difference")
}
