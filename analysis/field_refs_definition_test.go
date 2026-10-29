// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

func TestRefTokenAtCursor_CursorOnPath(t *testing.T) {
	src := []byte(`api_url = "${db.host}"`)
	// `${db.host}` spans chars 11..21. Cursor at 14 (mid `db`).
	path, ok := refTokenAtCursor(src, lspPos(0, 14))
	require.True(t, ok)
	assert.Equal(t, "db.host", path)
}

func TestRefTokenAtCursor_CursorOnOpeningDollar(t *testing.T) {
	src := []byte(`api_url = "${db.host}"`)
	// Char 11 = `$`. Inclusive.
	path, ok := refTokenAtCursor(src, lspPos(0, 11))
	require.True(t, ok)
	assert.Equal(t, "db.host", path)
}

func TestRefTokenAtCursor_CursorOnClosingBrace(t *testing.T) {
	src := []byte(`api_url = "${db.host}"`)
	// `}` is at char 20; m[1] = char 21 (exclusive end).
	path, ok := refTokenAtCursor(src, lspPos(0, 21))
	require.True(t, ok)
	assert.Equal(t, "db.host", path)
}

func TestRefTokenAtCursor_CursorOutsideRefReturnsFalse(t *testing.T) {
	src := []byte(`api_url = "${db.host}!"`)
	// Char 22 = `!`, just past the closing `}`.
	_, ok := refTokenAtCursor(src, lspPos(0, 22))
	assert.False(t, ok)
}

func TestRefTokenAtCursor_NoRefOnLineReturnsFalse(t *testing.T) {
	src := []byte(`api_url = "plain"`)
	_, ok := refTokenAtCursor(src, lspPos(0, 5))
	assert.False(t, ok)
}

func TestWalkEntriesByPath_TopLevelAssignment(t *testing.T) {
	pos, ok := findAssignmentByPath([]byte(`@type c4test.v1.AppConfig
api_url = "https://example"
debug = true
`), []string{"api_url"})
	require.True(t, ok)
	assert.Equal(t, 2, pos.Line, "pxf is 1-indexed: line 2 is the api_url assignment")
}

func TestWalkEntriesByPath_NestedAssignment(t *testing.T) {
	pos, ok := findAssignmentByPath([]byte(`@type c4test.v1.AppConfig
db {
  host = "abc"
  port = 5432
}
`), []string{"db", "host"})
	require.True(t, ok)
	assert.Equal(t, 3, pos.Line,
		"host = ... is on line 3 (1-indexed) of the source")
}

func TestWalkEntriesByPath_BlockMatchedAtRoot(t *testing.T) {
	// Path "db" alone matches the block itself, not any inner field.
	pos, ok := findAssignmentByPath([]byte(`@type c4test.v1.AppConfig
db {
  host = "abc"
}
`), []string{"db"})
	require.True(t, ok)
	assert.Equal(t, 2, pos.Line)
}

func TestWalkEntriesByPath_NoMatchReturnsFalse(t *testing.T) {
	_, ok := findAssignmentByPath([]byte(`@type c4test.v1.AppConfig
api_url = "x"
`), []string{"nonexistent"})
	assert.False(t, ok)
}

// lspPos is a tiny helper to keep the position-construction noise
// out of the test bodies.
func lspPos(line, char uint32) protocol.Position {
	return protocol.Position{Line: line, Character: char}
}
