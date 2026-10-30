// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"sort"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// MethodSemanticTokensFull is the LSP method for the full-document
// semantic-tokens request. Re-exported from protocol so the dispatcher
// matches against a single source of truth.
const MethodSemanticTokensFull = protocol.MethodTextDocumentSemanticTokensFull

// SemanticTokens is the layered, schema-driven highlight surface
// (LSP 3.16+). The legend below maps each token type / modifier name
// to a numeric index; on the wire we emit those indices in the
// SemanticTokens.Data uint32 array.
//
// Type indices and modifier bit positions are stable — clients
// negotiate by name on initialize and look the index up. Adding a
// new type/modifier means appending to the slice, never reordering.
var (
	SemanticTokenTypes = []protocol.SemanticTokenTypes{
		protocol.SemanticTokenTypesMacro,      // 0  - @type, @dataset, @proto, @<name> directive heads
		protocol.SemanticTokenTypesNamespace,  // 1  - dotted type URLs (acme.demo.v1.Greeting)
		protocol.SemanticTokenTypesProperty,   // 2  - Assignment.Key, MapEntry.Key
		protocol.SemanticTokenTypesStruct,     // 3  - Block.Name (message-typed nested block)
		protocol.SemanticTokenTypesString,     // 4  - StringVal literals
		protocol.SemanticTokenTypesNumber,     // 5  - IntVal, FloatVal, DurationVal, TimestampVal
		protocol.SemanticTokenTypesKeyword,    // 6  - true / false / null
		protocol.SemanticTokenTypesEnumMember, // 7  - IdentVal that resolves to an enum value
		// .proto RFC-001 constructs. `decorator` is the LSP
		// 3.17 standard type for @-annotations.
		protocol.SemanticTokenTypes("decorator"), // 8  - @name annotation-use sites in .proto
	}

	semanticTokenModifiers = []protocol.SemanticTokenModifiers{
		protocol.SemanticTokenModifiersDeprecated,     // bit 0 - field marked [deprecated=true]
		protocol.SemanticTokenModifiersDefaultLibrary, // bit 1 - field from a protolsp-internal descriptor
	}
)

const (
	stMacro      uint32 = 0
	stNamespace  uint32 = 1
	stProperty   uint32 = 2
	stStruct     uint32 = 3
	stString     uint32 = 4
	stNumber     uint32 = 5
	stKeyword    uint32 = 6
	stEnumMember uint32 = 7
	stDecorator  uint32 = 8

	smDeprecated     uint32 = 1 << 0
	smDefaultLibrary uint32 = 1 << 1
)

// SemanticTokensLegend exposes the legend the server publishes at
// initialize. Public so tests can assert the on-wire indices stay
// aligned with the stMacro / stNamespace / ... constants above.
func SemanticTokensLegend() protocol.SemanticTokensLegend {
	return protocol.SemanticTokensLegend{
		TokenTypes:     tokenNames(SemanticTokenTypes),
		TokenModifiers: tokenNames(semanticTokenModifiers),
	}
}

func tokenNames[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, t := range in {
		out[i] = string(t)
	}
	return out
}

// SemanticTokensFor builds the SemanticTokens response for a document
// or returns nil for an unopened / unparseable file.
func (s *Workspace) SemanticTokensFor(docURI uri.URI) *protocol.SemanticTokens {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	// .proto documents highlight from the retained protocompile AST:
	// RFC-001 keywords + annotation-use sites only;
	// stock proto syntax stays with the editor's grammar layer. The
	// pxf walk below never applies to them (Parsed is always nil for
	// .proto docs).
	if doc.protoAST != nil {
		b := &semanticTokenBuilder{source: doc.Source, encoding: s.positionEncoding}
		b.walkProtoFile(doc.protoAST)
		return &protocol.SemanticTokens{Data: b.encode()}
	}
	if doc.Parsed == nil {
		return &protocol.SemanticTokens{Data: []uint32{}}
	}
	var rootMd protoreflect.MessageDescriptor
	if doc.Parsed.TypeURL != "" {
		if mt, err := s.findTypeForDoc(doc, protoreflect.FullName(doc.Parsed.TypeURL)); err == nil && mt != nil {
			rootMd = mt.Descriptor()
		}
	}
	b := &semanticTokenBuilder{source: doc.Source, encoding: s.positionEncoding}
	b.walkDocument(doc.Parsed, rootMd)
	return &protocol.SemanticTokens{Data: b.encode()}
}

