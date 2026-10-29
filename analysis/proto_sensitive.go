// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// @sensitive(class) support. protowire#163 resolved the
// §6.7 classification parameter as `annotation sensitive(class:
// string = "")` with an *open, org-defined* class vocabulary — no
// fixed enum, no spec registry — and the `protowire.` prefix
// reserved. The two protolsp consequences:
//
//   - Completion: the only source of class-name candidates is the
//     workspace's own usage. Inside the argument parens of a use
//     resolving to protowire.schema.v1.sensitive, protolsp offers
//     every class already used by a `sensitive` annotation anywhere
//     in the workspace (current buffer + proto index) — a
//     consistency aid, not a validity oracle: field → class →
//     key-domain routing breaks on typo'd near-duplicates.
//   - The reserved-prefix rejection itself needs no protolsp code:
//     protocompile v0.21.0's use-arg validation rejects string
//     `class` values that are "protowire" or a dotted extension, so
//     the compile pass surfaces it like any other semantic error
//     (pinned by TestDidOpen_SensitiveReservedClassPrefix).

package analysis

import (
	"slices"
	"strings"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/token"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// protowireSensitiveFQN is the standard-library sensitivity marker
// (RFC-001 §6.7). Class completion keys on this resolved name — a
// user annotation that happens to be called `sensitive` keeps its
// ordinary argument completion.
const protowireSensitiveFQN = "protowire.schema.v1.sensitive"

// protowireAnnotationsImportPath is the canonical import path of the
// protowire standard-library annotations file. Referenced when a
// `@sensitive` use can't be resolved through the AST-first chain
// (the import may be served registry-only): importing the canonical
// path is signal enough that the use means the standard marker.
const protowireAnnotationsImportPath = "protowire/schema/v1/annotations.proto"

// protoAnnotationUseAt returns the innermost annotation use whose
// argument parens contain the cursor — the position where argument
// completion (declared functions, or sensitivity classes for
// @sensitive) applies. Zero when the cursor is outside every use's
// parens; a cursor on the name or `@` sigil is
// protoAnnotationUsePrefix territory and lands outside too.
func protoAnnotationUseAt(parsed *protoast.File, pos protocol.Position, enc positions.Encoding) ast.DeclAnnotationUse {
	if parsed == nil || parsed.AST == nil {
		return ast.DeclAnnotationUse{}
	}
	offset := parsed.OffsetIn(pos, enc)
	if offset < 0 {
		return ast.DeclAnnotationUse{}
	}
	path := protoFindAt(parsed, pos, enc)
	for i := len(path.Nodes) - 1; i >= 0; i-- {
		if use, ok := path.Nodes[i].(ast.DeclAnnotationUse); ok {
			if protoast.Contains(use.Parens().Span(), offset) {
				return use
			}
			return ast.DeclAnnotationUse{}
		}
	}
	return ast.DeclAnnotationUse{}
}

// protoSensitiveUse reports whether the use site refers to the
// standard protowire.schema.v1.sensitive marker: written fully
// qualified, resolving there through the AST-first chain, or — when
// a bare `sensitive` resolves nowhere — imported under the canonical
// standard-library path (registry-only workspaces).
func (s *Workspace) protoSensitiveUse(f *protoast.File, use ast.DeclAnnotationUse) bool {
	name := strings.TrimPrefix(use.Name().Canonicalized(), ".")
	if name == protowireSensitiveFQN {
		return true
	}
	if name != "sensitive" && !strings.HasSuffix(name, ".sensitive") {
		return false
	}
	if r := s.resolveProtoAnnotationRef(f, name); !r.IsZero() {
		return qualifyProtoName(filePackage(r.File), r.Decl.Name().Text()) == protowireSensitiveFQN
	}
	return slices.Contains(protoImports(f), protowireAnnotationsImportPath)
}

// protoSensitiveClassArg picks the use's class argument: the named
// `class = …` form wins, else the first positional argument (the
// declaration has exactly one parameter). Zero when the use has no
// argument.
func protoSensitiveClassArg(use ast.DeclAnnotationUse) ast.AnnotationUseArg {
	var positional ast.AnnotationUseArg
	for arg := range seq.Values(use.Args()) {
		if arg.IsNamed() {
			if arg.Name().Text() == "class" {
				return arg
			}
			continue
		}
		if positional.IsZero() {
			positional = arg
		}
	}
	return positional
}

// protoSensitiveClassValue extracts the class string when the
// argument's value re-parsed as a string literal; ok=false for
// absent, non-literal, or non-string arguments.
func protoSensitiveClassValue(arg ast.AnnotationUseArg) (string, bool) {
	if arg.IsZero() {
		return "", false
	}
	v := arg.Value()
	if v.IsZero() || v.Kind() != ast.ExprKindLiteral {
		return "", false
	}
	tok := v.AsLiteral().Token
	if tok.Kind() != token.String {
		return "", false
	}
	return tok.AsString().Text(), true
}

// protoWalkAnnotationUses visits every annotation use in the file:
// leading uses on block declarations, trailing uses on type /
// function / annotation declarations and defs (fields, enum values),
// orphaned top-level uses, recursively through message bodies. The
// same carrier set the semantic-token walk paints.
func protoWalkAnnotationUses(f *protoast.File, visit func(ast.DeclAnnotationUse)) {
	if f == nil || f.AST == nil {
		return
	}
	var walk func(decls seq.Indexer[ast.DeclAny])
	visitAll := func(uses seq.Indexer[ast.DeclAnnotationUse]) {
		for use := range seq.Values(uses) {
			visit(use)
		}
	}
	walk = func(decls seq.Indexer[ast.DeclAny]) {
		for decl := range seq.Values(decls) {
			switch decl.Kind() {
			case ast.DeclKindType:
				visitAll(decl.AsType().Annotations())
			case ast.DeclKindFunction:
				visitAll(decl.AsFunction().Annotations())
			case ast.DeclKindAnnotation:
				visitAll(decl.AsAnnotation().Annotations())
			case ast.DeclKindAnnotationUse:
				visit(decl.AsAnnotationUse())
			case ast.DeclKindDef:
				def := decl.AsDef()
				visitAll(def.Annotations())
				if body := def.Body(); !body.IsZero() {
					walk(body.Decls())
				}
			case ast.DeclKindBody:
				walk(decl.AsBody().Decls())
			}
		}
	}
	walk(f.AST.Decls())
}

// protoSensitiveClassNames harvests the workspace's sensitivity-class
// vocabulary: every string-literal class argument of a use whose leaf
// name is `sensitive`, across the current buffer and every indexed
// file. The leaf-name filter is deliberately looser than
// protoSensitiveUse's resolution — harvesting collects vocabulary,
// and a class used under an aliased import elsewhere is still the
// org's vocabulary. Reserved-prefix values (compile errors as of
// protocompile v0.21.0) and the empty class stay out. Sorted, unique.
func (s *Workspace) protoSensitiveClassNames(current *protoast.File) []string {
	seen := map[string]struct{}{}
	harvest := func(f *protoast.File) {
		protoWalkAnnotationUses(f, func(use ast.DeclAnnotationUse) {
			name := use.Name().Canonicalized()
			if name != "sensitive" && !strings.HasSuffix(name, ".sensitive") {
				return
			}
			class, ok := protoSensitiveClassValue(protoSensitiveClassArg(use))
			if !ok || class == "" ||
				class == "protowire" || strings.HasPrefix(class, "protowire.") {
				return
			}
			seen[class] = struct{}{}
		})
	}
	harvest(current)
	if idx := s.SnapshotProtoIndex(); idx != nil {
		for _, p := range idx.ImportPaths() {
			if f := idx.Lookup(p); f != nil {
				harvest(f)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for class := range seen {
		out = append(out, class)
	}
	slices.Sort(out)
	return out
}

// protoSensitiveClassItems builds the completion items for a cursor
// inside a @sensitive use's parens. Inside the class string literal
// the items are the bare class names (the client replaces the
// in-progress word between the quotes); outside a literal they carry
// the quotes. nil falls through to the generic argument completion —
// except inside a string literal, where function names could never
// be right, so an empty non-nil list suppresses them.
func (s *Workspace) protoSensitiveClassItems(doc *Document, pos protocol.Position) []protocol.CompletionItem {
	inString := false
	if off := doc.protoAST.OffsetIn(pos, s.positionEncoding); off >= 0 {
		if _, at := doc.protoAST.AST.Stream().Around(off); !at.IsZero() &&
			at.Kind() == token.String && protoast.Contains(at.Span(), off) {
			inString = true
		}
	}
	classes := s.protoSensitiveClassNames(doc.protoAST)
	if len(classes) == 0 {
		if inString {
			return []protocol.CompletionItem{}
		}
		return nil
	}
	items := make([]protocol.CompletionItem, 0, len(classes))
	for _, class := range classes {
		label := class
		if !inString {
			label = `"` + class + `"`
		}
		items = append(items, protocol.CompletionItem{
			Label:  label,
			Kind:   protocol.CompletionItemKindValue,
			Detail: "sensitivity class (workspace)",
		})
	}
	return items
}
