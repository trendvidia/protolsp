// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Completion + navigation for RFC-001 declarations. Phase A
// gave the schema-extension constructs semantic tokens; Phase B
// wired the annotation surfaces:
//
//   - `@` completion offers the annotation names declared in the
//     current file and its imports,
//   - hover on a `@name` use site (or the annotation declaration's
//     own name) renders the declaration card, and
//   - go-to-definition on a use site jumps to the declaration.
//
// Phase C extends hover + definition to the remaining RFC-001
// constructs: `type` aliases (as field types, alias values, and
// parameter types) and `function` declarations (referenced from
// parsed annotation arguments), plus self-navigation on both
// declarations' names.
//
// Resolution mirrors the message/enum type-reference resolver
// (resolveProtoTypeRef): in-file declarations first, then files the
// current file imports, with dotted qualifiers matched against the
// imported file's package. RFC-001 declarations are top-level only,
// so the walks never recurse into bodies.

package analysis

import (
	"fmt"
	"strings"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/source"
	"github.com/trendvidia/protocompile/token"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// protoAnnotationLookup is the resolved declaration for an
// annotation-reference resolve attempt. The protoTypeLookup analogue
// for `annotation` declarations; both zero when nothing matched.
type protoAnnotationLookup struct {
	File *protoast.File
	Decl ast.DeclAnnotation
}

// IsZero reports whether the lookup found nothing.
func (r protoAnnotationLookup) IsZero() bool {
	return r.File == nil
}

// resolveProtoAnnotationRef finds the `annotation` declaration for
// `name` (the canonicalized path of a use site, possibly dotted)
// reachable from currentFile. Same priority + scoping rules as
// resolveProtoTypeRef: the name is interpreted against the
// enclosing scopes of the referencing file's package, so a bare name
// binds in-file and in same-package (or enclosing-package) imports —
// never across unrelated packages — and a dotted name works under
// any enclosing-scope interpretation (`demo.v1.checked` written in
// acme.demo.v1).
func (s *Workspace) resolveProtoAnnotationRef(currentFile *protoast.File, name string) protoAnnotationLookup {
	if currentFile == nil || name == "" {
		return protoAnnotationLookup{}
	}
	refPkg := filePackage(currentFile)
	for _, leaf := range protoTopLevelNameCandidates(name, refPkg, refPkg) {
		if a := findAnnotationByName(currentFile, leaf); !a.IsZero() {
			return protoAnnotationLookup{File: currentFile, Decl: a}
		}
	}

	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return protoAnnotationLookup{}
	}
	for _, imp := range protoImports(currentFile) {
		importedFile := idx.Lookup(imp)
		if importedFile == nil {
			continue
		}
		for _, leaf := range protoTopLevelNameCandidates(name, refPkg, filePackage(importedFile)) {
			if a := findAnnotationByName(importedFile, leaf); !a.IsZero() {
				return protoAnnotationLookup{File: importedFile, Decl: a}
			}
		}
	}
	return protoAnnotationLookup{}
}

// findAnnotationByName returns the top-level `annotation` declaration
// named `name`, or the zero DeclAnnotation. Top-level only: RFC-001
// annotations can't nest inside message/service bodies.
func findAnnotationByName(f *protoast.File, name string) ast.DeclAnnotation {
	if f == nil || f.AST == nil || name == "" {
		return ast.DeclAnnotation{}
	}
	for decl := range seq.Values(f.AST.Decls()) {
		if a := decl.AsAnnotation(); !a.IsZero() && a.Name().Text() == name {
			return a
		}
	}
	return ast.DeclAnnotation{}
}

// annotationUseNameAt extracts the annotation name a cursor position
// refers to, when the descent path lands on a `@name` use site.
// Handles two leaf shapes:
//
//   - the `@` sigil itself, whose leaf is the DeclAnnotationUse (the
//     sigil isn't a descendable child, so descent stops at the use),
//   - an ident inside the use's name path — distinguished from
//     idents in the argument list by span-matching the parent path
//     against the use's Name.
func annotationUseNameAt(path *protoPath) (string, bool) {
	if use, ok := path.Leaf().(ast.DeclAnnotationUse); ok {
		name := use.Name().Canonicalized()
		return name, name != ""
	}
	tok, ok := path.Leaf().(token.Token)
	if !ok || tok.Kind() != token.Ident {
		return "", false
	}
	refPath, ok := path.Parent().(ast.Path)
	if !ok {
		return "", false
	}
	useNode := path.Ancestor(func(n source.Spanner) bool {
		_, ok := n.(ast.DeclAnnotationUse)
		return ok
	})
	use, ok := useNode.(ast.DeclAnnotationUse)
	if !ok || !sameProtoNode(refPath, use.Name()) {
		return "", false
	}
	name := use.Name().Canonicalized()
	return name, name != ""
}

