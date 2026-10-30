// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// DocumentSymbolsFor builds the symbol tree for a document. Returns
// nil when the document isn't open or hasn't parsed; an empty (but
// non-nil) slice is reserved for "parsed, but nothing to show," which
// the current grammar never produces — a parsed doc always has at
// least @type or some directive / entry.
func (s *Workspace) DocumentSymbolsFor(docURI uri.URI) []protocol.DocumentSymbol {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	if doc.Parsed == nil {
		return nil
	}
	// Resolve the bound message type if @type is set; non-fatal when
	// it doesn't resolve — we still emit symbols without descriptor-
	// derived detail, the outline just shows raw field names without
	// `: <type>` annotation.
	var rootMsg protoreflect.MessageDescriptor
	if doc.Parsed.TypeURL != "" {
		if mt, err := s.findTypeForDoc(doc, protoreflect.FullName(doc.Parsed.TypeURL)); err == nil && mt != nil {
			rootMsg = mt.Descriptor()
		}
	}
	w := &symbolWalker{source: doc.Source, encoding: s.positionEncoding}
	return w.build(doc.Parsed, rootMsg)
}

// symbolWalker collects DocumentSymbol nodes from a pxf AST. The
// walker is single-use — every dispatchDocumentSymbol call gets a
// fresh one with the doc's source bytes + negotiated encoding.
type symbolWalker struct {
	source   []byte
	encoding positions.Encoding
}

// build is the entry point: walks Document-level directives + entries
// and produces the top-level symbol slice.
func (w *symbolWalker) build(doc *pxf.Document, md protoreflect.MessageDescriptor) []protocol.DocumentSymbol {
	out := make([]protocol.DocumentSymbol, 0, len(doc.Entries)+len(doc.Directives)+len(doc.Datasets)+len(doc.Protos)+1)

	// @type directive. The AST stores only the TypeURL string, not
	// the directive's source position — but per the grammar @type
	// is always at the top of the file (line 1, col 1) when present.
	// Using (0,0) for both endpoints is a reasonable approximation
	// for outline purposes; we don't need it to be pixel-accurate
	// since the user clicks the symbol to jump there and clients
	// scroll to the line.
	if doc.TypeURL != "" {
		typeRange := protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 0, Character: uint32(len("@type ") + len(doc.TypeURL))}, // #nosec G115 -- type URLs are short
		}
		out = append(out, protocol.DocumentSymbol{
			Name:           "@type " + doc.TypeURL,
			Kind:           protocol.SymbolKindNamespace,
			Range:          typeRange,
			SelectionRange: typeRange,
		})
	}

	for _, d := range doc.Directives {
		out = append(out, w.directiveSymbol(d))
	}
	for _, d := range doc.Datasets {
		out = append(out, w.datasetSymbol(d))
	}
	for _, p := range doc.Protos {
		out = append(out, w.protoSymbol(p))
	}

	out = append(out, w.entrySymbols(doc.Entries, md)...)
	return out
}

// entrySymbols walks an Entry slice, resolving each Assignment /
// Block against the enclosing descriptor when available. Returns a
// new slice; safe to call recursively for nested blocks.
func (w *symbolWalker) entrySymbols(entries []pxf.Entry, md protoreflect.MessageDescriptor) []protocol.DocumentSymbol {
	out := make([]protocol.DocumentSymbol, 0, len(entries))
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			out = append(out, w.assignmentSymbol(n, md))
		case *pxf.Block:
			out = append(out, w.blockSymbol(n, md))
		case *pxf.MapEntry:
			out = append(out, w.mapEntrySymbol(n))
		}
	}
	return out
}

func (w *symbolWalker) assignmentSymbol(a *pxf.Assignment, md protoreflect.MessageDescriptor) protocol.DocumentSymbol {
	detail := ""
	if md != nil {
		if fd := md.Fields().ByName(protoreflect.Name(a.Key)); fd != nil {
			detail = fieldTypeLabel(fd)
		}
	}
	r := w.rangeForName(a.Pos, a.Key)
	return protocol.DocumentSymbol{
		Name:           a.Key,
		Detail:         strPtr(detail),
		Kind:           protocol.SymbolKindField,
		Range:          r,
		SelectionRange: r,
	}
}

