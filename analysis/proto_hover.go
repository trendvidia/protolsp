// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Hover for .proto documents (pxfed docs/design/proto-authoring.md).
// Hovers on type references (Foo in `Foo my_field = 1;`) resolve
// against declarations in the same file, in an imported on-disk file
// via the workspace index, or — for a read-only registry:// preview —
// in an imported registry file via the namespace lookup (goed#122).

package analysis

import (
	"context"
	"fmt"
	"strings"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/source"
	"github.com/trendvidia/protocompile/token"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/protoast"
)

// protoHoverFor renders hover content for a position in a .proto
// document, or returns nil for "no hover info." Mirrors HoverFor's
// nil-means-nothing convention so dispatchHover can treat both
// languages uniformly.
//
// Resolution order (first match wins):
//  1. Cursor on a field type ident → resolve the named type in this
//     file and render its declaration card.
//  2. Cursor on a message/enum/field/enum-value name ident → render
//     the declaration card for the enclosing node.
//
// Unknown positions (whitespace between decls, comments, the syntax
// directive line) return nil. Doc-comment harvesting walks the token
// stream backwards from the declaration, which lines up with
// protoc's convention for what gets attached as the documentation of
// a declaration.
func (s *Workspace) protoHoverFor(docURI protocol.DocumentURI, pos protocol.Position) *protocol.Hover {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	if doc.protoAST == nil {
		return nil
	}

	path := protoFindAt(doc.protoAST, pos, s.positionEncoding)
	leaf := path.Leaf()
	if leaf == nil {
		return nil
	}

	// Sub-workspace scope for a registry:// preview's cross-file
	// resolution (#215); "" for the root and every on-disk file. See
	// the twin in protoDefinitionFor.
	wsDir := doc.WorkspaceDir()

	// RFC-001 positions (#218 Phase B, #219 Phase C): annotation use
	// sites, type/function/annotation declaration names, RFC-001 type
	// slots, and function references. Checked first because the
	// classification below only understands DeclDef ancestry and
	// would return nil for all of these shapes.
	if hv := s.protoRFC001HoverFor(docURI, wsDir, doc.protoAST, path); hv != nil {
		return hv
	}

	// Function calls inside opaque engine-expression fragments (#220):
	// no AST value nodes exist for the walk above, but the compile
	// pass resolved every call site — hit-test the cursor against
	// those and render the function's declaration card.
	if hv := s.rfc001OpaqueCallHover(wsDir, doc, pos); hv != nil {
		return hv
	}

	// Only ident-token positions yield hover. Other tokens (keywords,
	// punctuation, literals) carry syntactic noise the user already
	// sees in the editor; spending markdown on them adds clutter.
	tok, ok := leaf.(token.Token)
	if !ok || tok.Kind() != token.Ident {
		return nil
	}

	// Idents inside declarations descend through the declaration's
	// name / type paths, so the parent is the ast.Path the ident is
	// a component of. Type references (`acme.demo.v1.Foo` in a
	// field's type slot) may be multi-component — classify and
	// resolve on the whole path, not just the leaf word the cursor
	// happens to be on.
	refPath, ok := path.Parent().(ast.Path)
	if !ok {
		return nil
	}
	defNode := path.Ancestor(func(n source.Spanner) bool {
		_, ok := n.(ast.DeclDef)
		return ok
	})
	def, ok := defNode.(ast.DeclDef)
	if !ok {
		return nil
	}

	if sameProtoNode(refPath, def.Name()) {
		// Cursor is on the declaration's own name.
		switch def.Classify() {
		case ast.DefKindField:
			return renderProtoFieldHover(doc.protoAST, def.AsField())
		case ast.DefKindMessage:
			return renderProtoMessageHover(doc.protoAST, def)
		case ast.DefKindEnum:
			return renderProtoEnumHover(doc.protoAST, def)
		case ast.DefKindEnumValue:
			return renderProtoEnumValueHover(doc.protoAST, def.AsEnumValue())
		}
		return nil
	}

	// Not the name — for a field-classified def the remaining path
	// position is the type slot (possibly nested under a repeated /
	// optional prefix or a map<...>'s type arguments).
	if def.Classify() == ast.DefKindField {
		if md := s.renderProtoFieldTypeHover(docURI, wsDir, doc.protoAST, refPath.Canonicalized(), protoRefScopeAt(doc.protoAST, path)); md != "" {
			return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: md}}
		}
	}
	return nil
}

// sameProtoNode reports whether two nodes are the same node
// occurrence, by span identity. The new AST hands out value-typed
// node handles, so the old pointer-identity check ("is this ident
// the same parsed token") becomes "does it cover the same source
// bytes of the same file" — equivalent within one parse, since two
// distinct nodes never share an identical span.
func sameProtoNode(a, b source.Spanner) bool {
	sa, sb := source.GetSpan(a), source.GetSpan(b)
	return !sa.IsZero() && !sb.IsZero() &&
		sa.File == sb.File && sa.Start == sb.Start && sa.End == sb.End
}