// rfc001DeclNameAt classifies a cursor on an RFC-001 declaration's
// own name ident (`annotation NAME`, `type NAME = …`, `function
// NAME(…)`). Returns the name token (the self-definition target) and
// a hover renderer bound to the declaration, or ok=false when the
// cursor is elsewhere. The name is a direct token child of each decl
// node — unlike DeclDef names, which descend through an ast.Path.
func rfc001DeclNameAt(f *protoast.File, path *protoPath) (name token.Token, hover func() *protocol.Hover, ok bool) {
	tok, isTok := path.Leaf().(token.Token)
	if !isTok || tok.Kind() != token.Ident {
		return token.Token{}, nil, false
	}
	switch decl := path.Parent().(type) {
	case ast.DeclAnnotation:
		if sameProtoNode(tok, decl.Name()) {
			return decl.Name(), func() *protocol.Hover { return renderProtoAnnotationHover(f, decl) }, true
		}
	case ast.DeclType:
		if sameProtoNode(tok, decl.Name()) {
			return decl.Name(), func() *protocol.Hover { return renderProtoTypeAliasHover(f, decl) }, true
		}
	case ast.DeclFunction:
		if sameProtoNode(tok, decl.Name()) {
			return decl.Name(), func() *protocol.Hover { return renderProtoFunctionHover(f, decl) }, true
		}
	}
	return token.Token{}, nil, false
}

// rfc001TypeRefAt extracts the type-reference path when the cursor
// sits on one of the type slots RFC-001 declarations introduce:
//
//   - the aliased value of a `type` declaration
//     (`type PrimaryEmail = Email;` — cursor on Email),
//   - a function parameter's type (`function f(m: Money)`),
//   - an annotation parameter's type (`annotation d(m: Money = …)`)
//     — the type slot only; a parameter's DEFAULT expression is a
//     value, not a type, and deliberately doesn't match.
//
// The returned name resolves through the same machinery as field
// type references (messages, enums, and — new in Phase C — type
// aliases), so all type slots behave identically.
func rfc001TypeRefAt(path *protoPath) (string, bool) {
	tok, ok := path.Leaf().(token.Token)
	if !ok || tok.Kind() != token.Ident {
		return "", false
	}
	refPath, ok := path.Parent().(ast.Path)
	if !ok {
		return "", false
	}
	n := path.Ancestor(func(n source.Spanner) bool {
		switch n.(type) {
		case ast.DeclType, ast.DeclFunctionParam, ast.DeclAnnotationParam:
			return true
		}
		return false
	})
	var slot source.Spanner
	switch decl := n.(type) {
	case ast.DeclType:
		slot = decl.Value()
	case ast.DeclFunctionParam:
		slot = decl.Type().RemovePrefixes().AsPath().Path
	case ast.DeclAnnotationParam:
		slot = decl.Type().RemovePrefixes().AsPath().Path
	default:
		return "", false
	}
	if !sameProtoNode(refPath, slot) {
		return "", false
	}
	return refPath.Canonicalized(), true
}

// rfc001FunctionRefAt extracts the referenced function name when the
// cursor sits on a parsed path argument of an annotation use —
// `@check(is_email)` with the cursor on is_email. Only parsed
// ExprPath captures classify; opaque engine-expression fragments
// (AnnotationUseArg.Value() zero) have no AST nodes to hit, so
// function calls inside them can't navigate (that's the source-map's
// job for compiled artifacts, and runtime-error mapping's for live
// buffers).
func rfc001FunctionRefAt(path *protoPath) (string, bool) {
	tok, ok := path.Leaf().(token.Token)
	if !ok || tok.Kind() != token.Ident {
		return "", false
	}
	refPath, ok := path.Parent().(ast.Path)
	if !ok {
		return "", false
	}
	argNode := path.Ancestor(func(n source.Spanner) bool {
		_, ok := n.(ast.AnnotationUseArg)
		return ok
	})
	arg, ok := argNode.(ast.AnnotationUseArg)
	if !ok || !sameProtoNode(refPath, arg.Value()) {
		return "", false
	}
	return refPath.Canonicalized(), true
}

