// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// R1 of the dataset-regions design. Line-based pre-pass that
// identifies every `@dataset <type> { ... }` block in the source
// and surfaces it as a DatasetRegion. Resilient to mid-edit
// states: an unclosed @dataset still produces a region (whose
// range extends to end-of-document) so the editor can render its
// "huge region" affordance during typing.
//
// String-aware brace counting: a `{` inside `"..."` (single- or
// triple-quoted) does NOT advance the depth. Comments aren't
// handled in R1 (PXF supports `//` and `#` line comments + `/* */`
// blocks); the realistic comment-with-braces case is rare enough
// to defer.
//
// R1 scope: detection + request. NO change notification yet (R2),
// NO AST integration for rowCount / resolvable yet (R2), NO
// per-row validation (R4 — the prerequisite incremental sync
// landed in I1).

package analysis

import (
	"strings"

	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

const (
	// MethodDatasetRegions is the custom request returning the
	// current DatasetRegion list for a document. Pull-style; the
	// editor calls at connect / cache invalidation. R2 adds a
	// push notification on structural changes.
	MethodDatasetRegions = "protolsp/datasetRegions"

	// defaultHugeRegionBytes is the byte-size threshold above
	// which a region is marked Huge by default. Per the dataset-
	// regions design's §"Threshold ownership": 100 KB or 1000
	// rows, whichever hits first. R1 only knows byte size (no
	// row count until R2's AST pass), so the row component is
	// ignored.
	defaultHugeRegionBytes = 100 * 1024
)

// DatasetRegion captures one @dataset block found by the
// line-based scanner. Range covers the `@dataset` directive line
// through the closing `}` (or end-of-document for unclosed
// blocks). TypeFQN is the token after `@dataset` on the directive
// line, or "" when there is none. Resolvable and RowCount stay zero
// / -1 until the AST pass fills them in. Resolvable is true when the
// row type is known, so a region bound to an anonymous @proto has an
// empty TypeFQN and is Resolvable: that type's name is protolsp's
// own, and it is not sent (#292).
type DatasetRegion struct {
	Range      protocol.Range `json:"range"`
	TypeFQN    string         `json:"typeFQN"`
	Resolvable bool           `json:"resolvable"`
	RowCount   int            `json:"rowCount"` // -1 when parse hasn't determined it
	ByteSize   int            `json:"byteSize"`
	Huge       bool           `json:"huge"`
}

// DatasetRegionsParams is the request shape for [MethodDatasetRegions].
type DatasetRegionsParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
}

// DatasetRegionsResult is the response shape. Always returns an
// empty (non-nil) slice when no regions are present — LSP
// convention: empty array, not null.
type DatasetRegionsResult struct {
	Regions []DatasetRegion `json:"regions"`
}

// DetectDatasetRegions runs the line-based scanner against the
// document's source and returns the list of regions found.
// hugeThresholdBytes governs the Huge flag; pass 0 to use the
// default. Pure function — no Document mutation here; the caller
// stores the result on doc.DatasetRegions.
// enc controls how Range positions in the returned regions
// encode their Character component (UTF-16 code units vs UTF-8
// byte offsets). The negotiated value flows from
// Server.positionEncoding.
func DetectDatasetRegions(source []byte, hugeThresholdBytes int, enc positions.Encoding) []DatasetRegion {
	if hugeThresholdBytes <= 0 {
		hugeThresholdBytes = defaultHugeRegionBytes
	}
	var out []DatasetRegion

	// Walk the source byte by byte. When we're at the start of a
	// line (i.e. immediately after a `\n` or at byte 0), look for
	// the `@dataset` directive pattern. When found, brace-count
	// forward to find the matching closing `}`.
	i := 0
	curLine := uint32(0)
	for i < len(source) {
		// Skip leading whitespace on the line.
		j := i
		for j < len(source) && (source[j] == ' ' || source[j] == '\t') {
			j++
		}
		if j < len(source) && strings.HasPrefix(string(source[j:]), "@dataset") {
			region := scanDatasetFromAt(source, j, curLine, hugeThresholdBytes, enc)
			out = append(out, region)
			// Continue scanning AFTER this region. The end position
			// might be on a later line — convert it to a byte index.
			afterByte, afterLine := byteIndexAt(source, region.Range.End, enc)
			i = afterByte
			curLine = afterLine
			continue
		}
		// Advance to the next line.
		for i < len(source) && source[i] != '\n' {
			i++
		}
		if i < len(source) && source[i] == '\n' {
			i++
			curLine++
		}
	}
	return out
}

