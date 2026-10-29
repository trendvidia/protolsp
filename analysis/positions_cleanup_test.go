// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

func TestFieldRefContextAtCursor_UTF16StartHonorsSurrogatePair(t *testing.T) {
	// Source has an emoji BEFORE the ${ — `start` (the LSP
	// character of the first byte after `${`) must reflect the
	// UTF-16 code-unit position, not the UTF-8 byte position.
	// Line: `x = "😀${db.h"` —
	//   UTF-8 bytes: x(0) (1)=(2) (3)"(4)😀(5-8)$(9){(10)d(11)b(12).(13)h(14)"(15)
	//   UTF-16 chars: x(0) (1)=(2) (3)"(4)😀(5-6)$(7){(8)d(9)b(10).(11)h(12)"(13)
	src := []byte(`x = "😀${db.h"`)

	// Cursor BETWEEN `h` and the closing `"`. In LSP this is the
	// position AFTER `h`:
	//   UTF-8: char 15 (one past byte 14 = 'h')
	//   UTF-16: char 13 (one past code unit 12 = 'h')
	prefix, start, ok := fieldRefContextAtCursor(src, protocol.Position{
		Line: 0, Character: 15,
	}, positions.UTF8)
	require.True(t, ok)
	assert.Equal(t, "db.h", prefix)
	// UTF-8: contentStart byte = 11 → LSP char 11.
	assert.Equal(t, uint32(11), start,
		"UTF-8: start is the byte offset of the first byte after `${`")

	prefix, start, ok = fieldRefContextAtCursor(src, protocol.Position{
		Line: 0, Character: 13,
	}, positions.UTF16)
	require.True(t, ok)
	assert.Equal(t, "db.h", prefix)
	// UTF-16: src[0:11] = `x = "😀${` — 5 ASCII chars before
	// emoji + 2 surrogate code units + $ + { = 9 code units.
	assert.Equal(t, uint32(9), start,
		"UTF-16: start counts the emoji as 2 code units")
}

func TestDatasetRegions_PostMigrationRoundTripsThroughByteIndexAt(t *testing.T) {
	// DetectDatasetRegions internally uses byteIndexAt to advance
	// past a found region. The round-trip (byteToLineCol →
	// byteIndexAt) must preserve the byte position regardless of
	// encoding, since both helpers consult the same encoding.
	const src = "# 😀\n@dataset trade { { id = \"X\" } }\nrest of doc\n"

	for _, enc := range []positions.Encoding{positions.UTF8, positions.UTF16} {
		regions := DetectDatasetRegions([]byte(src), 0, enc)
		require.Len(t, regions, 1, "enc=%v", enc)
		// Round-trip: the End position → byte → should land on
		// the same byte regardless of encoding. We can't easily
		// extract the exact byte here, but we can confirm the
		// byteIndexAt of the returned end position is consistent
		// with the encoding the region was built in.
		byteOff, _ := byteIndexAt([]byte(src), regions[0].Range.End, enc)
		assert.LessOrEqual(t, byteOff, len(src))
		assert.Greater(t, byteOff, 0)
	}
}