// semanticToken is the pre-encode representation of one highlight
// span. line/char are absolute (0-indexed LSP positions); the
// builder converts to the delta-encoded uint32 quintuple at encode().
type semanticToken struct {
	line, char, length, tokType, modifiers uint32
}

// semanticTokenBuilder accumulates tokens during the AST walk and
// emits them in the LSP delta-encoded form. Single-use per request.
type semanticTokenBuilder struct {
	source   []byte
	encoding positions.Encoding
	tokens   []semanticToken
}

// add records one token from a pxf Position + a byte length on that
// line. Zero-length tokens are skipped (LSP rejects them and they
// carry no visual signal anyway).
func (b *semanticTokenBuilder) add(pos pxf.Position, byteLen int, tokType, modifiers uint32) {
	if byteLen <= 0 {
		return
	}
	line, character := positions.LineCol(b.source, pos.Offset, b.encoding)
	endLine, endCharacter := positions.LineCol(b.source, pos.Offset+byteLen, b.encoding)
	// LSP forbids tokens that span multiple lines. The lexer
	// shouldn't produce any here (the only multi-line literal is
	// the bytes literal, which we don't emit a token for), so this
	// is defensive: silently drop a malformed entry rather than
	// emit invalid data.
	if endLine != line {
		return
	}
	length := endCharacter - character
	if length == 0 {
		return
	}
	b.tokens = append(b.tokens, semanticToken{
		line:      line,
		char:      character,
		length:    length,
		tokType:   tokType,
		modifiers: modifiers,
	})
}

func (b *semanticTokenBuilder) walkDocument(doc *pxf.Document, md protoreflect.MessageDescriptor) {
	// @type directive. Always at the top of the file when present
	// (per the v1 grammar). We tokenize from offset 0 to the first
	// newline — splitting into "@type" (macro) + TypeURL (namespace)
	// is straightforward via the source bytes.
	if doc.TypeURL != "" {
		b.emitTypeDirective(doc.TypeURL)
	}
	for _, d := range doc.Directives {
		b.emitDirective(d)
	}
	for _, d := range doc.Datasets {
		b.emitDataset(d)
	}
	for _, p := range doc.Protos {
		b.emitProto(p)
	}
	b.walkEntries(doc.Entries, md)
}

func (b *semanticTokenBuilder) emitTypeDirective(typeURL string) {
	// The @type directive only stores the TypeURL string; its
	// position isn't in the AST. We re-find it from the source so
	// the highlight lands on the right span — searching for the
	// literal "@type" prefix is unambiguous (the grammar forbids it
	// outside the directive head).
	headOff := indexAt(b.source, "@type", 0)
	if headOff < 0 {
		return
	}
	headPos := pxf.Position{Offset: headOff}
	b.add(headPos, len("@type"), stMacro, 0)
	urlOff := indexAt(b.source, typeURL, headOff+len("@type"))
	if urlOff < 0 {
		return
	}
	b.add(pxf.Position{Offset: urlOff}, len(typeURL), stNamespace, 0)
}

func (b *semanticTokenBuilder) emitDirective(d pxf.Directive) {
	// Pos points at the `@` of the directive head. The head occupies
	// "@" + d.Name (no spaces inside per grammar).
	b.add(d.Pos, len("@")+len(d.Name), stMacro, 0)
	// Highlight prefixes (the dotted type identifier, e.g.
	// `chameleon.v1.LayerHeader` in `@header chameleon.v1.LayerHeader`).
	// Prefix positions aren't in the AST; we scan the source from
	// just after the directive head for whitespace-separated tokens
	// matching each prefix.
	cursor := d.Pos.Offset + len("@") + len(d.Name)
	for _, p := range d.Prefixes {
		off := indexAt(b.source, p, cursor)
		if off < 0 {
			break
		}
		b.add(pxf.Position{Offset: off}, len(p), stNamespace, 0)
		cursor = off + len(p)
	}
}

