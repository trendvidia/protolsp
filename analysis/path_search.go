// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// pathHit describes one Assignment or Block whose dotted path
// matches a search. uri/range together form an LSP Location; key
// is the leaf identifier (Assignment.Key or Block.Name) and its
// length is the on-source length of the identifier — useful for
// rename TextEdits that replace just the identifier rather than
// the whole entry.
type pathHit struct {
	uri   uri.URI
	leaf  string
	pos   pxf.Position
	rang  protocol.Range
	isKey bool // true for Assignment, false for Block
}

// pathAtPosition walks a parsed document and returns the dotted
// path under the cursor plus the identifier-only range. Returns
// ("", _, false) when the cursor isn't on an entry the walker
// recognises (assignment or block name).
//
// Path construction mirrors findHoverTargetWithPath: top-level
// `api_url` ⇒ "api_url"; cursor on `host` inside `db { host = X }`
// ⇒ "db.host"; cursor on the `db` token ⇒ "db".
func pathAtPosition(
	source []byte,
	encoding positions.Encoding,
	entries []pxf.Entry,
	cursor protocol.Position,
) (path string, leafRange protocol.Range, leaf string, ok bool) {
	target := cursor.Line + 1 // pxf lines are 1-indexed
	return pathAtPositionRec(source, encoding, entries, target, "")
}

func pathAtPositionRec(
	source []byte,
	encoding positions.Encoding,
	entries []pxf.Entry,
	cursorLine uint32,
	prefix string,
) (string, protocol.Range, string, bool) {
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if uint32(n.Pos.Line) == cursorLine { // #nosec G115 -- non-negative
				return joinPath(prefix, n.Key), identRange(source, encoding, n.Pos, n.Key), n.Key, true
			}
		case *pxf.Block:
			if uint32(n.Pos.Line) == cursorLine { // #nosec G115 -- non-negative
				return joinPath(prefix, n.Name), identRange(source, encoding, n.Pos, n.Name), n.Name, true
			}
			if p, r, l, ok := pathAtPositionRec(source, encoding, n.Entries, cursorLine, joinPath(prefix, n.Name)); ok {
				return p, r, l, true
			}
		}
	}
	return "", protocol.Range{}, "", false
}

// findPathHits walks a document and collects every Assignment / Block
// whose dotted path equals target. Used by references + rename to
// gather the set of edits to apply.
func findPathHits(
	source []byte,
	encoding positions.Encoding,
	docURI uri.URI,
	entries []pxf.Entry,
	target string,
) []pathHit {
	var hits []pathHit
	collectPathHits(source, encoding, docURI, entries, target, "", &hits)
	return hits
}

func collectPathHits(
	source []byte,
	encoding positions.Encoding,
	docURI uri.URI,
	entries []pxf.Entry,
	target string,
	prefix string,
	out *[]pathHit,
) {
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if joinPath(prefix, n.Key) == target {
				*out = append(*out, pathHit{
					uri:   docURI,
					leaf:  n.Key,
					pos:   n.Pos,
					rang:  identRange(source, encoding, n.Pos, n.Key),
					isKey: true,
				})
			}
		case *pxf.Block:
			if joinPath(prefix, n.Name) == target {
				*out = append(*out, pathHit{
					uri:   docURI,
					leaf:  n.Name,
					pos:   n.Pos,
					rang:  identRange(source, encoding, n.Pos, n.Name),
					isKey: false,
				})
			}
			collectPathHits(source, encoding, docURI, n.Entries, target, joinPath(prefix, n.Name), out)
		}
	}
}

func joinPath(prefix, leaf string) string {
	if prefix == "" {
		return leaf
	}
	return prefix + "." + leaf
}

func identRange(src []byte, enc positions.Encoding, pos pxf.Position, name string) protocol.Range {
	line, character := positions.LineCol(src, pos.Offset, enc)
	return protocol.Range{
		Start: protocol.Position{Line: line, Character: character},
		End:   protocol.Position{Line: line, Character: character + uint32(len(name))}, // #nosec G115 -- ids are short
	}
}

// IsValidPxfIdentifier validates the new name passed to a rename
// request against the PXF identifier grammar (letter / underscore
// start, alnum / underscore body). Rejecting at the server side
// surfaces a clear error in the editor rather than producing edits
// the parser will then reject.
func IsValidPxfIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !isIdentStart(r) {
				return false
			}
			continue
		}
		if !isIdentBody(r) {
			return false
		}
	}
	// PXF doesn't reserve identifier words — true/false/null are
	// parsed as bareword values, not keywords, so renaming a field
	// to `true` is grammatically fine. Reject anyway: a field
	// named `true` is the kind of accident that turns into a
	// debugging mystery later.
	switch name {
	case "true", "false", "null":
		return false
	}
	return !strings.ContainsAny(name, " \t\n")
}

func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isIdentBody(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}
