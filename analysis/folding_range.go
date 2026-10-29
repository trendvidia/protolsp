// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// General block folding (#256): a standard pull-based
// textDocument/foldingRange provider covering block structure in every
// file protolsp handles — .pxf documents and .proto schemas — so
// editors can collapse to read high-level structure.
//
// This is deliberately structure-only: a fold range per multi-line
// block, plus (for .proto) the import group and comment blocks. No
// enrichment — row counts, huge/auto-collapse hints, and summaries stay
// in the specialized @dataset regions push (protolsp/datasetRegionsChanged),
// which is layered on top for that separate concern.

package analysis

import (
	"bytes"
	"sort"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/source"
	"github.com/trendvidia/protocompile/token"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// FoldingRangesFor builds the fold ranges for a .pxf document: one per
// multi-line block (nodes, `children`, `state`, `lifecycle`, map / list
// values) and per multi-line directive. Returns nil when the document
// isn't open or hasn't parsed. Single-line blocks aren't foldable and
// are dropped.
func (s *Workspace) FoldingRangesFor(docURI protocol.DocumentURI) []protocol.FoldingRange {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	if doc.Parsed == nil {
		return nil
	}
	w := &pxfFoldWalker{source: doc.Source, encoding: s.positionEncoding}
	w.walkDocument(doc.Parsed)
	return w.ranges
}

// pxfFoldWalker accumulates fold ranges from a pxf AST. Single-use per
// request, like symbolWalker.
type pxfFoldWalker struct {
	source   []byte
	encoding positions.Encoding
	ranges   []protocol.FoldingRange
}

// addSpan records a fold from startOffset to endOffset (byte offsets
// into w.source). Spans that start and end on the same line are dropped
// — there's nothing to fold. kind is empty for a plain structural block.
func (w *pxfFoldWalker) addSpan(startOffset, endOffset int, kind protocol.FoldingRangeKind) {
	startLine, _ := positions.LineCol(w.source, startOffset, w.encoding)
	endLine, _ := positions.LineCol(w.source, endOffset, w.encoding)
	if endLine <= startLine {
		return
	}
	w.ranges = append(w.ranges, protocol.FoldingRange{
		StartLine: startLine,
		EndLine:   endLine,
		Kind:      kind,
	})
}

func (w *pxfFoldWalker) walkDocument(doc *pxf.Document) {
	for i := range doc.Directives {
		w.walkDirective(doc.Directives[i])
	}
	w.walkEntries(doc.Entries)
}

// walkDirective folds a directive's inline `{ ... }` block. The pxf AST
// records only the directive's start position and the raw inner bytes
// of its block, not the block's end — so recover the closing brace: the
// first `{` at/after the directive start opens the block (prefixes are
// bare identifiers, never braces), and its inner span is exactly
// len(Body) bytes, so the byte just past the body is the matching `}`.
func (w *pxfFoldWalker) walkDirective(d pxf.Directive) {
	if d.Body == nil {
		return
	}
	rel := bytes.IndexByte(w.source[d.Pos.Offset:], '{')
	if rel < 0 {
		return
	}
	closeBrace := d.Pos.Offset + rel + 1 + len(d.Body)
	if closeBrace >= len(w.source) || w.source[closeBrace] != '}' {
		return
	}
	w.addSpan(d.Pos.Offset, closeBrace, "")
}

func (w *pxfFoldWalker) walkEntries(entries []pxf.Entry) {
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Block:
			w.addSpan(n.Pos.Offset, n.End.Offset, "")
			w.walkEntries(n.Entries)
		case *pxf.Assignment:
			w.walkValue(n.Value)
		case *pxf.MapEntry:
			w.walkValue(n.Value)
		}
	}
}

// walkValue folds block-shaped and list-shaped values (map bodies,
// inline list-of-message values) and recurses into their contents.
// Scalar values carry no foldable structure.
func (w *pxfFoldWalker) walkValue(v pxf.Value) {
	switch n := v.(type) {
	case *pxf.BlockVal:
		w.addSpan(n.Pos.Offset, n.End.Offset, "")
		w.walkEntries(n.Entries)
	case *pxf.ListVal:
		w.addSpan(n.Pos.Offset, n.End.Offset, "")
		for _, el := range n.Elements {
			w.walkValue(el)
		}
	}
}