// protoRFC001HoverFor renders the declaration card for a cursor on
// any RFC-001 construct — an annotation use site, a declaration's
// own name, a type slot, or a function reference — or nil when the
// position is none of those (caller falls through to the classic
// message/enum hover path).
//
// Registry:// previews only surface in-file resolutions on the
// annotation/function paths: their cross-file targets live in other
// registry files, and the on-disk index would leak file:// content
// into the read-only browsing view. Type-slot references delegate to
// renderProtoFieldTypeHover, which already carries the registry
// discipline (and the sub-workspace scope via wsDir).
func (s *Workspace) protoRFC001HoverFor(docURI protocol.DocumentURI, wsDir string, f *protoast.File, path *protoPath) *protocol.Hover {
	if _, hover, ok := rfc001DeclNameAt(f, path); ok {
		return hover()
	}
	if name, ok := annotationUseNameAt(path); ok {
		name = strings.TrimPrefix(name, ".")
		result := s.resolveProtoAnnotationRef(f, name)
		if !result.IsZero() && (result.File == f || !IsRegistryProtoURI(uri.URI(docURI))) {
			return renderProtoAnnotationHover(result.File, result.Decl)
		}
		return s.rfc001RemoteHover(wsDir, f, rfc001KindAnnotation, name, "")
	}
	if name, ok := rfc001FunctionRefAt(path); ok {
		name = strings.TrimPrefix(name, ".")
		result := s.resolveProtoFunctionRef(f, name)
		if !result.IsZero() && (result.File == f || !IsRegistryProtoURI(uri.URI(docURI))) {
			return renderProtoFunctionHover(result.File, result.Decl)
		}
		return s.rfc001RemoteHover(wsDir, f, rfc001KindFunction, name, "")
	}
	if name, ok := rfc001TypeRefAt(path); ok {
		if md := s.renderProtoFieldTypeHover(docURI, wsDir, f, name, protoRefScopeAt(f, path)); md != "" {
			return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: md}}
		}
	}
	return nil
}

// protoRFC001DefinitionFor resolves go-to-definition for a cursor on
// any RFC-001 construct: use sites and references jump to their
// declaration; a declaration's own name jumps to itself (matching
// the degenerate-but-harmless behavior of message/enum names).
// Returns nil when the position isn't RFC-001-related.
func (s *Workspace) protoRFC001DefinitionFor(docURI protocol.DocumentURI, wsDir string, f *protoast.File, path *protoPath) *protocol.Location {
	if name, _, ok := rfc001DeclNameAt(f, path); ok {
		return &protocol.Location{URI: docURI, Range: nodeLSPRange(f, name, s.positionEncoding)}
	}
	if name, ok := annotationUseNameAt(path); ok {
		name = strings.TrimPrefix(name, ".")
		result := s.resolveProtoAnnotationRef(f, name)
		if !result.IsZero() {
			if loc := s.rfc001DeclLocation(docURI, f, result.File, result.Decl.Name()); loc != nil {
				return loc
			}
		}
		return s.rfc001RemoteLocation(wsDir, f, rfc001KindAnnotation, name, "")
	}
	if name, ok := rfc001FunctionRefAt(path); ok {
		name = strings.TrimPrefix(name, ".")
		result := s.resolveProtoFunctionRef(f, name)
		if !result.IsZero() {
			if loc := s.rfc001DeclLocation(docURI, f, result.File, result.Decl.Name()); loc != nil {
				return loc
			}
		}
		return s.rfc001RemoteLocation(wsDir, f, rfc001KindFunction, name, "")
	}
	if name, ok := rfc001TypeRefAt(path); ok {
		return s.resolveProtoTypeDefinition(docURI, wsDir, f, name, protoRefScopeAt(f, path))
	}
	return nil
}