// renderProtoFieldHover formats a field declaration as a hover
// card. Shape:
//
//	**field_name** `type` (field N)
//	Defined in `<file>`
//
//	<leading doc comment, if any>
func renderProtoFieldHover(f *protoast.File, field ast.DefField) *protocol.Hover {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** `%s`", field.Name.Text(), protoFieldTypeLabel(field))
	if tag, ok := protoTagValue(field.Tag); ok {
		fmt.Fprintf(&b, " (field %d)", tag)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Defined in `%s`\n", f.Name())
	if comment := protoLeadingComment(f, field.Decl); comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: b.String()}}
}

// protoTagValue extracts a field tag / enum number as a uint64 when
// the expression is a plain number literal. Mirrors the old AST's
// Tag *UintLiteralNode nil-check + .Val read.
func protoTagValue(tag ast.ExprAny) (uint64, bool) {
	num := tag.AsLiteral().AsNumber()
	if num.IsZero() {
		return 0, false
	}
	v, _ := num.Int()
	return v, true
}

// renderProtoMessageHover formats a message declaration. Shape:
//
//	**message MessageName**
//	Defined in `<file>`
//
//	<leading doc comment, if any>
func renderProtoMessageHover(f *protoast.File, def ast.DeclDef) *protocol.Hover {
	var b strings.Builder
	fmt.Fprintf(&b, "**message %s**\n", def.Name().Canonicalized())
	fmt.Fprintf(&b, "Defined in `%s`\n", f.Name())
	if comment := protoLeadingComment(f, def); comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: b.String()}}
}

// renderProtoEnumHover formats an enum declaration. Shape:
//
//	**enum EnumName**
//	Defined in `<file>`
//
//	<leading doc comment, if any>
func renderProtoEnumHover(f *protoast.File, def ast.DeclDef) *protocol.Hover {
	var b strings.Builder
	fmt.Fprintf(&b, "**enum %s**\n", def.Name().Canonicalized())
	fmt.Fprintf(&b, "Defined in `%s`\n", f.Name())
	if comment := protoLeadingComment(f, def); comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: b.String()}}
}

// renderProtoEnumValueHover formats an enum-value declaration. Shape:
//
//	**VALUE_NAME** = N
//
//	<leading doc comment, if any>
func renderProtoEnumValueHover(f *protoast.File, v ast.DefEnumValue) *protocol.Hover {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**", v.Name.Text())
	if !v.Tag.IsZero() {
		// The tag expression's source text handles both positive
		// and negative numbers without re-implementing the union.
		if raw := v.Tag.Span().Text(); raw != "" {
			fmt.Fprintf(&b, " = %s", raw)
		}
	}
	b.WriteString("\n")
	if comment := protoLeadingComment(f, v.Decl); comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: b.String()}}
}

// renderProtoFieldTypeHover resolves a type reference (the possibly
// qualified name from a field's type slot) against declarations
// reachable from the current file and renders its declaration card.
// Priority: in-file declarations first (Phase 2), then imported
// files. For an on-disk document those imports resolve through the
// workspace index (Phase 3); for a registry:// preview they resolve
// through the preview's namespace lookup (goed#122), mirroring
// go-to-definition (goed#119). Returns the rendered markdown, or ""
// when the name doesn't resolve to any reachable declaration.
//
// For a name that doesn't resolve, we return "" — the user sees
// no hover, which is honest: we don't know what the type is. A
// missing import or a misconfigured workspace are the typical
// causes; protocompile's diagnostics surface those separately.
//
// workspaceDir scopes the registry:// cross-file resolution to the
// preview's sub-workspace registry (#215), matching the definition
// path; it is unused on the on-disk path.
//
// refScope is the reference site's lexical scope (protoRefScopeAt,
// #239); "" falls back to the file's package inside the resolvers.
func (s *Workspace) renderProtoFieldTypeHover(docURI protocol.DocumentURI, workspaceDir string, f *protoast.File, name, refScope string) string {
	if isProtoBuiltinType(name) {
		// Built-in scalars (string, int32, bool, …) — no hover.
		// Editors render the keyword color already; markdown
		// noise here adds nothing.
		return ""
	}
	// Strip leading dot from fully-qualified names (".acme.Foo");
	// the resolver matches against dotted names without the
	// absolute marker.
	name = strings.TrimPrefix(name, ".")

	result := s.resolveProtoTypeRef(f, name, refScope)

	// In-file declaration (same file) — render directly. Works for
	// both on-disk and registry:// documents.
	if !result.IsZero() && result.File == f {
		return renderProtoTypeLookupHover(result)
	}

	// Registry:// preview (goed#122): a cross-file type is declared in
	// ANOTHER registry file, resolved through the preview's namespace
	// lookup — the same path go-to-definition uses (goed#119). On-disk
	// index hits are discarded, mirroring the definition handler.
	if IsRegistryProtoURI(uri.URI(docURI)) {
		if ns, _, ok := ParseRegistryURI(uri.URI(docURI)); ok {
			if reg, _ := s.resolveRegistryProtoTypeLookup(context.Background(), ns, workspaceDir, f, name, refScope); !reg.IsZero() {
				return renderProtoTypeLookupHover(reg)
			}
		}
		if hv := s.rfc001RemoteHover(workspaceDir, f, rfc001KindType, name, refScope); hv != nil {
			return hv.Contents.Value
		}
		return ""
	}

	// On-disk cross-file declaration.
	if !result.IsZero() {
		return renderProtoTypeLookupHover(result)
	}

	// Remote fallback (#219 Phase C, #229): the name may be a
	// message, enum, or RFC-001 `type` alias declared in an import
	// the index doesn't hold — resolvable through the schema lookup
	// as served source or from the compiled descriptor.
	if hv := s.rfc001RemoteHover(workspaceDir, f, rfc001KindType, name, refScope); hv != nil {
		return hv.Contents.Value
	}
	return ""
}

