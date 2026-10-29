// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package protoast wraps trendvidia/protocompile's parser for the
// LSP features that retain a per-document AST (hover, definition,
// completion, cross-file navigation, protocheck / protovalidate
// schema walks). The compiler-driven diagnostics path does NOT go
// through here — it uses protocompile's root Compiler directly.
//
// The wrapper exists because the new AST hands out value-typed
// nodes whose spans are byte offsets into a *source.File; every
// consumer needs the (ast.File, source.File) pair together to
// convert spans to LSP positions. File bundles them with the path
// the parser was fed (mirroring the old FileNode.Name()).
package protoast

import (
	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/parser"
	"github.com/trendvidia/protocompile/report"
	"github.com/trendvidia/protocompile/source"
	"github.com/trendvidia/protocompile/source/length"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

// File is one parsed .proto source: the AST, the indexed source
// text its spans point into, and the path the parser was fed.
type File struct {
	// Path is the name given to the parser — an absolute disk path
	// for on-disk files, a registry path for registry:// previews.
	Path string

	// AST is the parsed file. Never nil on a non-nil File: the new
	// parser always produces a (possibly partial) AST, so fault
	// tolerance comes from missing/zero nodes rather than a nil
	// file. Callers check IsZero on individual nodes, not nil here.
	AST *ast.File

	// Src is the indexed source text AST spans resolve against.
	Src *source.File
}

// Name returns the path the parser was fed. Mirrors the old
// protocompile FileNode.Name() accessor that callers (protoindex
// consumers, the compile resolver) keyed on.
func (f *File) Name() string {
	if f == nil {
		return ""
	}
	return f.Path
}

// Parse parses .proto source into a File, swallowing all
// diagnostics. This is the navigation-path parse: diagnostics for
// open documents come from the compiler pass, and index / shadow /
// registry copies never publish diagnostics at all. The returned
// AST may be partial when the source is broken — navigation
// degrades to "no info" on the malformed parts.
func Parse(path string, data []byte) *File {
	src := source.NewFile(path, string(data))
	var rep report.Report
	f, _ := parser.Parse(path, src, &rep)
	if f == nil {
		return nil
	}
	return &File{Path: path, AST: f, Src: src}
}

// OffsetIn converts a 0-indexed LSP position into a byte offset in
// f.Src, interpreting the column in the negotiated position
// encoding (UTF-16 code units by default, bytes when the client
// negotiated UTF-8) — the input-side twin of RangeIn. A
// column past the end of its line clamps to the line end (per
// spec — editors send such positions for cursors on indented blank
// lines). Returns -1 when the line lies outside the file.
//
// Unlike Range there is no fixed-UTF-16 form: every position a
// client sends is exact in the negotiated encoding, so every
// conversion must state it.
func (f *File) OffsetIn(pos protocol.Position, enc positions.Encoding) int {
	if f == nil || f.Src == nil {
		return -1
	}
	line := int(pos.Line) + 1 // #nosec G115 -- LSP positions are non-negative
	col := int(pos.Character) + 1
	text := f.Src.Text()
	if lastLine := f.Src.LineByOffset(len(text)); line > lastLine+1 {
		return -1
	}
	start, end := f.Src.LineOffsets(line)
	// The clamp boundary excludes the trailing newline — a cursor
	// can sit after the line's last character, not after its \n.
	lineEnd := end
	if lineEnd > start && text[lineEnd-1] == '\n' {
		lineEnd--
	}
	unit := length.UTF16
	if enc == positions.UTF8 {
		unit = length.Bytes
	}
	off := f.Src.InverseLocation(line, col, unit).Offset
	if off > lineEnd {
		off = lineEnd
	}
	return off
}

// RangeIn converts a span into a 0-indexed LSP range under the
// negotiated position encoding. Zero spans yield the zero range.
//
// The fixed-UTF-16 Range/SpanRange forms are gone (no approximate call
// sites remain): every emitted range must state the
// encoding, same rule as OffsetIn on the input side.
func (f *File) RangeIn(s source.Span, enc positions.Encoding) protocol.Range {
	if f == nil {
		return protocol.Range{}
	}
	return SpanRangeIn(s, enc)
}

// SpanRangeIn converts any span into a 0-indexed LSP range whose
// columns are counted in the negotiated position encoding (UTF-16
// code units by default, bytes when the client negotiated UTF-8).
// Spans carry their own *source.File, so this needs no
// protoast.File — diagnostics from a compile pipeline convert the
// same way navigation spans do. Zero spans yield the zero range.
func SpanRangeIn(s source.Span, enc positions.Encoding) protocol.Range {
	if s.IsZero() {
		return protocol.Range{}
	}
	unit := length.UTF16
	if enc == positions.UTF8 {
		unit = length.Bytes
	}
	start := s.File.Location(s.Start, unit)
	end := s.File.Location(s.End, unit)
	return protocol.Range{
		Start: protocol.Position{
			Line:      uint32(start.Line - 1), // #nosec G115 -- locations are 1-indexed
			Character: uint32(start.Column - 1),
		},
		End: protocol.Position{
			Line:      uint32(end.Line - 1), // #nosec G115
			Character: uint32(end.Column - 1),
		},
	}
}

// Contains reports whether the span contains the byte offset.
// Spans are half-open: [Start, End). Zero spans contain nothing.
func Contains(s source.Span, offset int) bool {
	return !s.IsZero() && offset >= s.Start && offset < s.End
}
