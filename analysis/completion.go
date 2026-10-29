// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"strings"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// CompletionFor builds a CompletionList for (uri, position), or nil
// when no completions apply.
//
// Three completion modes are wired:
//   - On an `@type` line, suggest every known message FQN from the
//     resolver. Works without a successful parse — the line text is
//     enough to identify the context.
//   - On the right-hand side of an assignment to an enum-typed field,
//     suggest the enum's defined values.
//   - Otherwise, suggest field names on the message in scope at the
//     cursor (descends into nested blocks via brace counting).
//
// The AST-aware modes prefer the current parse but fall back to the
// last-good parse when the document is mid-edit and currently
// invalid (typing `field = ` produces a single broken line, but the
// rest of the AST is still meaningful).
func (s *Workspace) CompletionFor(docURI protocol.DocumentURI, pos protocol.Position) *protocol.CompletionList {
	// Internal descriptors are always available via findType /
	// rangeTypes — no gate on s.lookup here; the no-user-registry
	// path still gets completion for protolsp.v1.* and
	// chameleon.v1.* types.
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()

	// ${field.path} completion path (C4b). Checked first because
	// the cursor inside a ${...} could be anywhere — including in
	// a value that ALSO sits on what's otherwise an assignment line.
	// The detection is cheap (line-local source scan) so leading
	// with it costs nothing in the common no-${ case.
	if list := s.completeFieldReference(doc, pos); list != nil {
		return list
	}

	// Message-type completion path: `@type ...`. Doesn't need a parsed
	// AST — line shape suffices.
	if isTypeDirectiveLine(doc.Source, pos.Line) {
		return s.completeMessageTypes()
	}

	// For the remaining modes we need an AST + a bound @type. Fall
	// back to LastGoodParse when the current parse failed.
	ast := doc.Parsed
	if ast == nil {
		ast = doc.LastGoodParse
	}
	if ast == nil {
		return nil
	}
	// effectiveTypeURL applies chameleon.schema_fqn for layer
	// files without their own @type; field-name completion uses
	// it just like hover and definition do.
	typeURL := s.effectiveTypeURL(doc)
	if typeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(typeURL))
	if err != nil {
		return nil
	}

	// Find the message in scope at the cursor.
	ctxDesc, atKeyedEntryName := contextDescriptor(ast.Entries, mt.Descriptor(), doc.Source, pos, s.positionEncoding)

	// Directly inside a keyed repeated field's block, at entry-name
	// position, the user is typing a free-form key — not a field of the
	// element message and not a field of the parent. No schema-driven
	// completion applies; returning nil avoids offering the wrong names.
	if atKeyedEntryName {
		return nil
	}

	// Enum-value completion: on the RHS of `<name> = ` for a known
	// enum-typed field.
	if fieldName := fieldAtCursorRHS(doc.Source, pos, s.positionEncoding); fieldName != "" {
		fd := ctxDesc.Fields().ByName(protoreflect.Name(fieldName))
		if fd != nil && fd.Kind() == protoreflect.EnumKind {
			return completeEnumValues(fd.Enum())
		}
		// Field exists but isn't enum (or doesn't exist) — fall
		// through to field-name completion would be wrong on the RHS;
		// return nil so the client doesn't suggest field names where
		// a value belongs.
		return nil
	}

	// Field-name completion: cursor is at the start of a new entry.
	fields := ctxDesc.Fields()
	if fields.Len() == 0 {
		return nil
	}
	items := make([]protocol.CompletionItem, 0, fields.Len())
	for i := range fields.Len() {
		items = append(items, fieldCompletionItem(fields.Get(i)))
	}
	return &protocol.CompletionList{
		IsIncomplete: false,
		Items:        items,
	}
}

// contextDescriptor walks the AST to find the MessageDescriptor whose
// fields are in scope at the cursor. Falls back to root when nothing
// matches — "nothing matches" is the common case at top level. The
// brace-depth check guards against suggesting nested fields after a
// Block has already been closed before the cursor.
//
// The second return, atKeyedEntryName, reports that the cursor sits
// directly inside a keyed repeated field's block at entry-name position
// (draft -01 §3.13) — where the next token is a free-form key, not a
// field name — so the caller suppresses field completion there.
func contextDescriptor(
	entries []pxf.Entry,
	root protoreflect.MessageDescriptor,
	source []byte,
	cursor protocol.Position,
	enc positions.Encoding,
) (protoreflect.MessageDescriptor, bool) {
	return descendContext(entries, root, source, cursor, enc)
}