// scanDatasetFromAt parses one @dataset block starting at byte
// position `atIdx` (which must be the start of the `@` character).
// `atLine` is the 0-indexed line number of that position.
//
// Handles both PXF @dataset surface forms (draft §3.4.4):
//
//   - Block form: `@dataset Trade { { id = "X" } { id = "Y" } }`
//     — brace-count to the matching `}`.
//   - Tuple form: `@dataset Trade(id, qty)\n("X", 10)\n("Y", 20)\n`
//     — the header line declares columns; subsequent lines that
//     start with `(` are row tuples. The region ends at the first
//     line whose first non-whitespace character isn't `(`.
//
// String literals (single + triple quoted) are treated as opaque
// in both forms so `{`, `}`, `(`, `)` inside strings don't confuse
// the counter.
//
// The R1 scanner extracts an approximate TypeFQN by stopping at
// the first delimiter (`(`, `{`, whitespace, or newline). The R2
// AST pass overwrites TypeFQN with the canonical
// pxf.DatasetDirective.Type when the parse succeeds.
func scanDatasetFromAt(source []byte, atIdx int, atLine uint32, hugeThresholdBytes int, enc positions.Encoding) DatasetRegion {
	const directive = "@dataset"
	bodyStart := atIdx + len(directive)
	_, startCol := positions.LineCol(source, atIdx, enc)

	// Skip leading whitespace after @dataset.
	cursor := bodyStart
	for cursor < len(source) && (source[cursor] == ' ' || source[cursor] == '\t') {
		cursor++
	}
	// Capture the type FQN — stop at the first delimiter that
	// ends a Go-style identifier ('(' for tuple form; '{' for
	// block form; whitespace or newline for malformed cases).
	typeStart := cursor
	for cursor < len(source) {
		c := source[cursor]
		if c == '(' || c == '{' || c == ' ' || c == '\t' || c == '\n' {
			break
		}
		cursor++
	}
	typeFQN := string(source[typeStart:cursor])

	// Skip whitespace between type and form-discriminator.
	for cursor < len(source) && (source[cursor] == ' ' || source[cursor] == '\t') {
		cursor++
	}

	var endByte int
	switch {
	case cursor < len(source) && source[cursor] == '{':
		// Block form: brace-count to matching `}`.
		closeIdx, foundClose := findMatchingClose(source, cursor)
		endByte = closeIdx + 1
		if !foundClose {
			endByte = len(source)
		}
	case cursor < len(source) && source[cursor] == '(':
		// Tuple form: header `(col1, col2, ...)` then row lines.
		endByte = scanTupleDatasetEnd(source, cursor)
	default:
		// Unclosed / malformed (user paused mid-directive).
		// Extend to end-of-document so the editor's placeholder
		// UI still has a target.
		endByte = len(source)
	}

	endLine, endCol := byteToLineCol(source, endByte, enc)
	byteSize := endByte - atIdx
	return DatasetRegion{
		Range: protocol.Range{
			Start: protocol.Position{Line: atLine, Character: startCol},
			End:   protocol.Position{Line: endLine, Character: endCol},
		},
		TypeFQN:    typeFQN,
		Resolvable: false,
		RowCount:   -1,
		ByteSize:   byteSize,
		Huge:       byteSize >= hugeThresholdBytes,
	}
}

// scanTupleDatasetEnd handles the tuple form of @dataset. cursor
// starts at the `(` of the column list. We:
//
//  1. Scan to the closing `)` of the header (rejecting `\n` —
//     unclosed header is treated as the entire remaining source).
//  2. Skip the rest of the header line.
//  3. Read lines as long as each starts (after whitespace) with
//     `(`. The first line that doesn't ends the region.
//  4. Return the byte offset of where the region ends (the start
//     of the first non-tuple line, or EOF).
//
// Treats string literals as opaque so `)` inside a string doesn't
// terminate the header prematurely.
func scanTupleDatasetEnd(source []byte, openParen int) int {
	// Close the header on this line.
	i := openParen + 1
	for i < len(source) {
		switch source[i] {
		case '"':
			if i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
				i = skipTripleQuoted(source, i+3)
			} else {
				i = skipSingleQuoted(source, i+1)
			}
			continue
		case ')':
			i++
			goto headerClosed
		case '\n':
			// Unclosed header — extend to EOF.
			return len(source)
		}
		i++
	}
	return len(source)