// ProtoFoldingRangesFor builds the fold ranges for a .proto schema: one
// per message / enum / service / oneof / extend / rpc body (recursively,
// including nested types), plus the import group (kind=imports) and
// comment blocks (kind=comment). Returns nil when the document isn't
// open or hasn't parsed.
func (s *Workspace) ProtoFoldingRangesFor(docURI protocol.DocumentURI) []protocol.FoldingRange {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	if doc.protoAST == nil || doc.protoAST.AST == nil {
		return nil
	}
	w := &protoFoldWalker{file: doc.protoAST, encoding: s.positionEncoding}
	w.walkDecls(doc.protoAST.AST.Decls())
	w.foldImports()
	w.foldComments()
	return w.ranges
}

// protoFoldWalker accumulates fold ranges from a parsed .proto AST.
type protoFoldWalker struct {
	file     *protoast.File
	encoding positions.Encoding
	ranges   []protocol.FoldingRange
}

func (w *protoFoldWalker) addSpan(s source.Span, kind protocol.FoldingRangeKind) {
	if s.IsZero() {
		return
	}
	rng := protoast.SpanRangeIn(s, w.encoding)
	if rng.End.Line <= rng.Start.Line {
		return
	}
	w.ranges = append(w.ranges, protocol.FoldingRange{
		StartLine: rng.Start.Line,
		EndLine:   rng.End.Line,
		Kind:      kind,
	})
}

// walkDecls folds every definition body (`{ ... }`) and recurses into
// it. Any DeclDef with a non-zero body qualifies — message, enum,
// service, oneof, extend, group, and an rpc with an options block — so
// the one rule covers them all. The error-tolerant parser can nest
// bodies anywhere, so the walk treats every level uniformly.
func (w *protoFoldWalker) walkDecls(decls seq.Indexer[ast.DeclAny]) {
	for decl := range seq.Values(decls) {
		switch decl.Kind() {
		case ast.DeclKindDef:
			def := decl.AsDef()
			body := def.Body()
			if body.IsZero() {
				continue
			}
			w.addSpan(body.Braces().Span(), "")
			w.walkDecls(body.Decls())
		case ast.DeclKindBody:
			w.walkDecls(decl.AsBody().Decls())
		}
	}
}

// foldImports folds the leading import group as one range. A lone import
// spans a single line and is dropped by addSpan — only a group of two or
// more imports across distinct lines produces a fold.
func (w *protoFoldWalker) foldImports() {
	var first, last source.Span
	for imp := range w.file.AST.Imports() {
		s := imp.Span()
		if s.IsZero() {
			continue
		}
		if first.IsZero() {
			first = s
		}
		last = s
	}
	if first.IsZero() {
		return
	}
	w.addSpan(source.Join(first, last), protocol.ImportsFoldingRange)
}

// foldComments folds comment blocks: a multi-line block comment
// (`/* ... */`) or a run of consecutive line comments (`//`) on adjacent
// lines. Only comments that begin their line are considered — a trailing
// comment after code (`int x = 1; // note`) isn't a foldable block.
func (w *protoFoldWalker) foldComments() {
	src := w.file.Src
	if src == nil {
		return
	}
	text := src.Text()
	type lineSpan struct{ start, end uint32 }
	var comments []lineSpan
	for tok := range w.file.AST.Stream().All() {
		if tok.Kind() != token.Comment {
			continue
		}
		s := tok.Span()
		if s.IsZero() || !commentStartsLine(text, s.Start) {
			continue
		}
		rng := protoast.SpanRangeIn(s, w.encoding)
		comments = append(comments, lineSpan{rng.Start.Line, rng.End.Line})
	}
	sort.Slice(comments, func(i, j int) bool { return comments[i].start < comments[j].start })

	for i := 0; i < len(comments); {
		start, end := comments[i].start, comments[i].end
		j := i + 1
		for j < len(comments) && comments[j].start <= end+1 {
			if comments[j].end > end {
				end = comments[j].end
			}
			j++
		}
		if end > start {
			w.ranges = append(w.ranges, protocol.FoldingRange{
				StartLine: start,
				EndLine:   end,
				Kind:      protocol.CommentFoldingRange,
			})
		}
		i = j
	}
}

// commentStartsLine reports whether the byte at offset is preceded on
// its line only by whitespace — i.e. the comment starting there is the
// first non-space content on the line.
func commentStartsLine(text string, offset int) bool {
	for i := offset - 1; i >= 0; i-- {
		switch text[i] {
		case '\n':
			return true
		case ' ', '\t', '\r':
			continue
		default:
			return false
		}
	}
	return true
}