// descendContext resolves the in-scope descriptor within `entries`
// (bound to `cur`) at the cursor, recursing one nesting level per call.
// It mirrors the old loop but understands keyed repeated fields: a keyed
// field's block introduces an entry layer whose sub-blocks are keyed by
// a free-form name and bind to the element message.
func descendContext(
	entries []pxf.Entry,
	cur protoreflect.MessageDescriptor,
	source []byte,
	cursor protocol.Position,
	enc positions.Encoding,
) (protoreflect.MessageDescriptor, bool) {
	b := innermostEnclosingBlock(entries, source, cursor, enc)
	if b == nil {
		return cur, false
	}
	fd := cur.Fields().ByName(protoreflect.Name(b.Name))
	if fd != nil && pxf.KeyField(fd) != nil {
		elem := fd.Message()
		// Inside the keyed block: is the cursor within one of its
		// entries' bodies, or directly at entry-name position?
		if entry, ok := innermostEnclosingKeyedEntry(b.Entries, source, cursor, enc); ok {
			return descendContext(entry.body, elem, source, cursor, enc)
		}
		return elem, true
	}
	if fd == nil || fd.Message() == nil {
		return cur, false
	}
	return descendContext(b.Entries, fd.Message(), source, cursor, enc)
}

// innermostEnclosingBlock returns the *pxf.Block among entries whose
// braces enclose the cursor and whose opening is deepest (largest start
// line), or nil when none does. Same selection the old contextDescriptor
// loop made, factored out for reuse.
func innermostEnclosingBlock(entries []pxf.Entry, source []byte, cursor protocol.Position, enc positions.Encoding) *pxf.Block {
	var best *pxf.Block
	cursorLine := cursor.Line + 1 // pxf is 1-indexed
	for _, e := range entries {
		b, ok := e.(*pxf.Block)
		if !ok {
			continue
		}
		if uint32(b.Pos.Line) > cursorLine { // #nosec G115 -- parser lines are non-negative
			continue
		}
		if !blockEnclosesCursor(source, b.Pos, cursor, enc) {
			continue
		}
		if best == nil || b.Pos.Line > best.Pos.Line {
			best = b
		}
	}
	return best
}

// innermostEnclosingKeyedEntry returns the keyed entry among `entries`
// whose body braces enclose the cursor and whose opening is deepest, or
// ok=false when the cursor is directly at entry-name level (inside the
// keyed block but not inside any entry). Handles both the `key { ... }`
// and `key = { ... }` entry spellings.
func innermostEnclosingKeyedEntry(entries []pxf.Entry, source []byte, cursor protocol.Position, enc positions.Encoding) (keyedEntry, bool) {
	var best keyedEntry
	found := false
	cursorLine := cursor.Line + 1 // pxf is 1-indexed
	for _, e := range entries {
		ke, ok := asKeyedEntry(e)
		if !ok {
			continue
		}
		if uint32(ke.pos.Line) > cursorLine { // #nosec G115 -- parser lines are non-negative
			continue
		}
		if !blockEnclosesCursor(source, ke.pos, cursor, enc) {
			continue
		}
		if !found || ke.pos.Line > best.pos.Line {
			best = ke
			found = true
		}
	}
	return best, found
}