headerClosed:
	// Skip to end of header line.
	for i < len(source) && source[i] != '\n' {
		i++
	}
	if i < len(source) && source[i] == '\n' {
		i++
	}

	// Read tuple rows. A line that doesn't start with `(` (after
	// optional whitespace) ends the region. Blank lines are
	// permitted between rows but a non-empty non-`(` line ends.
	for i < len(source) {
		lineStart := i
		j := i
		for j < len(source) && (source[j] == ' ' || source[j] == '\t') {
			j++
		}
		// Blank line — keep going.
		if j < len(source) && source[j] == '\n' {
			i = j + 1
			continue
		}
		// End of source.
		if j >= len(source) {
			return j
		}
		// Non-tuple line — region ends just before it.
		if source[j] != '(' {
			return lineStart
		}
		// Tuple row — advance to next line.
		for i < len(source) && source[i] != '\n' {
			// String literals on row lines too.
			if source[i] == '"' {
				if i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
					i = skipTripleQuoted(source, i+3)
				} else {
					i = skipSingleQuoted(source, i+1)
				}
				continue
			}
			i++
		}
		if i < len(source) && source[i] == '\n' {
			i++
		}
	}
	return i
}

// findMatchingClose scans forward from the opening-brace index
// counting `{` / `}` while treating string literals as opaque.
// Returns the byte index of the matching `}` and true; or
// (len(source)-ish, false) when EOF arrived first.
//
// String-literal handling covers:
//   - `"..."` (single-quoted) — respects \" escapes
//   - `"""..."""` (triple-quoted)
//   - `b"..."` byte-string prefix (the b is just a regular char to
//     the scanner; the inner `"..."` parses normally)
//
// Comments are NOT handled in R1 (rare to have `{`/`}` in PXF
// comments; future expansion if it bites).
func findMatchingClose(source []byte, openIdx int) (int, bool) {
	depth := 1
	i := openIdx + 1
	for i < len(source) {
		switch source[i] {
		case '"':
			// Triple-quoted?
			if i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
				i = skipTripleQuoted(source, i+3)
			} else {
				i = skipSingleQuoted(source, i+1)
			}
			continue
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
		i++
	}
	return len(source), false
}

// skipSingleQuoted advances past a `"..."` string body starting at
// `i` (the byte after the opening quote). Returns the byte index
// AFTER the closing quote (or len(source) if unterminated).
// Honors `\` as an escape — `\"` does NOT terminate.
func skipSingleQuoted(source []byte, i int) int {
	for i < len(source) {
		switch source[i] {
		case '\\':
			i += 2 // skip the escape pair
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return i
}

// skipTripleQuoted advances past a `"""..."""` string body. Returns
// the byte index AFTER the closing `"""` (or len(source) if
// unterminated).
func skipTripleQuoted(source []byte, i int) int {
	for i+2 < len(source) {
		if source[i] == '"' && source[i+1] == '"' && source[i+2] == '"' {
			return i + 3
		}
		i++
	}
	return len(source)
}

// byteToLineCol converts a byte offset to an LSP (line, character)
// position in the given encoding. Character is UTF-8 byte offset
// within the line for UTF-8; UTF-16 code-unit count for UTF-16.
// Used to build DatasetRegion.Range positions that go to the
// editor — must match what the client negotiated at initialize.
func byteToLineCol(source []byte, offset int, enc positions.Encoding) (uint32, uint32) {
	return positions.LineCol(source, offset, enc)
}

// byteIndexAt is the inverse of byteToLineCol — converts an LSP
// position to a byte offset + the line it landed on. Used by the
// scanner to advance past a region without recomputing positions.
// Interprets pos.Character per the negotiated encoding.
func byteIndexAt(source []byte, pos protocol.Position, enc positions.Encoding) (int, uint32) {
	off, err := positions.Offset(source, pos, enc)
	if err != nil {
		off = len(source)
	}
	return off, pos.Line
}