func (b *semanticTokenBuilder) emitDataset(d pxf.DatasetDirective) {
	b.add(d.Pos, len("@dataset"), stMacro, 0)
	cursor := d.Pos.Offset + len("@dataset")
	if d.Type != "" {
		off := indexAt(b.source, d.Type, cursor)
		if off >= 0 {
			b.add(pxf.Position{Offset: off}, len(d.Type), stNamespace, 0)
			cursor = off + len(d.Type)
		}
	}
	for _, col := range d.Columns {
		off := indexAt(b.source, col, cursor)
		if off < 0 {
			break
		}
		b.add(pxf.Position{Offset: off}, len(col), stProperty, 0)
		cursor = off + len(col)
	}
}

func (b *semanticTokenBuilder) emitProto(p pxf.ProtoDirective) {
	b.add(p.Pos, len("@proto"), stMacro, 0)
	if p.TypeName != "" {
		off := indexAt(b.source, p.TypeName, p.Pos.Offset+len("@proto"))
		if off >= 0 {
			b.add(pxf.Position{Offset: off}, len(p.TypeName), stNamespace, 0)
		}
	}
}

func (b *semanticTokenBuilder) walkEntries(entries []pxf.Entry, md protoreflect.MessageDescriptor) {
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			b.emitAssignment(n, md)
		case *pxf.MapEntry:
			b.add(n.Pos, len(n.Key), stProperty, 0)
			b.emitValue(n.Value, nil)
		case *pxf.Block:
			b.emitBlock(n, md)
		}
	}
}

func (b *semanticTokenBuilder) emitAssignment(a *pxf.Assignment, md protoreflect.MessageDescriptor) {
	var fd protoreflect.FieldDescriptor
	if md != nil {
		fd = md.Fields().ByName(protoreflect.Name(a.Key))
	}
	mods := uint32(0)
	if fd != nil {
		if fd.Options() != nil {
			// Deprecation is a stable boolean on FieldOptions in
			// descriptor.proto. Empty options or no proto → no
			// modifier; we'd rather under-decorate than mislabel.
			if opts, ok := fd.Options().(interface{ GetDeprecated() bool }); ok && opts.GetDeprecated() {
				mods |= smDeprecated
			}
		}
		// Mark fields that come from a protolsp-internal file
		// (`protolsp.v1.*`, `chameleon.v1.*`) so editors can style
		// them distinctly — useful in `.protolsp.pxf` and
		// `chain.pxf` files where every field is internal.
		if isInternalDescriptorFile(fd.ParentFile().Path()) {
			mods |= smDefaultLibrary
		}
	}
	b.add(a.Pos, len(a.Key), stProperty, mods)
	b.emitValue(a.Value, fd)
}

func (b *semanticTokenBuilder) emitBlock(blk *pxf.Block, md protoreflect.MessageDescriptor) {
	var fd protoreflect.FieldDescriptor
	if md != nil {
		fd = md.Fields().ByName(protoreflect.Name(blk.Name))
	}
	mods := uint32(0)
	if fd != nil && isInternalDescriptorFile(fd.ParentFile().Path()) {
		mods |= smDefaultLibrary
	}
	b.add(blk.Pos, len(blk.Name), stStruct, mods)
	var childMd protoreflect.MessageDescriptor
	if fd != nil && fd.Message() != nil {
		childMd = fd.Message()
	}
	b.walkEntries(blk.Entries, childMd)
}

func (b *semanticTokenBuilder) emitValue(v pxf.Value, fd protoreflect.FieldDescriptor) {
	switch n := v.(type) {
	case *pxf.StringVal:
		// Re-scan source to find the matching closing quote — the
		// AST stores the decoded string in Value but the highlight
		// must cover the source bytes (with quotes + escapes).
		if span := stringSpanFromSource(b.source, n.Pos.Offset); span > 0 {
			b.add(n.Pos, span, stString, 0)
		}
	case *pxf.IntVal:
		b.add(n.Pos, numberLen(n.Pos, n.End, n.Raw), stNumber, 0)
	case *pxf.FloatVal:
		b.add(n.Pos, numberLen(n.Pos, n.End, n.Raw), stNumber, 0)
	case *pxf.DurationVal:
		b.add(n.Pos, len(n.Raw), stNumber, 0)
	case *pxf.TimestampVal:
		b.add(n.Pos, len(n.Raw), stNumber, 0)
	case *pxf.BoolVal:
		w := len("true")
		if !n.Value {
			w = len("false")
		}
		b.add(n.Pos, w, stKeyword, 0)
	case *pxf.NullVal:
		b.add(n.Pos, len("null"), stKeyword, 0)
	case *pxf.IdentVal:
		// Bare identifiers used as values are almost always enum
		// names. When the bound field IS an enum and the ident
		// matches a known value, mark as enumMember. Otherwise fall
		// back to keyword so the editor still styles it.
		tokType := uint32(stKeyword)
		if fd != nil && fd.Enum() != nil {
			if fd.Enum().Values().ByName(protoreflect.Name(n.Name)) != nil {
				tokType = stEnumMember
			}
		}
		b.add(n.Pos, len(n.Name), tokType, 0)
	case *pxf.ListVal:
		for _, e := range n.Elements {
			b.emitValue(e, fd)
		}
	case *pxf.BlockVal:
		// Anonymous block — recurse for its entries with no
		// descriptor (we don't know the enclosing message here for
		// inline list / map blocks).
		b.walkEntries(n.Entries, nil)
	}
}