// blockEnclosesCursor returns true when the Block's opening `{` is
// before the cursor AND the brace count from there to the cursor stays
// positive — i.e. the block hasn't been closed yet. Comments and
// string literals are skipped so a `}` inside a comment or string
// doesn't fool the count.
//
// `enc` interprets cursor.Character — UTF-8 byte offset or UTF-16
// code-unit count per the negotiated position encoding. blockStart
// is a pxf.Position; its Column is always a byte offset since the
// parser walks bytes.
func blockEnclosesCursor(source []byte, blockStart pxf.Position, cursor protocol.Position, enc positions.Encoding) bool {
	startOff := offsetForLineCol(source, blockStart.Line, blockStart.Column)
	if startOff < 0 || startOff >= len(source) {
		return false
	}
	endOff, err := positions.Offset(source, cursor, enc)
	if err != nil {
		endOff = len(source)
	}
	if endOff <= startOff {
		return false
	}
	// Walk from startOff to endOff. Track string + comment state.
	depth := 0
	i := startOff
	for i < endOff {
		c := source[i]
		switch {
		case c == '"':
			// Skip the string literal. Handle escapes; bail out cleanly
			// at end of source.
			i++
			for i < endOff {
				if source[i] == '\\' && i+1 < endOff {
					i += 2
					continue
				}
				if source[i] == '"' {
					i++
					break
				}
				i++
			}
		case c == '#':
			// Line comment to EOL.
			for i < endOff && source[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < endOff && source[i+1] == '/':
			for i < endOff && source[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < endOff && source[i+1] == '*':
			i += 2
			for i+1 < endOff && (source[i] != '*' || source[i+1] != '/') {
				i++
			}
			if i+1 < endOff {
				i += 2
			} else {
				i = endOff
			}
		case c == '{':
			depth++
			i++
		case c == '}':
			depth--
			i++
		default:
			i++
		}
	}
	return depth > 0
}

// offsetForLineCol converts a (1-indexed line, 1-indexed col) to a byte
// offset in source. Returns -1 when the line is out of range; clamps a
// past-the-end column to end-of-line.
func offsetForLineCol(source []byte, line, col int) int {
	if line < 1 || col < 1 {
		return -1
	}
	idx := 0
	curLine := 1
	for idx < len(source) && curLine < line {
		if source[idx] == '\n' {
			curLine++
		}
		idx++
	}
	if curLine != line {
		return -1
	}
	// idx is at start of target line; advance by (col-1) bytes, stopping at \n.
	for c := 1; c < col && idx < len(source); c++ {
		if source[idx] == '\n' {
			break
		}
		idx++
	}
	return idx
}

// isTypeDirectiveLine reports whether the line at 0-indexed `line`
// in source is (or is becoming) a `@type` directive — i.e. the first
// non-whitespace token on the line is `@type` (possibly mid-typing as
// `@t`, `@ty`, …, since the user invokes completion before the keyword
// is finished). Used to switch completion modes.
func isTypeDirectiveLine(source []byte, line uint32) bool {
	start := offsetForLineCol(source, int(line)+1, 1)
	if start < 0 {
		return false
	}
	// Walk to end of line / EOF.
	end := start
	for end < len(source) && source[end] != '\n' {
		end++
	}
	lineBytes := source[start:end]
	// Skip leading whitespace.
	i := 0
	for i < len(lineBytes) && (lineBytes[i] == ' ' || lineBytes[i] == '\t') {
		i++
	}
	if i >= len(lineBytes) || lineBytes[i] != '@' {
		return false
	}
	// Match "@type" or a partial prefix the user is still typing.
	// "@t", "@ty", "@typ", "@type" all qualify. Anything after `@`
	// that isn't a prefix of "type" rules out type completion (e.g.
	// `@dataset`, `@proto`).
	rest := lineBytes[i+1:]
	const keyword = "type"
	if len(rest) > len(keyword) {
		// Already past `@type` — only accept when followed by whitespace
		// or end-of-input, so `@typex` isn't treated as the directive.
		if string(rest[:len(keyword)]) != keyword {
			return false
		}
		next := rest[len(keyword)]
		return next == ' ' || next == '\t'
	}
	// rest is a partial — must be a prefix of "type".
	return string(rest) == keyword[:len(rest)]
}

// completeMessageTypes returns a CompletionList of every known message
// FQN the resolver can enumerate. The LSP client filters by the
// user's in-progress identifier.
func (s *Workspace) completeMessageTypes() *protocol.CompletionList {
	items := make([]protocol.CompletionItem, 0, 32)
	seen := make(map[string]struct{}, 32)
	add := func(mt protoreflect.MessageType) bool {
		name := string(mt.Descriptor().FullName())
		if _, dup := seen[name]; dup {
			return true
		}
		seen[name] = struct{}{}
		items = append(items, protocol.CompletionItem{
			Label:      name,
			Kind:       protocol.CompletionItemKindClass,
			Detail:     "message",
			InsertText: name,
		})
		return true
	}
	// User's registry-backed types first; internal types fill in
	// the FQNs the user's registry doesn't ship (protolsp.v1.* +
	// chameleon.v1.*) so @type completion still offers them on
	// workspaces without a registry.
	if lk := s.lookupMgr.Snapshot(); lk != nil {
		lk.RangeMessages(add)
	}
	if s.internalLookup != nil {
		s.internalLookup.RangeMessages(add)
	}
	if len(items) == 0 {
		return nil
	}
	return &protocol.CompletionList{IsIncomplete: false, Items: items}
}

// fieldAtCursorRHS detects whether the cursor is on the right-hand
// side of a `<name> = …` or `<name>: …` assignment on the current
// line, and returns the field name when so. Returns "" otherwise.
//
// Works on raw source so it functions regardless of parse success —
// during typing, the line is often mid-edit and would fail
// pxf.Parse. We just need to know "the user is typing a value for
// field X" to decide whether to offer enum-value suggestions.
func fieldAtCursorRHS(source []byte, pos protocol.Position, enc positions.Encoding) string {
	lineStart := offsetForLineCol(source, int(pos.Line)+1, 1)
	if lineStart < 0 {
		return ""
	}
	cursorOff, err := positions.Offset(source, pos, enc)
	if err != nil || cursorOff < lineStart {
		return ""
	}
	if cursorOff > len(source) {
		cursorOff = len(source)
	}
	prefix := source[lineStart:cursorOff]

	// First separator decides field vs value: `=` for top-level
	// assignment, `:` for map-entry syntax. We require the cursor be
	// strictly past the separator.
	sepIdx := -1
	for i, b := range prefix {
		if b == '=' || b == ':' {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		return ""
	}
	name := trimSpace(prefix[:sepIdx])
	if !isProtoIdent(name) {
		return ""
	}
	return name
}

// completeEnumValues emits CompletionItems for every value of the
// given enum descriptor. Order matches the descriptor's source order.
// InsertText is the bare value name; the LSP client filters by what
// the user typed.
func completeEnumValues(ed protoreflect.EnumDescriptor) *protocol.CompletionList {
	values := ed.Values()
	items := make([]protocol.CompletionItem, 0, values.Len())
	for i := range values.Len() {
		ev := values.Get(i)
		items = append(items, protocol.CompletionItem{
			Label:      string(ev.Name()),
			Kind:       protocol.CompletionItemKindEnumMember,
			Detail:     string(ed.FullName()),
			InsertText: string(ev.Name()),
		})
	}
	if len(items) == 0 {
		return nil
	}
	return &protocol.CompletionList{IsIncomplete: false, Items: items}
}

// trimSpace returns prefix with leading and trailing ASCII whitespace
// removed. Avoids strings.TrimSpace's allocation when the input is a
// short byte slice we already have on hand.
func trimSpace(b []byte) string {
	start := 0
	end := len(b)
	for start < end && isASCIIWS(b[start]) {
		start++
	}
	for end > start && isASCIIWS(b[end-1]) {
		end--
	}
	return string(b[start:end])
}

func isASCIIWS(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// isProtoIdent reports whether s is a valid protobuf identifier:
// initial letter or underscore, then letters, digits, or underscores.
// Defensive — keeps `fieldAtCursorRHS` from feeding garbage like
// `123 = ` or `field-name = ` into the descriptor lookup.
func isProtoIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// fieldCompletionItem renders a CompletionItem for one field. Detail
// shows the type; InsertText prefills "<name> = " so the user
// continues typing the value rather than the equals sign. The doc
// comment from the proto source (when present) populates the
// Documentation field.
func fieldCompletionItem(fd protoreflect.FieldDescriptor) protocol.CompletionItem {
	kind := protocol.CompletionItemKindField
	item := protocol.CompletionItem{
		Label:      string(fd.Name()),
		Kind:       kind,
		Detail:     fieldTypeLabel(fd),
		InsertText: string(fd.Name()) + " = ",
	}
	loc := fd.ParentFile().SourceLocations().ByDescriptor(fd)
	if c := strings.TrimSpace(loc.LeadingComments); c != "" {
		item.Documentation = c
	}
	return item
}
