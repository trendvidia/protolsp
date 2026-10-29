// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C4b second piece: completion when the cursor is inside a
// ${...} interpolation token. The user types `${db.` and asks
// for completion → we enumerate the bound message's reachable
// dotted field paths and return the ones whose path prefix
// matches.
//
// The detection is purely source-based: scan backward in the
// current line from the cursor, find `${` without an intervening
// `}` or newline. The prefix between `${` and the cursor is what
// the client filters against. TextEdit replaces exactly that
// prefix so completion works correctly when the user has typed
// dots (which would otherwise be treated as a word boundary by
// the client's default heuristic and lead to "db.db.host" after
// inserting "db.host").
//
// Scope (intentional):
//   - Single line only. Multi-line ${...} spans (triple-quoted
//     strings) are rare and not handled here.
//   - Enumerates paths up to MaxFieldPathCompletionDepth to avoid
//     pathological output on recursive / deeply nested schemas.
//   - Skips the env / ctx context interpolation suggestions —
//     those are runtime sources, not field references.

package analysis

import (
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/positions"
)

// MaxFieldPathCompletionDepth caps recursion through message-typed
// fields when enumerating completion candidates. 5 covers
// reasonable schemas; deeper paths can be typed manually. The cap
// also prevents the recursion-guard map from ballooning on
// pathological schemas that self-reference through multiple message
// types.
const MaxFieldPathCompletionDepth = 5

// completeFieldReference returns the completion list when the
// cursor at pos is inside a ${...} token, or nil otherwise.
// Caller (CompletionFor) checks this BEFORE the existing
// @type / enum-value / field-name paths.
func (s *Workspace) completeFieldReference(doc *Document, pos protocol.Position) *protocol.CompletionList {
	if s == nil || doc == nil {
		return nil
	}
	prefix, start, ok := fieldRefContextAtCursor(doc.Source, pos, s.positionEncoding)
	if !ok {
		return nil
	}
	ast := doc.Parsed
	if ast == nil {
		ast = doc.LastGoodParse
	}
	if ast == nil || ast.TypeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(ast.TypeURL))
	if err != nil || mt == nil {
		return nil
	}
	paths := enumerateFieldPaths(mt.Descriptor(), MaxFieldPathCompletionDepth)
	if len(paths) == 0 {
		return nil
	}
	// Replacement range: from `${` content start to cursor. The
	// client uses this to swap the user's typed prefix with the
	// selected completion's NewText cleanly.
	editRange := protocol.Range{
		Start: protocol.Position{Line: pos.Line, Character: start},
		End:   pos,
	}
	items := make([]protocol.CompletionItem, 0, len(paths))
	for _, p := range paths {
		items = append(items, protocol.CompletionItem{
			Label:      p,
			Kind:       protocol.CompletionItemKindField,
			Detail:     "${" + p + "}",
			FilterText: p,
			TextEdit: &protocol.TextEdit{
				Range:   editRange,
				NewText: p,
			},
		})
	}
	// Surface the prefix on the result for tests / debuggers — no-op
	// in production. (We can't add it to the CompletionList struct,
	// but we keep the variable in scope so the compiler doesn't drop
	// the detection result.)
	_ = prefix
	return &protocol.CompletionList{IsIncomplete: false, Items: items}
}

// fieldRefContextAtCursor reports whether the cursor sits inside a
// ${...} token on its line, and when so returns:
//   - prefix: the text the user has typed since `${`
//   - start: the LSP Character of the first byte after `${` on the
//     cursor's line (used to build the replacement TextEdit range)
//   - ok: true when in context
//
// Detection is line-local: a `\n` between `${` and the cursor
// breaks the context (LSP's default behavior for triple-quoted
// multi-line strings is uncommon enough to defer). A `}` between
// `${` and the cursor closes the ref — also breaks context.
func fieldRefContextAtCursor(source []byte, pos protocol.Position, enc positions.Encoding) (prefix string, start uint32, ok bool) {
	lineStart := offsetForLineCol(source, int(pos.Line)+1, 1)
	if lineStart < 0 {
		return "", 0, false
	}
	cursorOff, err := positions.Offset(source, pos, enc)
	if err != nil || cursorOff < lineStart {
		return "", 0, false
	}
	if cursorOff > len(source) {
		cursorOff = len(source)
	}
	// Scan backward from just before the cursor; bail on } or newline.
	for i := cursorOff - 1; i >= lineStart; i-- {
		if i > lineStart && source[i-1] == '$' && source[i] == '{' {
			contentStart := i + 1
			// `start` goes into the TextEdit Range we return to the
			// editor — must be in the negotiated encoding. Convert
			// contentStart's byte offset to an LSP character via
			// LineCol so UTF-16 clients count surrogate pairs
			// correctly when the prefix string contains non-BMP.
			_, ch := positions.LineCol(source, contentStart, enc)
			return string(source[contentStart:cursorOff]), ch, true
		}
		c := source[i]
		if c == '}' || c == '\n' || c == '"' {
			// `}` closes the ref; `"` puts us outside the string
			// literal (we walked past the closing quote going
			// backward); `\n` means the ref spans lines which
			// this single-line detector doesn't handle.
			return "", 0, false
		}
	}
	return "", 0, false
}

// enumerateFieldPaths returns every reachable dotted field path on
// the descriptor up to maxDepth. Used as completion candidates for
// the ${...} reference context. Walk order is descriptor field-
// declaration order, depth-first — so e.g. db comes before
// db.host, db.host comes before db.port.
//
// Recursion guard: the seen map prevents infinite loops on
// schemas with self-referential nested message types
// (`message Tree { Tree left = 1; ... }`). Without it, the walk
// would never terminate on those structures.
func enumerateFieldPaths(desc protoreflect.MessageDescriptor, maxDepth int) []string {
	if desc == nil || maxDepth <= 0 {
		return nil
	}
	var out []string
	seen := map[protoreflect.FullName]bool{}
	var walk func(d protoreflect.MessageDescriptor, prefix string, depth int)
	walk = func(d protoreflect.MessageDescriptor, prefix string, depth int) {
		if d == nil || depth > maxDepth {
			return
		}
		fullName := d.FullName()
		if seen[fullName] {
			return
		}
		seen[fullName] = true
		defer delete(seen, fullName)

		fields := d.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			name := string(fd.Name())
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			out = append(out, path)
			if fd.Kind() == protoreflect.MessageKind && fd.Message() != nil {
				walk(fd.Message(), path, depth+1)
			}
		}
	}
	walk(desc, "", 1)
	return out
}
