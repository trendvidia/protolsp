// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package protoast

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

// TestOffsetIn pins the position→offset conversion under both
// negotiated encodings (#232 — the input-side twin of RangeIn).
// Line 1 of the fixture puts a 4-byte / 2-code-unit emoji inside a
// block comment BEFORE the message keyword, so every column after
// it differs between the two encodings:
//
//	/*😀*/ message Greeting {}
//	bytes:  / * [😀 = 4 bytes] * / ␣ m…e ␣ G  → 'G' at byte 17
//	utf-16: / * [😀 = 2 units] * / ␣ m…e ␣ G  → 'G' at unit 15
func TestOffsetIn(t *testing.T) {
	const src = "syntax = \"proto3\";\n/*\U0001F600*/ message Greeting {}\n"
	f := Parse("test.proto", []byte(src))
	require.NotNil(t, f)

	lineStart := len("syntax = \"proto3\";\n")
	gByte := lineStart + 17

	tests := []struct {
		name string
		pos  protocol.Position
		enc  positions.Encoding
		want int
	}{
		{"utf-16 counts code units", protocol.Position{Line: 1, Character: 15}, positions.UTF16, gByte},
		{"utf-8 counts bytes", protocol.Position{Line: 1, Character: 17}, positions.UTF8, gByte},
		{"ascii-only line agrees", protocol.Position{Line: 0, Character: 7}, positions.UTF8,
			f.OffsetIn(protocol.Position{Line: 0, Character: 7}, positions.UTF16)},
		{"column past line end clamps (utf-8)", protocol.Position{Line: 0, Character: 999}, positions.UTF8,
			len("syntax = \"proto3\";")},
		{"line past EOF is -1", protocol.Position{Line: 99, Character: 0}, positions.UTF8, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, f.OffsetIn(tt.pos, tt.enc))
		})
	}
}