// renderProtoTypeLookupHover renders the declaration card for a
// resolved type lookup (message or enum), or "" for a zero lookup.
// Shared by the in-file, on-disk cross-file, and registry cross-file
// hover paths so all three produce identical cards.
func renderProtoTypeLookupHover(r protoTypeLookup) string {
	if r.IsZero() {
		return ""
	}
	if r.IsAlias() {
		return renderProtoTypeAliasHover(r.File, r.Alias).Contents.Value
	}
	if r.IsEnum() {
		return renderProtoEnumHover(r.File, r.Def).Contents.Value
	}
	return renderProtoMessageHover(r.File, r.Def).Contents.Value
}

// protoFieldTypeLabel renders a field's type for hover. For scalars
// and message/enum types the type path's canonical text is exactly
// what we want; repeated / optional / required prefixes are part of
// the type in the new AST and are prepended in source order.
func protoFieldTypeLabel(field ast.DefField) string {
	var parts []string
	for prefixed := range field.Type.Prefixes() {
		if kw := prefixed.PrefixToken().Text(); kw != "" {
			parts = append(parts, kw)
		}
	}
	base := field.Type.RemovePrefixes()
	if p := base.AsPath(); !p.IsZero() {
		parts = append(parts, p.Canonicalized())
	} else if raw := base.Span().Text(); raw != "" {
		// map<K, V> and friends — the raw source text reads fine.
		parts = append(parts, raw)
	}
	return strings.Join(parts, " ")
}

// protoLeadingComment returns the leading doc comment for a node
// joined as a single block (multi-line comments preserved). Returns
// "" when there are no leading comments — caller skips the section.
//
// Convention: protoc treats the comment block immediately above a
// declaration as documentation for that declaration. Comments are
// ordinary tokens in the new AST, so this walks the token stream
// backwards from the declaration's first token, collecting comments
// until the previous declaration's last token. A comment on the
// same line as that previous token is its trailing comment, not our
// documentation, and is dropped. The `//` / `/* */` markers are
// stripped and lines trimmed so the markdown reads cleanly.
func protoLeadingComment(f *protoast.File, n source.Spanner) string {
	span := source.GetSpan(n)
	if f == nil || f.AST == nil || span.IsZero() {
		return ""
	}
	_, first := f.AST.Stream().Around(span.Start)
	if first.IsZero() {
		return ""
	}
	var comments []token.Token
	cursor := token.NewCursorAt(first)
	for {
		prev := cursor.PrevSkippable()
		if prev.IsZero() {
			break
		}
		if prev.Kind() == token.Space {
			continue
		}
		if prev.Kind() == token.Comment {
			comments = append([]token.Token{prev}, comments...)
			continue
		}
		// Previous declaration's last token. A comment starting on
		// that token's line is its trailing comment — drop it.
		if len(comments) > 0 && f.Src != nil {
			prevLine := f.Src.LineByOffset(source.GetSpan(prev).End)
			firstLine := f.Src.LineByOffset(source.GetSpan(comments[0]).Start)
			if prevLine == firstLine {
				comments = comments[1:]
			}
		}
		break
	}
	if len(comments) == 0 {
		return ""
	}
	var lines []string
	for _, c := range comments {
		text := c.Span().Text()
		// Strip syntactic markers — `//`, leading whitespace,
		// trailing CR. `/* */` block comments come through as the
		// full text including markers; trim them too.
		text = strings.TrimPrefix(text, "//")
		text = strings.TrimPrefix(text, "/*")
		text = strings.TrimSuffix(text, "*/")
		// Per-line trim for multi-line comments.
		for _, line := range strings.Split(text, "\n") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	// Drop empty leading / trailing lines so the markdown doesn't
	// start with an awkward gap.
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// isProtoBuiltinType reports whether name is one of protobuf's
// built-in scalar types. Used to short-circuit the type-reference
// hover — no point looking up `string` in the local declarations.
func isProtoBuiltinType(name string) bool {
	switch name {
	case "double", "float", "int32", "int64", "uint32", "uint64",
		"sint32", "sint64", "fixed32", "fixed64", "sfixed32", "sfixed64",
		"bool", "string", "bytes":
		return true
	}
	return false
}
