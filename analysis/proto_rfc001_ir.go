// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// RFC-001 IR artifacts (#220). The compile pass (analyzeProto's
// queries.IR run) produces more than diagnostics: the IR knows which
// function each call site inside an opaque engine-expression
// fragment resolved to, and the fdp lowering emits the canonical
// source map that keys every lowered construct by descriptor_path.
// Both answer questions the AST alone can't:
//
//   - An expression-classified annotation argument
//     (`@check(is_email(this.value))`) captures raw tokens —
//     AnnotationUseArg.Value() is zero, so the AST-walking resolvers
//     in proto_rfc001_nav.go have no path node to classify. The IR's
//     ExtractCalls already scanned the fragment and resolved each
//     `name(` run against the visible `function` declarations.
//   - A runtime validator reports violations keyed by
//     (file, descriptor_path). Mapping those back to editor ranges
//     needs the same SourceMap the compiled artifact carries.
//
// analyzeProto distills both into plain values on the Document (byte
// offsets + strings, no IR handles) so nothing here outlives the
// compile session's arenas.

package analysis

import (
	"strings"

	"github.com/trendvidia/protocompile/fdp"
	schemav1 "github.com/trendvidia/protocompile/gen/protowire/schema/v1"
	"github.com/trendvidia/protocompile/ir"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/source/length"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// rfc001CallSite is one resolved function-call site extracted from an
// expression-classified annotation argument of this document. Offsets
// are byte offsets into the document source (the IR compiled the same
// bytes analyzeProto handed the opener), bounding the callee name as
// written — the qualified-ident run at the head of the call, not the
// argument list. Cursor hits on the name navigate; hits inside the
// call's arguments don't (those tokens mean engine-expression things
// the LSP can't speak for).
type rfc001CallSite struct {
	NameStart, NameEnd int
	// FQN is the resolved target's fully-qualified function name
	// (package-qualified, no leading dot), resolvable through
	// resolveProtoFunctionRef like any parsed function reference.
	FQN string
}

// rfc001CallSites walks every annotation-use surface of the compiled
// file — the same carrier set fdp's source-map emission covers — and
// extracts the call sites of expression-classified arguments. Only
// spans anchored in filename participate: alias-propagated uses can
// carry spans pointing into the alias's declaring file, and those
// belong to that document's own analyze pass.
func rfc001CallSites(irFile *ir.File, filename string) []rfc001CallSite {
	var sites []rfc001CallSite
	collect := func(uses seq.Indexer[ir.AnnotationUse]) {
		for use := range seq.Values(uses) {
			for _, b := range use.ArgBindings() {
				if !b.Param.IsExpression() {
					continue
				}
				for _, call := range use.ExtractCalls(b.Arg) {
					span := call.Span
					if span.IsZero() || span.Path() != filename {
						continue
					}
					nameLen := rfc001CallNameLen(span.Text())
					if nameLen == 0 {
						continue
					}
					sites = append(sites, rfc001CallSite{
						NameStart: span.Start,
						NameEnd:   span.Start + nameLen,
						FQN:       string(call.Target.FullName()),
					})
				}
			}
		}
	}
	for ty := range seq.Values(irFile.AllTypes()) {
		collect(ty.Annotations())
		for m := range seq.Values(ty.Members()) {
			collect(m.Annotations())
		}
		for o := range seq.Values(ty.Oneofs()) {
			collect(o.Annotations())
		}
	}
	for ext := range seq.Values(irFile.AllExtensions()) {
		collect(ext.Annotations())
	}
	for svc := range seq.Values(irFile.Services()) {
		collect(svc.Annotations())
		for m := range seq.Values(svc.Methods()) {
			collect(m.Annotations())
		}
	}
	for a := range seq.Values(irFile.Annotations()) {
		collect(a.Annotations())
	}
	for fn := range seq.Values(irFile.Functions()) {
		collect(fn.Annotations())
	}
	for ta := range seq.Values(irFile.TypeAliases()) {
		collect(ta.Annotations())
	}
	return sites
}

// rfc001CallNameLen measures the qualified-ident run at the head of a
// call span's text — the callee name portion of `pkg.func(args)`.
func rfc001CallNameLen(text string) int {
	i := 0
	for i < len(text) && (isProtoIdentByte(text[i]) || text[i] == '.') {
		i++
	}
	return i
}

// rfc001SourceMapFor extracts the canonical protowire.schema.v1
// SourceMap by running the compiled file through the same fdp
// lowering the protowire compiler ships — descriptor paths and
// anchor positions match the compiled artifact byte for byte, so
// runtime violation keys resolve against it directly. nil when the
// file lowers without any source-mappable construct (every classic
// proto file) or the generation fails on a partially-diagnosed IR.
func rfc001SourceMapFor(irFile *ir.File) *schemav1.SourceMap {
	fdesc, err := fdp.DescriptorProto(irFile)
	if err != nil || fdesc == nil || fdesc.GetOptions() == nil {
		return nil
	}
	sm, _ := proto.GetExtension(fdesc.GetOptions(), schemav1.E_SourceMap).(*schemav1.SourceMap)
	return sm
}

// rfc001CallSiteAt returns the resolved function FQN when the cursor
// sits on the callee name of an extracted call site. The position's
// column is interpreted in the negotiated encoding, same as
// protoFindAt (#232). Caller holds doc.mu (read side) — same
// discipline as every protoAST consumer.
func rfc001CallSiteAt(doc *Document, pos protocol.Position, enc positions.Encoding) (string, bool) {
	if len(doc.rfc001Calls) == 0 || doc.protoAST == nil {
		return "", false
	}
	off := doc.protoAST.OffsetIn(pos, enc)
	if off < 0 {
		return "", false
	}
	for _, c := range doc.rfc001Calls {
		if off >= c.NameStart && off < c.NameEnd {
			return c.FQN, true
		}
	}
	return "", false
}

// rfc001OpaqueCallDefinition resolves go-to-definition for a cursor
// on a function call inside an opaque engine-expression fragment.
// The IR already resolved the callee, so this only maps the FQN back
// to a declaration site: AST-first through the import chain, then
// the remote (registry/carrier) fallback. nil when the cursor isn't
// on an extracted call name.
func (s *Workspace) rfc001OpaqueCallDefinition(docURI protocol.DocumentURI, wsDir string, doc *Document, pos protocol.Position) *protocol.Location {
	name, ok := rfc001CallSiteAt(doc, pos, s.positionEncoding)
	if !ok {
		return nil
	}
	name = strings.TrimPrefix(name, ".")
	if result := s.resolveProtoFunctionRef(doc.protoAST, name); !result.IsZero() {
		if loc := s.rfc001DeclLocation(docURI, doc.protoAST, result.File, result.Decl.Name()); loc != nil {
			return loc
		}
	}
	return s.rfc001RemoteLocation(wsDir, doc.protoAST, rfc001KindFunction, name, "")
}

// rfc001OpaqueCallHover renders the function declaration card for a
// cursor on a call inside an opaque fragment — the hover twin of
// rfc001OpaqueCallDefinition.
func (s *Workspace) rfc001OpaqueCallHover(wsDir string, doc *Document, pos protocol.Position) *protocol.Hover {
	name, ok := rfc001CallSiteAt(doc, pos, s.positionEncoding)
	if !ok {
		return nil
	}
	name = strings.TrimPrefix(name, ".")
	if result := s.resolveProtoFunctionRef(doc.protoAST, name); !result.IsZero() {
		return renderProtoFunctionHover(result.File, result.Decl)
	}
	return s.rfc001RemoteHover(wsDir, doc.protoAST, rfc001KindFunction, name, "")
}

// RFC001SourceRange resolves a source-map descriptor_path key (RFC-001
// §8.3.1 — the key a protowire runtime attaches to violations) to the
// editor range of its source anchor in the given document. This is
// the runtime-error → source mapping primitive (#220): a future
// violation-overlay feed calls this per violation and renders the
// result as an informational diagnostic.
//
// The returned range covers the token at the anchor (the `@` of a use
// site, the callee ident of a FUNCTION_CALL, the type ident of a
// TYPE_REFINEMENT). ok=false when the document isn't analyzed, holds
// no source map, the key doesn't parse, or no entry matches.
func (s *Workspace) RFC001SourceRange(docURI protocol.DocumentURI, descriptorPath string) (protocol.Range, bool) {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return protocol.Range{}, false
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	return s.rfc001SourceRangeLocked(doc, descriptorPath)
}

// rfc001SourceRangeLocked is RFC001SourceRange's body. Caller holds
// doc.mu (read or write) — split out so the runtime-violation
// overlay (#231) can resolve keys from inside diagnosticsForLocked's
// single-RLock window without recursing on the document lock.
func (s *Workspace) rfc001SourceRangeLocked(doc *Document, descriptorPath string) (protocol.Range, bool) {
	sm := doc.rfc001SourceMap
	if sm == nil || doc.protoAST == nil || doc.protoAST.Src == nil {
		return protocol.Range{}, false
	}
	key, err := fdp.ParseDescriptorPath(descriptorPath)
	if err != nil {
		return protocol.Range{}, false
	}
	canonical := key.String()
	for _, e := range sm.GetEntries() {
		if e.GetDescriptorPath() != canonical {
			continue
		}
		loc := e.GetSourceLocation()
		// Entries can anchor in another file (alias-propagated uses);
		// those aren't positions in THIS document.
		if loc == nil || loc.GetLine() <= 0 || loc.GetFile() != sm.GetFile() {
			continue
		}
		col := int(loc.GetColumn())
		if col < 1 {
			col = 1
		}
		// Carrier columns count Unicode code points (1-indexed).
		off := doc.protoAST.Src.InverseLocation(int(loc.GetLine()), col, length.Runes).Offset
		span := doc.protoAST.Src.Span(off, off+1)
		if _, at := doc.protoAST.AST.Stream().Around(off); !at.IsZero() && protoast.Contains(at.Span(), off) {
			span = at.Span()
		}
		return protoast.SpanRangeIn(span, s.positionEncoding), true
	}
	return protocol.Range{}, false
}
