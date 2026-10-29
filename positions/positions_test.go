// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package positions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

func TestNegotiate_PrefersUTF8WhenOffered(t *testing.T) {
	assert.Equal(t, UTF8, Negotiate([]string{"utf-16", "utf-8"}))
	assert.Equal(t, UTF8, Negotiate([]string{"utf-8"}))
}

func TestNegotiate_FallsBackToUTF16(t *testing.T) {
	assert.Equal(t, UTF16, Negotiate(nil),
		"missing list → LSP default UTF-16")
	assert.Equal(t, UTF16, Negotiate([]string{}),
		"empty list → UTF-16")
	assert.Equal(t, UTF16, Negotiate([]string{"utf-16"}),
		"only UTF-16 offered → UTF-16")
	assert.Equal(t, UTF16, Negotiate([]string{"utf-32", "utf-16"}),
		"UTF-32 isn't supported; UTF-16 wins over it")
}

func TestEncoding_StringMatchesLSPWireFormat(t *testing.T) {
	assert.Equal(t, "utf-8", UTF8.String())
	assert.Equal(t, "utf-16", UTF16.String())
}

func TestLineStart_HandlesBasicLayout(t *testing.T) {
	src := []byte("alpha\nbeta\ngamma")
	assert.Equal(t, 0, LineStart(src, 0))
	assert.Equal(t, 6, LineStart(src, 1))
	assert.Equal(t, 11, LineStart(src, 2))
}

func TestLineStart_LinePastEOFClampsToEOF(t *testing.T) {
	src := []byte("alpha\nbeta")
	assert.Equal(t, len(src), LineStart(src, 5))
}

func TestOffset_UTF8_AsciiIsByteOffset(t *testing.T) {
	src := []byte("alpha\nbeta")
	off, err := Offset(src, protocol.Position{Line: 1, Character: 2}, UTF8)
	require.NoError(t, err)
	assert.Equal(t, 8, off, "byte 8 is 't' in 'beta' (line 1, col 2)")
}

func TestOffset_UTF16_AsciiMatchesUTF8(t *testing.T) {
	src := []byte("alpha\nbeta")
	off, err := Offset(src, protocol.Position{Line: 1, Character: 2}, UTF16)
	require.NoError(t, err)
	assert.Equal(t, 8, off, "ASCII content: UTF-16 == UTF-8 byte offset")
}

func TestOffset_UTF16_NonBMPCountsTwoCodeUnits(t *testing.T) {
	// 😀 (U+1F600) is a non-BMP code point — 4 UTF-8 bytes, 2
	// UTF-16 code units (surrogate pair). After the emoji and a
	// space, character index 3 (UTF-16) lands at byte 5 (after
	// the 4-byte emoji + the space).
	src := []byte("😀 hi")
	off, err := Offset(src, protocol.Position{Line: 0, Character: 3}, UTF16)
	require.NoError(t, err)
	assert.Equal(t, 5, off,
		"UTF-16 char 3 == byte 5: 4 bytes emoji (2 code units) + 1 byte space (1 unit) = 3 units")
}

func TestOffset_UTF8_NonBMPIsBytePosition(t *testing.T) {
	src := []byte("😀 hi")
	// Character 5 in UTF-8 byte-offset semantics → byte 5
	off, err := Offset(src, protocol.Position{Line: 0, Character: 5}, UTF8)
	require.NoError(t, err)
	assert.Equal(t, 5, off, "UTF-8: char IS byte offset")
}

func TestOffset_ClampsBeyondEOL(t *testing.T) {
	src := []byte("ab\ncd")
	// Char 100 on line 0 → clamps to the newline byte (offset 2).
	off, err := Offset(src, protocol.Position{Line: 0, Character: 100}, UTF8)
	require.NoError(t, err)
	assert.Equal(t, 2, off)
}

func TestOffset_ClampsBeyondEOFLine(t *testing.T) {
	src := []byte("ab\ncd")
	// Line 99 → clamps to len(src).
	off, err := Offset(src, protocol.Position{Line: 99, Character: 0}, UTF8)
	require.NoError(t, err)
	assert.Equal(t, len(src), off)
}

func TestOffset_UTF16_InvalidUTF8ReturnsError(t *testing.T) {
	// Invalid UTF-8 byte 0xFF mid-line.
	src := []byte{'a', 0xFF, 'b'}
	_, err := Offset(src, protocol.Position{Line: 0, Character: 2}, UTF16)
	require.Error(t, err)
}

func TestLineCol_RoundTripsOffset(t *testing.T) {
	src := []byte("alpha\nbeta\ngamma")
	// Pick a few offsets and round-trip through both encodings.
	cases := []struct {
		byteOffset int
		wantLine   uint32
		wantCol8   uint32
		wantCol16  uint32
	}{
		{0, 0, 0, 0},
		{3, 0, 3, 3},
		{6, 1, 0, 0},
		{10, 1, 4, 4},
		{11, 2, 0, 0},
		{16, 2, 5, 5},
	}
	for _, c := range cases {
		line, col := LineCol(src, c.byteOffset, UTF8)
		assert.Equal(t, c.wantLine, line, "UTF-8 line for offset %d", c.byteOffset)
		assert.Equal(t, c.wantCol8, col, "UTF-8 col for offset %d", c.byteOffset)

		line, col = LineCol(src, c.byteOffset, UTF16)
		assert.Equal(t, c.wantLine, line, "UTF-16 line for offset %d", c.byteOffset)
		assert.Equal(t, c.wantCol16, col, "UTF-16 col for offset %d", c.byteOffset)
	}
}

func TestLineCol_NonBMPCharacterAddsTwoUTF16CodeUnits(t *testing.T) {
	src := []byte("😀ab") // 4 bytes emoji + a + b
	// Byte offset after the 'a' = byte 5.
	line, col := LineCol(src, 5, UTF16)
	assert.Equal(t, uint32(0), line)
	assert.Equal(t, uint32(3), col,
		"emoji = 2 UTF-16 code units + 'a' = 1 → col 3")

	// Same byte offset in UTF-8 is just the byte index from line start.
	_, colUTF8 := LineCol(src, 5, UTF8)
	assert.Equal(t, uint32(5), colUTF8)
}

func TestLineCol_NegativeOffsetClampsToZero(t *testing.T) {
	src := []byte("alpha\nbeta")
	line, col := LineCol(src, -1, UTF8)
	assert.Equal(t, uint32(0), line)
	assert.Equal(t, uint32(0), col)
}

func TestLineCol_OffsetPastEOFClampsToEnd(t *testing.T) {
	src := []byte("alpha\nbeta")
	line, col := LineCol(src, 100, UTF8)
	assert.Equal(t, uint32(1), line, "last line is 1 (0-indexed)")
	assert.Equal(t, uint32(4), col, "offset clamps to len(src) = end of 'beta'")
}

func TestOffset_TrailingNewlineHasVirtualLine(t *testing.T) {
	// LSP allows positions on the "virtual" line past the trailing
	// newline. LineStart returns len(src) for that line.
	src := []byte("alpha\n")
	off, err := Offset(src, protocol.Position{Line: 1, Character: 0}, UTF8)
	require.NoError(t, err)
	assert.Equal(t, len(src), off)
}
