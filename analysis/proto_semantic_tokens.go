// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Semantic tokens for .proto documents. Editors ship
// tmLanguage grammars for stock proto2/proto3, so protolsp stays out
// of their way there — the semantic layer only marks the RFC-001
// schema-extension constructs those grammars don't know about:
//
//   - the `type` / `function` / `annotation` declaration keywords,
//     which stock grammars tokenize as plain identifiers (or worse,
//     as a field type), and
//   - `@name` annotation-use sites, which have no proto2/proto3
//     precedent at all.
//
// Fed from the retained protoAST (Document.protoAST) that the
// analyze pipeline already parses for hover / definition, so a
// semanticTokens/full request never re-parses.

package analysis

import (
	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/source"

	"github.com/trendvidia/protolsp/protoast"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// walkProtoFile emits semantic tokens for one parsed .proto file.
// The parser is fault-tolerant — the AST may be partial — so every
// node goes through the zero-value checks the addSpan/emit helpers
// perform rather than assuming well-formed shapes.
func (b *semanticTokenBuilder) walkProtoFile(f *protoast.File) {
	if f == nil || f.AST == nil {
		return
	}
	b.protoDecls(f.AST.Decls())
}

// protoDecls walks one declaration sequence (file top level or a
// message/service body), emitting tokens for RFC-001 constructs and
// recursing into nested bodies. RFC-001 declarations are top-level
// only in well-formed files, but annotation uses attach to defs at
// any depth (leading on message/enum/service, trailing on fields),
// and a mid-edit buffer can put anything anywhere — the walk treats
// every level uniformly.
func (b *semanticTokenBuilder) protoDecls(decls seq.Indexer[ast.DeclAny]) {
	for decl := range seq.Values(decls) {
		switch decl.Kind() {
		case ast.DeclKindType:
			d := decl.AsType()
			b.addProtoSpan(d.KeywordToken().Span(), stKeyword)
			b.protoAnnotationUses(d.Annotations())
		case ast.DeclKindFunction:
			d := decl.AsFunction()
			b.addProtoSpan(d.KeywordToken().Span(), stKeyword)
			b.protoAnnotationUses(d.Annotations())
		case ast.DeclKindAnnotation:
			d := decl.AsAnnotation()
			b.addProtoSpan(d.KeywordToken().Span(), stKeyword)
			b.protoAnnotationUses(d.Annotations())
		case ast.DeclKindAnnotationUse:
			// An unbound use site — the parser couldn't attach it to
			// a following declaration (trailing junk, mid-edit state).
			// Still highlight it: the user typed an annotation use and
			// the coloring shouldn't flicker off while they type the
			// declaration below it.
			b.protoAnnotationUse(decl.AsAnnotationUse())
		case ast.DeclKindDef:
			def := decl.AsDef()
			b.protoAnnotationUses(def.Annotations())
			if body := def.Body(); !body.IsZero() {
				b.protoDecls(body.Decls())
			}
		case ast.DeclKindBody:
			b.protoDecls(decl.AsBody().Decls())
		}
	}
}

func (b *semanticTokenBuilder) protoAnnotationUses(uses seq.Indexer[ast.DeclAnnotationUse]) {
	for use := range seq.Values(uses) {
		b.protoAnnotationUse(use)
	}
}

// protoAnnotationUse emits one decorator token covering `@name` —
// the sigil plus the (possibly dotted) annotation path, excluding
// any argument list. A use so malformed it has no name still gets a
// token for the bare `@` (source.Join skips the zero name span).
func (b *semanticTokenBuilder) protoAnnotationUse(use ast.DeclAnnotationUse) {
	if use.IsZero() {
		return
	}
	b.addProtoSpan(source.Join(use.At(), use.Name()), stDecorator)
}

// addProtoSpan records one token from a protocompile source span.
// Spans are byte offsets into the same bytes as b.source, so the
// pxf-shaped add() path (offset + byte length → LSP line/col under
// the negotiated encoding) applies unchanged.
func (b *semanticTokenBuilder) addProtoSpan(s source.Span, tokType uint32) {
	if s.IsZero() {
		return
	}
	b.add(pxf.Position{Offset: s.Start}, s.Len(), tokType, 0)
}