func (w *symbolWalker) blockSymbol(b *pxf.Block, md protoreflect.MessageDescriptor) protocol.DocumentSymbol {
	detail := ""
	var (
		childMd protoreflect.MessageDescriptor
		fd      protoreflect.FieldDescriptor
	)
	if md != nil {
		if fd = md.Fields().ByName(protoreflect.Name(b.Name)); fd != nil {
			detail = fieldTypeLabel(fd)
			if fd.Message() != nil {
				childMd = fd.Message()
			}
		}
	}
	// Keyed repeated field: the block's direct children are entries
	// (keyed by name), each bound to the element message — not fields
	// of the parent. Render them as named entry symbols.
	var children []protocol.DocumentSymbol
	if pxf.KeyField(fd) != nil {
		children = w.keyedEntrySymbols(b.Entries, fd.Message())
	} else {
		children = w.entrySymbols(b.Entries, childMd)
	}
	name := w.rangeForName(b.Pos, b.Name)
	full := w.rangeFromEntriesTo(b.Pos, b.Entries)
	return protocol.DocumentSymbol{
		Name:           b.Name,
		Detail:         strPtr(detail),
		Kind:           protocol.SymbolKindObject,
		Range:          full,
		SelectionRange: name,
		Children:       children,
	}
}

// keyedEntrySymbols renders the entries of a keyed repeated field's
// block as named symbols. The symbol name is the entry key (the value
// the schema's (pxf.key) field takes for that entry); each entry nests
// recursively against elemMd, the element message — so a UI document's
// `children` block outlines by element name, drilling into each
// element's own fields (and any deeper keyed collections). Detail is
// the element type so the outline reads "loginButton : ui.v1.Element".
func (w *symbolWalker) keyedEntrySymbols(entries []pxf.Entry, elemMd protoreflect.MessageDescriptor) []protocol.DocumentSymbol {
	out := make([]protocol.DocumentSymbol, 0, len(entries))
	detail := ""
	if elemMd != nil {
		detail = string(elemMd.FullName())
	}
	for _, e := range entries {
		ke, ok := asKeyedEntry(e)
		if !ok {
			continue
		}
		name := w.rangeForEntryName(ke)
		full := w.rangeFromEntriesTo(ke.pos, ke.body)
		out = append(out, protocol.DocumentSymbol{
			Name:           ke.key,
			Detail:         strPtr(detail),
			Kind:           protocol.SymbolKindObject,
			Range:          full,
			SelectionRange: name,
			Children:       w.entrySymbols(ke.body, elemMd),
		})
	}
	return out
}

// rangeForEntryName returns the SelectionRange for a keyed entry's name
// token. Unlike rangeForName it spans the token as written in source —
// including the surrounding quotes for a quoted entry name — so the
// click target lands exactly on `"us east"`, not on a bare-length
// approximation of it.
func (w *symbolWalker) rangeForEntryName(ke keyedEntry) protocol.Range {
	startLine, startChar := positions.LineCol(w.source, ke.pos.Offset, w.encoding)
	start := protocol.Position{Line: startLine, Character: startChar}
	if end := nameTokenEnd(w.source, ke.pos.Offset, ke.quoted); end >= 0 {
		endLine, endChar := positions.LineCol(w.source, end, w.encoding)
		return protocol.Range{Start: start, End: protocol.Position{Line: endLine, Character: endChar}}
	}
	return protocol.Range{
		Start: start,
		End:   protocol.Position{Line: startLine, Character: startChar + uint32(len(ke.key))}, // #nosec G115 -- keys are short
	}
}

func (w *symbolWalker) mapEntrySymbol(m *pxf.MapEntry) protocol.DocumentSymbol {
	r := w.rangeForName(m.Pos, m.Key)
	return protocol.DocumentSymbol{
		Name:           m.Key,
		Kind:           protocol.SymbolKindKey,
		Range:          r,
		SelectionRange: r,
	}
}

