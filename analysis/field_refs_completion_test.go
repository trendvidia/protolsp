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

func TestFieldRefContextAtCursor_BasicMatch(t *testing.T) {
	src := []byte(`api_url = "${db.h"`)
	// Cursor between `h` (char 16) and `"` (char 17) → character 17.
	prefix, start, ok := fieldRefContextAtCursor(src, protocol.Position{
		Line: 0, Character: 17,
	}, positions.UTF16)
	require.True(t, ok)
	assert.Equal(t, "db.h", prefix)
	assert.Equal(t, uint32(13), start,
		"start is the LSP character of the first byte after `${`")
}

func TestFieldRefContextAtCursor_ClosedRefReturnsFalse(t *testing.T) {
	src := []byte(`api_url = "${db.host}!"`)
	// Cursor after `!` — well past the `}`.
	_, _, ok := fieldRefContextAtCursor(src, protocol.Position{
		Line: 0, Character: 22,
	}, positions.UTF16)
	assert.False(t, ok, "cursor past `}` is outside the ${} context")
}

func TestFieldRefContextAtCursor_EmptyRefMatches(t *testing.T) {
	src := []byte(`api_url = "${`)
	prefix, _, ok := fieldRefContextAtCursor(src, protocol.Position{
		Line: 0, Character: 13, // right after `${`
	}, positions.UTF16)
	assert.True(t, ok)
	assert.Equal(t, "", prefix)
}

func TestFieldRefContextAtCursor_NoDollarBraceReturnsFalse(t *testing.T) {
	src := []byte(`api_url = "plain string"`)
	_, _, ok := fieldRefContextAtCursor(src, protocol.Position{
		Line: 0, Character: 20,
	}, positions.UTF16)
	assert.False(t, ok)
}

func TestEnumerateFieldPaths_IncludesNestedPaths(t *testing.T) {
	mt := buildAppConfigWithNestedDb(t)
	paths := enumerateFieldPaths(mt.Descriptor(), MaxFieldPathCompletionDepth)

	for _, want := range []string{"api_url", "debug", "db", "db.host", "db.port"} {
		assert.Contains(t, paths, want,
			"enumeration must include path %q", want)
	}
}

func TestEnumerateFieldPaths_ZeroDepthYieldsNothing(t *testing.T) {
	mt := buildAppConfigWithNestedDb(t)
	assert.Empty(t, enumerateFieldPaths(mt.Descriptor(), 0),
		"depth 0 produces no paths (defensive — avoids unbounded recursion if cap is misconfigured)")
}
