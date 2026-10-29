// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Position → AST node lookup for .proto documents. Phase 2 of the
// proto-authoring plan: hover and go-to-definition both need to ask
// "what's under the cursor?" and the AST's byte-offset spans let us
// walk the tree against an LSP position converted to an offset.

package analysis

import (
	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/source"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// protoPath is the chain of AST nodes from the file down to the
// deepest node whose source span contains the cursor. Parent context
// matters for classifying what the leaf represents — an ident token
// might be a field name (parent is a field-classified ast.DeclDef
// and the token is its name), part of a field type reference (parent
// is an ast.Path under the def's type), a message name, and so on.
// Hover + definition handlers walk this path bottom-up to pick the
// first interpretation that fits.
//
// Nodes holds concrete node values (ast.DeclDef, ast.Path,
// token.Token, ast.ExprDict, …), never the DeclAny / ExprAny /
// TypeAny type-erased wrappers — descend unwraps those so handlers
// can type-switch directly.
//
// Empty Nodes means the cursor isn't inside any tracked declaration
// (whitespace between top-level decls, EOF, etc.) — caller returns
// nil to LSP, which renders nothing.
type protoPath struct {
	File  *protoast.File
	Nodes []source.Spanner
}

// Leaf returns the deepest node in the path, or nil when the path is
// empty.
func (p *protoPath) Leaf() source.Spanner {
	if p == nil || len(p.Nodes) == 0 {
		return nil
	}
	return p.Nodes[len(p.Nodes)-1]
}

// Parent returns the node whose span DIRECTLY contains the leaf
// (i.e., the leaf's parent in the descent path), or nil when the leaf
// is the file itself.
func (p *protoPath) Parent() source.Spanner {
	if p == nil || len(p.Nodes) < 2 {
		return nil
	}
	return p.Nodes[len(p.Nodes)-2]
}

// Ancestor returns the closest ancestor of the leaf for which
// match(n) returns true, or nil when no such ancestor exists. Walks
// from leaf upward (so the closest matching parent wins). Used to
// answer "what enclosing message / field / oneof am I in?" from a
// nested ident token without each call site re-implementing the walk.
func (p *protoPath) Ancestor(match func(source.Spanner) bool) source.Spanner {
	if p == nil {
		return nil
	}
	// i = len-1 is the leaf itself; ancestors start at len-2.
	for i := len(p.Nodes) - 2; i >= 0; i-- {
		if match(p.Nodes[i]) {
			return p.Nodes[i]
		}
	}
	return nil
}

// protoFindAt locates the deepest AST node whose source span contains
// the LSP cursor position. Returns a protoPath with the descent chain
// (file → … → leaf) or a path with empty Nodes when the cursor is
// outside any tracked node.
//
// LSP positions are 0-indexed (line, character) pairs whose column
// unit is the negotiated position encoding (#232 — the input-side
// twin of #219 criterion 4); spans are half-open byte-offset ranges.
// protoast.File.OffsetIn performs the conversion, and containment is
// a plain offset check — a cursor sitting right after a token is NOT
// inside it; users expect hover to fire when they're ON the
// identifier, not just adjacent to it.
//
// Performance: the AST is small relative to LSP request rates (one
// .proto file is at most a few thousand nodes). A full walk per
// request is acceptable; if the protolsp scope grows to
// thousand-file workspaces we can revisit.
func protoFindAt(parsed *protoast.File, pos protocol.Position, enc positions.Encoding) *protoPath {
	path := &protoPath{File: parsed}
	if parsed == nil || parsed.AST == nil {
		return path
	}
	offset := parsed.OffsetIn(pos, enc)
	if offset < 0 {
		return path
	}
	if !protoast.Contains(parsed.AST.Span(), offset) {
		return path
	}
	path.Nodes = append(path.Nodes, parsed.AST)
	descendProto(fileChildren(parsed.AST), offset, path)
	return path
}

// descendProto recurses through children looking for the deepest one
// whose span contains offset. The path is appended-to in pre-order,
// so on return path.Nodes is the chain from root to the deepest
// match. First matching child wins; sibling spans are disjoint in a
// well-formed AST.
func descendProto(children []source.Spanner, offset int, path *protoPath) {
	for _, child := range children {
		if child == nil || !protoast.Contains(source.GetSpan(child), offset) {
			continue
		}
		path.Nodes = append(path.Nodes, child)
		descendProto(protoChildren(child), offset, path)
		return
	}
}

// protoChildren enumerates the descendable child nodes of any node
// the walker appends. This is the traversal adapter over the new
// AST, which has no generic Children() — every composite kind is
// listed here once, and the feature handlers get concrete values to
// type-switch on. DeclAny / ExprAny / TypeAny wrappers are unwrapped
// via the append helpers so they never appear in a path.
//
// Order within a node doesn't matter for the containment walk
// (child spans are disjoint), so children are listed in rough
// source order for readability.
func protoChildren(n source.Spanner) []source.Spanner {
	var out []source.Spanner
	switch v := n.(type) {
	case *ast.File:
		return fileChildren(v)
	case ast.DeclBody:
		for decl := range seq.Values(v.Decls()) {
			out = appendDecl(out, decl)
		}
	case ast.DeclDef:
		for use := range seq.Values(v.Annotations()) {
			out = append(out, use)
		}
		out = appendType(out, v.Type())
		out = append(out, v.Name())
		if sig := v.Signature(); !sig.IsZero() {
			out = append(out, sig)
		}
		out = appendExpr(out, v.Value())
		if opts := v.Options(); !opts.IsZero() {
			out = append(out, opts)
		}
		if body := v.Body(); !body.IsZero() {
			out = append(out, body)
		}
	case ast.DeclSyntax:
		out = appendExpr(out, v.Value())
	case ast.DeclPackage:
		out = append(out, v.Path())
	case ast.DeclImport:
		out = appendExpr(out, v.ImportPath())
	case ast.DeclRange:
		ranges := v.Ranges()
		for i := range ranges.Len() {
			out = appendExpr(out, ranges.At(i))
		}
		if opts := v.Options(); !opts.IsZero() {
			out = append(out, opts)
		}
	case ast.DeclType:
		out = append(out, v.Name())
		out = appendExpr(out, v.Value())
		for use := range seq.Values(v.Annotations()) {
			out = append(out, use)
		}
	case ast.DeclFunction:
		out = append(out, v.Name())
		params := v.Params()
		for i := range params.Len() {
			out = append(out, params.At(i))
		}
		if opts := v.Options(); !opts.IsZero() {
			out = append(out, opts)
		}
		for use := range seq.Values(v.Annotations()) {
			out = append(out, use)
		}
	case ast.DeclFunctionParam:
		out = append(out, v.Name())
		out = appendType(out, v.Type())
	case ast.DeclAnnotation:
		out = append(out, v.Name())
		params := v.Params()
		for i := range params.Len() {
			out = append(out, params.At(i))
		}
		for use := range seq.Values(v.Annotations()) {
			out = append(out, use)
		}
	case ast.DeclAnnotationParam:
		out = append(out, v.Name())
		out = appendType(out, v.Type())
		out = appendExpr(out, v.Default())
	case ast.DeclAnnotationUse:
		out = append(out, v.Name())
		args := v.Args()
		for i := range args.Len() {
			out = append(out, args.At(i))
		}
	case ast.AnnotationUseArg:
		out = append(out, v.Name())
		out = appendExpr(out, v.Value())
	case ast.Signature:
		inputs := v.Inputs()
		for i := range inputs.Len() {
			out = appendType(out, inputs.At(i))
		}
		outputs := v.Outputs()
		for i := range outputs.Len() {
			out = appendType(out, outputs.At(i))
		}
	case ast.CompactOptions:
		entries := v.Entries()
		for i := range entries.Len() {
			out = append(out, entries.At(i))
		}
	case ast.Option:
		out = append(out, v.Path)
		out = appendExpr(out, v.Value)
	case ast.TypePrefixed:
		out = appendType(out, v.Type())
	case ast.TypeGeneric:
		out = append(out, v.Path())
		args := v.Args()
		for i := range args.Len() {
			out = appendType(out, args.At(i))
		}
	case ast.Path:
		for pc := range v.Components() {
			if ext := pc.AsExtension(); !ext.IsZero() {
				out = append(out, ext)
			} else if tok := pc.AsIdent(); !tok.IsZero() {
				out = append(out, tok)
			}
		}
	case ast.ExprPrefixed:
		out = appendExpr(out, v.Expr())
	case ast.ExprRange:
		start, end := v.Bounds()
		out = appendExpr(out, start)
		out = appendExpr(out, end)
	case ast.ExprArray:
		elems := v.Elements()
		for i := range elems.Len() {
			out = appendExpr(out, elems.At(i))
		}
	case ast.ExprDict:
		elems := v.Elements()
		for i := range elems.Len() {
			out = append(out, elems.At(i))
		}
	case ast.ExprField:
		out = appendExpr(out, v.Key())
		out = appendExpr(out, v.Value())
	}
	// token.Token, ast.ExprLiteral, and name tokens are leaves.
	return out
}

// fileChildren lists the top-level decls of a file, unwrapped to
// concrete types.
func fileChildren(f *ast.File) []source.Spanner {
	var out []source.Spanner
	for decl := range seq.Values(f.Decls()) {
		out = appendDecl(out, decl)
	}
	return out
}

// appendDecl unwraps a DeclAny to its concrete type and appends it.
// Zero and empty decls are skipped.
func appendDecl(out []source.Spanner, d ast.DeclAny) []source.Spanner {
	switch d.Kind() {
	case ast.DeclKindSyntax:
		return append(out, d.AsSyntax())
	case ast.DeclKindPackage:
		return append(out, d.AsPackage())
	case ast.DeclKindImport:
		return append(out, d.AsImport())
	case ast.DeclKindDef:
		def := d.AsDef()
		// A LEADING annotation use (`@name` on the line above a
		// message/enum/service) attaches to the def in the AST but
		// its span lies BEFORE the def's span — the containment
		// walk would never descend into it through the def. Surface
		// those uses as siblings; trailing uses (inside the def's
		// span) stay reachable as protoChildren of the def.
		for use := range seq.Values(def.Annotations()) {
			if !protoast.Contains(source.GetSpan(def), source.GetSpan(use).Start) {
				out = append(out, use)
			}
		}
		return append(out, def)
	case ast.DeclKindBody:
		return append(out, d.AsBody())
	case ast.DeclKindRange:
		return append(out, d.AsRange())
	case ast.DeclKindType:
		return append(out, d.AsType())
	case ast.DeclKindFunction:
		return append(out, d.AsFunction())
	case ast.DeclKindAnnotation:
		return append(out, d.AsAnnotation())
	case ast.DeclKindAnnotationUse:
		return append(out, d.AsAnnotationUse())
	}
	return out
}

// appendType unwraps a TypeAny to its concrete type and appends it.
// TypePath is appended as its underlying ast.Path so handlers see
// one canonical "type reference" shape.
func appendType(out []source.Spanner, t ast.TypeAny) []source.Spanner {
	switch t.Kind() {
	case ast.TypeKindPath:
		return append(out, t.AsPath().Path)
	case ast.TypeKindPrefixed:
		return append(out, t.AsPrefixed())
	case ast.TypeKindGeneric:
		return append(out, t.AsGeneric())
	}
	return out
}

// appendExpr unwraps an ExprAny to its concrete type and appends it.
// ExprPath is appended as its underlying ast.Path — same canonical
// shape as type references, since handlers classify both by walking
// the path's parent.
func appendExpr(out []source.Spanner, e ast.ExprAny) []source.Spanner {
	switch e.Kind() {
	case ast.ExprKindLiteral:
		return append(out, e.AsLiteral())
	case ast.ExprKindPath:
		return append(out, e.AsPath().Path)
	case ast.ExprKindPrefixed:
		return append(out, e.AsPrefixed())
	case ast.ExprKindRange:
		return append(out, e.AsRange())
	case ast.ExprKindArray:
		return append(out, e.AsArray())
	case ast.ExprKindDict:
		return append(out, e.AsDict())
	case ast.ExprKindField:
		return append(out, e.AsField())
	}
	return out
}

// nodeLSPRange converts a node's span to an LSP Range (0-indexed,
// columns in the negotiated position encoding — #219 criterion 4:
// a fixed UTF-16 count mis-places columns after non-ASCII when the
// client negotiated UTF-8). Zero spans yield the zero range.
func nodeLSPRange(parsed *protoast.File, n source.Spanner, enc positions.Encoding) protocol.Range {
	if parsed == nil || n == nil {
		return protocol.Range{}
	}
	return parsed.RangeIn(source.GetSpan(n), enc)
}
