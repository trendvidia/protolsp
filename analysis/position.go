// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// PxfPosToLSP converts a pxf.Position (1-indexed line and column —
// standard parser convention) to a protocol.Position (0-indexed,
// LSP convention). Defensive clamps for zero or negative values from
// synthetic positions (e.g. errors lacking a real source location)
// land at the start of the document.
func PxfPosToLSP(p pxf.Position) protocol.Position {
	line := uint32(0)
	col := uint32(0)
	if p.Line > 1 {
		line = uint32(p.Line - 1)
	}
	if p.Column > 1 {
		col = uint32(p.Column - 1)
	}
	return protocol.Position{Line: line, Character: col}
}

// pxfPosToRange builds a single-character LSP Range at the given pxf
// position. Useful for diagnostics that only have a point location,
// not a span — the squiggle ends up under the offending token's
// first character. The end of the range is one character beyond the
// start so editors that require a non-empty range still render the
// diagnostic.
func pxfPosToRange(p pxf.Position) protocol.Range {
	start := PxfPosToLSP(p)
	end := start
	end.Character++
	return protocol.Range{Start: start, End: end}
}