// rfc001DeclLocation builds the Location for a resolved RFC-001
// declaration. In-file targets stamp the request URI so registry://
// previews land on the preview document (protoFileURI can't rebuild
// a URI from their relative parse path); cross-file targets use the
// declaring file's own file:// URI — except from registry previews,
// which never leak on-disk locations.
func (s *Workspace) rfc001DeclLocation(docURI protocol.DocumentURI, currentFile, declFile *protoast.File, name token.Token) *protocol.Location {
	if declFile == currentFile {
		return &protocol.Location{URI: docURI, Range: nodeLSPRange(currentFile, name, s.positionEncoding)}
	}
	if IsRegistryProtoURI(uri.URI(docURI)) {
		return nil
	}
	return protoLocation(declFile, name, s.positionEncoding)
}

// renderProtoAnnotationHover formats an annotation declaration as a
// hover card. Shape mirrors the message/enum cards:
//
//	**annotation name(param: type)**
//	Defined in `<file>`
//
//	<leading doc comment, if any>
func renderProtoAnnotationHover(f *protoast.File, decl ast.DeclAnnotation) *protocol.Hover {
	var b strings.Builder
	fmt.Fprintf(&b, "**annotation %s**\n", protoAnnotationSignature(decl))
	fmt.Fprintf(&b, "Defined in `%s`\n", f.Name())
	if comment := protoLeadingComment(f, decl); comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: b.String()}}
}

// protoAnnotationSignature renders a declaration's name plus its
// parameter list as written in source — `description(text: string)`
// — or just the name for a parameterless `annotation required;`.
// The parens token is a fused tree whose span covers open..close, so
// its raw text is exactly the parameter list.
func protoAnnotationSignature(decl ast.DeclAnnotation) string {
	name := decl.Name().Text()
	if parens := decl.Parens(); !parens.IsZero() {
		return name + parens.Span().Text()
	}
	return name
}

// ─── `type` aliases + `function` declarations ─────

// findTypeAliasByName returns the top-level `type` declaration named
// `name`, or the zero DeclType. Top-level only, like annotations.
func findTypeAliasByName(f *protoast.File, name string) ast.DeclType {
	if f == nil || f.AST == nil || name == "" {
		return ast.DeclType{}
	}
	for decl := range seq.Values(f.AST.Decls()) {
		if t := decl.AsType(); !t.IsZero() && t.Name().Text() == name {
			return t
		}
	}
	return ast.DeclType{}
}

// findFunctionByName returns the top-level `function` declaration
// named `name`, or the zero DeclFunction.
func findFunctionByName(f *protoast.File, name string) ast.DeclFunction {
	if f == nil || f.AST == nil || name == "" {
		return ast.DeclFunction{}
	}
	for decl := range seq.Values(f.AST.Decls()) {
		if fn := decl.AsFunction(); !fn.IsZero() && fn.Name().Text() == name {
			return fn
		}
	}
	return ast.DeclFunction{}
}

// protoFunctionLookup is the resolved declaration for a function-
// reference resolve attempt. The protoAnnotationLookup analogue for
// `function` declarations.
type protoFunctionLookup struct {
	File *protoast.File
	Decl ast.DeclFunction
}

// IsZero reports whether the lookup found nothing.
func (r protoFunctionLookup) IsZero() bool {
	return r.File == nil
}

// resolveProtoFunctionRef finds the `function` declaration for
// `name` reachable from currentFile. Same priority + scoping rules
// as resolveProtoAnnotationRef.
func (s *Workspace) resolveProtoFunctionRef(currentFile *protoast.File, name string) protoFunctionLookup {
	if currentFile == nil || name == "" {
		return protoFunctionLookup{}
	}
	refPkg := filePackage(currentFile)
	for _, leaf := range protoTopLevelNameCandidates(name, refPkg, refPkg) {
		if fn := findFunctionByName(currentFile, leaf); !fn.IsZero() {
			return protoFunctionLookup{File: currentFile, Decl: fn}
		}
	}

	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return protoFunctionLookup{}
	}
	for _, imp := range protoImports(currentFile) {
		importedFile := idx.Lookup(imp)
		if importedFile == nil {
			continue
		}
		for _, leaf := range protoTopLevelNameCandidates(name, refPkg, filePackage(importedFile)) {
			if fn := findFunctionByName(importedFile, leaf); !fn.IsZero() {
				return protoFunctionLookup{File: importedFile, Decl: fn}
			}
		}
	}
	return protoFunctionLookup{}
}