func (w *symbolWalker) directiveSymbol(d pxf.Directive) protocol.DocumentSymbol {
	label := "@" + d.Name
	if d.Type != "" {
		label += " " + d.Type
	} else if len(d.Prefixes) > 0 {
		for _, p := range d.Prefixes {
			label += " " + p
		}
	}
	r := w.rangeForName(d.Pos, label)
	return protocol.DocumentSymbol{
		Name:           label,
		Kind:           protocol.SymbolKindEvent,
		Range:          r,
		SelectionRange: r,
	}
}

func (w *symbolWalker) datasetSymbol(d pxf.DatasetDirective) protocol.DocumentSymbol {
	label := "@dataset " + d.Type
	r := w.rangeForName(d.Pos, label)
	children := make([]protocol.DocumentSymbol, 0, len(d.Columns))
	for _, col := range d.Columns {
		cr := w.rangeForName(d.Pos, col)
		children = append(children, protocol.DocumentSymbol{
			Name:           col,
			Kind:           protocol.SymbolKindField,
			Range:          cr,
			SelectionRange: cr,
		})
	}
	return protocol.DocumentSymbol{
		Name:           label,
		Detail:         strPtr(d.Type),
		Kind:           protocol.SymbolKindStruct,
		Range:          r,
		SelectionRange: r,
		Children:       children,
	}
}

func (w *symbolWalker) protoSymbol(p pxf.ProtoDirective) protocol.DocumentSymbol {
	label := "@proto"
	if p.TypeName != "" {
		label += " " + p.TypeName
	} else {
		label += " (" + p.Shape.String() + ")"
	}
	r := w.rangeForName(p.Pos, label)
	return protocol.DocumentSymbol{
		Name:           label,
		Kind:           protocol.SymbolKindInterface,
		Range:          r,
		SelectionRange: r,
	}
}

// rangeForName returns a single-line range covering `name` starting
// at pos. Used for the symbol's SelectionRange (the click target the
// editor highlights) and as the default Range for leaf symbols.
func (w *symbolWalker) rangeForName(pos pxf.Position, name string) protocol.Range {
	line, character := positions.LineCol(w.source, pos.Offset, w.encoding)
	end := character + uint32(len(name)) // #nosec G115 -- symbol names are short identifiers
	return protocol.Range{
		Start: protocol.Position{Line: line, Character: character},
		End:   protocol.Position{Line: line, Character: end},
	}
}

// rangeFromEntriesTo builds a Range from `start` to the last entry's
// position (extended one line forward to approximate the closing `}`).
// For empty blocks we fall back to the name range. The pxf AST doesn't
// record end positions for Block / Assignment so this is best-effort —
// good enough for outline highlighting, where slight overshoot at the
// closing brace is invisible to the user.
func (w *symbolWalker) rangeFromEntriesTo(start pxf.Position, entries []pxf.Entry) protocol.Range {
	startLine, startChar := positions.LineCol(w.source, start.Offset, w.encoding)
	if len(entries) == 0 {
		return protocol.Range{
			Start: protocol.Position{Line: startLine, Character: startChar},
			End:   protocol.Position{Line: startLine, Character: startChar},
		}
	}
	last := entries[len(entries)-1]
	lastPos := entryPos(last)
	endLine, endChar := positions.LineCol(w.source, lastPos.Offset, w.encoding)
	// Extend by one line to approximate the closing `}` on the
	// following line, which is the conventional layout. For inline
	// `{ a = 1; b = 2 }` this overshoots by one line — harmless for
	// outline rendering.
	return protocol.Range{
		Start: protocol.Position{Line: startLine, Character: startChar},
		End:   protocol.Position{Line: endLine + 1, Character: endChar},
	}
}

func entryPos(e pxf.Entry) pxf.Position {
	switch n := e.(type) {
	case *pxf.Assignment:
		return n.Pos
	case *pxf.MapEntry:
		return n.Pos
	case *pxf.Block:
		return n.Pos
	}
	return pxf.Position{}
}