// encode converts the accumulated tokens into the LSP delta-encoded
// uint32 stream: every 5 elements describe one token as
// (deltaLine, deltaStart, length, tokenType, modifierBitset).
func (b *semanticTokenBuilder) encode() []uint32 {
	if len(b.tokens) == 0 {
		return []uint32{}
	}
	// Sort by (line, char) so the delta encoding is monotonic.
	// The AST walk is depth-first in source order, but nested
	// blocks can re-enter earlier lines through ListVal /
	// BlockVal recursion — sorting once is cheaper than fighting
	// the walk shape.
	sort.SliceStable(b.tokens, func(i, j int) bool {
		if b.tokens[i].line != b.tokens[j].line {
			return b.tokens[i].line < b.tokens[j].line
		}
		return b.tokens[i].char < b.tokens[j].char
	})
	out := make([]uint32, 0, len(b.tokens)*5)
	var prevLine, prevChar uint32
	for i, t := range b.tokens {
		deltaLine := t.line - prevLine
		deltaStart := t.char
		if deltaLine == 0 && i > 0 {
			deltaStart = t.char - prevChar
		}
		out = append(out, deltaLine, deltaStart, t.length, t.tokType, t.modifiers)
		prevLine = t.line
		prevChar = t.char
	}
	return out
}

// indexAt returns the byte offset of the first occurrence of `needle`
// in src starting at or after `from`, or -1 when absent. Used to
// locate substrings whose positions the AST doesn't record (directive
// type names, dataset columns).
func indexAt(src []byte, needle string, from int) int {
	if from < 0 {
		from = 0
	}
	if from >= len(src) || needle == "" {
		return -1
	}
	for i := from; i+len(needle) <= len(src); i++ {
		if string(src[i:i+len(needle)]) == needle {
			return i
		}
	}
	return -1
}

// stringSpanFromSource returns the byte length of the string literal
// whose opening quote sits at src[start]. Handles backslash escapes
// (skips the char after `\`). Returns 0 if the literal doesn't close
// on the same line — multi-line strings aren't valid in PXF and the
// token would be invalid anyway.
func stringSpanFromSource(src []byte, start int) int {
	if start < 0 || start >= len(src) {
		return 0
	}
	quote := src[start]
	if quote != '"' && quote != '\'' {
		return 0
	}
	for i := start + 1; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++ // skip escaped char
			continue
		case '\n':
			return 0
		case quote:
			return i - start + 1
		}
	}
	return 0
}

// isInternalDescriptorFile reports whether a proto file path is one
// protolsp ships in its binary (the workspace-config + chameleon
// descriptors). Used to attach the `defaultLibrary` token modifier
// so editors can style internal fields distinctly from user-domain
// schemas.
func isInternalDescriptorFile(path string) bool {
	switch path {
	case "protolsp/v1/workspace_config.proto", "chameleon/v1/chain.proto":
		return true
	}
	return false
}

// numberLen is a number literal's width in the source, its precision
// suffix included (0.1p53): End covers the suffix, Raw does not. A
// literal built without End falls back to Raw.
func numberLen(pos, end pxf.Position, raw string) int {
	if end.Offset > pos.Offset {
		return end.Offset - pos.Offset
	}
	return len(raw)
}