// renderProtoTypeAliasHover formats a `type` declaration as a hover
// card. Shape mirrors the annotation card:
//
//	**type PrimaryEmail = Email**
//	Defined in `<file>`
//
//	<leading doc comment, if any>
func renderProtoTypeAliasHover(f *protoast.File, decl ast.DeclType) *protocol.Hover {
	var b strings.Builder
	fmt.Fprintf(&b, "**type %s**\n", protoTypeAliasSignature(decl))
	fmt.Fprintf(&b, "Defined in `%s`\n", f.Name())
	if comment := protoLeadingComment(f, decl); comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: b.String()}}
}

// protoTypeAliasSignature renders `Name = <value as written>` — or
// just the name for a mid-edit declaration missing its value.
func protoTypeAliasSignature(decl ast.DeclType) string {
	name := decl.Name().Text()
	if value := decl.Value(); !value.IsZero() {
		if raw := value.Span().Text(); raw != "" {
			return name + " = " + raw
		}
	}
	return name
}

// renderProtoFunctionHover formats a `function` declaration as a
// hover card, same shape as the annotation card.
func renderProtoFunctionHover(f *protoast.File, decl ast.DeclFunction) *protocol.Hover {
	var b strings.Builder
	fmt.Fprintf(&b, "**function %s**\n", protoFunctionSignature(decl))
	fmt.Fprintf(&b, "Defined in `%s`\n", f.Name())
	if comment := protoLeadingComment(f, decl); comment != "" {
		b.WriteString("\n")
		b.WriteString(comment)
		b.WriteString("\n")
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: b.String()}}
}

// protoFunctionSignature renders a declaration's name plus its
// parameter list as written in source — `is_email(s: string)`.
func protoFunctionSignature(decl ast.DeclFunction) string {
	name := decl.Name().Text()
	if parens := decl.Parens(); !parens.IsZero() {
		return name + parens.Span().Text()
	}
	return name
}

// ─── `@` completion ──────────────────────────────────────────────

// protoAnnotationUsePrefix detects whether the cursor sits in a
// `@<partial>` or `@<qualifier>.<partial>` position and returns the
// dotted qualifier ("" when the cursor follows the `@` directly or
// mid-way through the first segment). Same permissive text-scan
// posture as protoDotCompletionPrefix: string/comment positions
// aren't excluded, the client's prefix filter suppresses the rare
// false positive.
//
// Examples (cursor at <c>):
//
//	@<c>                → ("", true)
//	@desc<c>            → ("", true)      // leaf filters client-side
//	@acme.demo.<c>      → ("acme.demo", true)
//	@acme.demo.che<c>   → ("acme.demo", true)
//	desc<c>             → ("", false)     // no @
func protoAnnotationUsePrefix(src []byte, pos protocol.Position, enc positions.Encoding) (string, bool) {
	off, err := positions.Offset(src, pos, enc)
	if err != nil || off < 0 || off > len(src) {
		return "", false
	}
	// Walk back through the in-progress leaf identifier.
	leafStart := off
	for leafStart > 0 && isProtoIdentByte(src[leafStart-1]) {
		leafStart--
	}
	if leafStart > 0 && src[leafStart-1] == '@' {
		return "", true
	}
	// A dotted use: walk back across `.identifier` segments to the
	// sigil. Any malformed step (empty segment, missing @) bows out
	// so dot completion can take its shot at the position instead.
	if leafStart == 0 || src[leafStart-1] != '.' {
		return "", false
	}
	qualEnd := leafStart - 1
	qualStart := qualEnd
	for qualStart > 0 {
		idStart := qualStart
		for idStart > 0 && isProtoIdentByte(src[idStart-1]) {
			idStart--
		}
		if idStart == qualStart {
			return "", false
		}
		qualStart = idStart
		if qualStart > 0 && src[qualStart-1] == '.' {
			qualStart--
			continue
		}
		break
	}
	if qualStart == 0 || src[qualStart-1] != '@' {
		return "", false
	}
	return string(src[qualStart:qualEnd]), true
}

// protoRFC001CompletionItems is the shared emit walk for top-level
// RFC-001 declaration names (annotations after `@`, functions inside
// annotation arguments), filtered by the dotted qualifier the user
// has typed (if any).
//
//   - qualifier "" → in-file declarations by bare name + imported
//     declarations qualified with their package (same shape as
//     protoImportedTypeItems: cross-package uses need the qualifier,
//     suggesting a bare name would suggest something invalid).
//   - qualifier "acme.demo.v1" (or any enclosing-scope interpretation
//     naming a package — `demo.v1` written in acme.demo.v1) →
//     bare names from files whose package it names (the qualifier is
//     already typed), plus in-file declarations when it names the
//     current package.
//
// Iteration is bounded by the import list, keeping the suggestion
// set to names the file can actually bind.
func (s *Workspace) protoRFC001CompletionItems(currentFile *protoast.File, qualifier, what string, kind protocol.CompletionItemKind, names func(*protoast.File) []string) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	add := func(label, provenance string) {
		items = append(items, protocol.CompletionItem{
			Label:      label,
			Kind:       kind,
			Detail:     what + " (" + provenance + ")",
			InsertText: label,
		})
	}
	currentPkg := filePackage(currentFile)
	if qualifier == "" || protoScopeQualifiesPackage(qualifier, currentPkg, currentPkg) {
		for _, name := range names(currentFile) {
			add(name, "this file")
		}
	}
	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return items
	}
	for _, importPath := range protoImports(currentFile) {
		imported := idx.Lookup(importPath)
		if imported == nil {
			continue
		}
		pkg := filePackage(imported)
		if qualifier != "" && !protoScopeQualifiesPackage(qualifier, currentPkg, pkg) {
			continue
		}
		for _, name := range names(imported) {
			label := name
			if qualifier == "" {
				label = qualifyProtoName(pkg, name)
			}
			add(label, importPath)
		}
	}
	return items
}

// protoAnnotationCompletionItems emits a CompletionItem for every
// annotation declaration reachable from the current file — the
// candidate set for a cursor after `@`.
func (s *Workspace) protoAnnotationCompletionItems(currentFile *protoast.File, qualifier string) []protocol.CompletionItem {
	// No Decorator kind exists in LSP; Function reads closest —
	// annotations declare a callable-looking parameter list.
	return s.protoRFC001CompletionItems(currentFile, qualifier, "annotation", protocol.CompletionItemKindFunction, protoAnnotationNames)
}

// protoFunctionCompletionItems emits a CompletionItem for every
// `function` declaration reachable from the current file — the
// candidate set for a cursor inside an annotation use's argument
// parens.
func (s *Workspace) protoFunctionCompletionItems(currentFile *protoast.File, qualifier string) []protocol.CompletionItem {
	return s.protoRFC001CompletionItems(currentFile, qualifier, "function", protocol.CompletionItemKindFunction, protoFunctionNames)
}

// protoTopLevelNames collects the names of the file's top-level
// declarations selected by pick, in source order. A zero token — the
// decl isn't the picked kind, or a mid-edit declaration is missing
// its name — reads as "" and is skipped.
func protoTopLevelNames(f *protoast.File, pick func(ast.DeclAny) token.Token) []string {
	if f == nil || f.AST == nil {
		return nil
	}
	var out []string
	for decl := range seq.Values(f.AST.Decls()) {
		if name := pick(decl).Text(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// protoAnnotationNames lists the file's top-level `annotation`
// declaration names.
func protoAnnotationNames(f *protoast.File) []string {
	return protoTopLevelNames(f, func(d ast.DeclAny) token.Token { return d.AsAnnotation().Name() })
}

// protoFunctionNames lists the file's top-level `function`
// declaration names.
func protoFunctionNames(f *protoast.File) []string {
	return protoTopLevelNames(f, func(d ast.DeclAny) token.Token { return d.AsFunction().Name() })
}

// protoTypeAliasNames lists the file's top-level `type` alias names.
func protoTypeAliasNames(f *protoast.File) []string {
	return protoTopLevelNames(f, func(d ast.DeclAny) token.Token { return d.AsType().Name() })
}
