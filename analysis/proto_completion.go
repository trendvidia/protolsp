// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Completion for .proto documents. Phase 4 of the proto-authoring
// plan: suggest keywords + type names contextually based on the
// enclosing block at the cursor. Phase 4a delivers keywords (file,
// message, enum, service, oneof scopes) and field-type names
// (built-in scalars + in-file message/enum declarations + types
// reachable through imports).
//
// Filtering is the LSP client's job: we return the full candidate
// set for the scope and let VS Code / Neovim narrow against the
// user's in-progress identifier. That matches the pattern the pxf
// completion handler uses and is what protocol §completion expects
// for `triggerKind == Invoked`.

package analysis

import (
	"strings"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// ProtoCompletionFor handles textDocument/completion for .proto
// files. Returns nil for "no completions" (treated as a null
// CompletionList by the dispatcher; see dispatchCompletion's notes).
func (s *Workspace) ProtoCompletionFor(docURI protocol.DocumentURI, pos protocol.Position) *protocol.CompletionList {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	// The parser keeps a partial AST on parse failures, so this
	// rarely trips — only a document the analyze pipeline never
	// touched has nothing to walk.
	if doc.protoAST == nil {
		return nil
	}

	// Annotation-use completion runs first: a cursor
	// after `@` (possibly mid-name, possibly package-qualified) is
	// unambiguously an RFC-001 annotation use, so declared annotation
	// names are the only sensible candidates — and the check must
	// precede dot completion, which would otherwise grab a qualified
	// `@acme.demo.<c>` and try to resolve `acme.demo` as a type.
	if qualifier, ok := protoAnnotationUsePrefix(doc.Source, pos, s.positionEncoding); ok {
		return protoCompletionList(s.protoAnnotationCompletionItems(doc.protoAST, qualifier))
	}

	// Annotation-argument completion: a cursor inside the
	// argument parens of a `@name(…)` use offers the declared
	// `function` names — the parsed-path captures RFC-001 §5.1
	// gives meaning to — mirroring how `@` completes annotation
	// names. Runs before dot completion so a package-qualified
	// argument (`@check(acme.demo.v1.<c>)`) completes the imported
	// functions bare instead of drilling into the package's types;
	// falls through when no function resolves, so the position can
	// still get the dot/scope treatment (an argument may equally be
	// an enum value or literal).
	//
	// A use resolving to protowire.schema.v1.sensitive takes its
	// class vocabulary instead: the one parameter is an
	// org-defined class string, so the workspace's harvested class
	// names are the answer and function names never are.
	if use := protoAnnotationUseAt(doc.protoAST, pos, s.positionEncoding); !use.IsZero() {
		if s.protoSensitiveUse(doc.protoAST, use) {
			if items := s.protoSensitiveClassItems(doc, pos); items != nil {
				return protoCompletionList(items)
			}
		}
		qualifier, _ := protoDotCompletionPrefix(doc.Source, pos, s.positionEncoding)
		if items := s.protoFunctionCompletionItems(doc.protoAST, qualifier); len(items) > 0 {
			return protoCompletionList(items)
		}
	}

	// The cursor's lexical scope steers dot completion, the minimal
	// labels of in-file type items, and nothing else below —
	// computed once here.
	refScope := protoRefScopeAt(doc.protoAST, protoFindAt(doc.protoAST, pos, s.positionEncoding))
	cursorChain := protoCursorChain(refScope, filePackage(doc.protoAST))

	// Dot completion runs next: when the cursor follows
	// `<qualifier>.<partial>`, the user is navigating into a type
	// or package and the scope-based fallback (keywords + every
	// reachable type) would just bury the real answer under noise.
	// The qualifier resolves through the cursor's enclosing scopes
	// (protoRefScopeAt) — the same interpretation the type
	// resolver gives a reference written there. Falling through to
	// scope completion only happens when the qualifier doesn't
	// resolve (or there isn't one).
	if qualifier, ok := protoDotCompletionPrefix(doc.Source, pos, s.positionEncoding); ok {
		if items := s.protoQualifiedCompletions(doc.protoAST, qualifier, refScope); items != nil {
			return protoCompletionList(items)
		}
	}

	scope := protoScopeAt(doc.protoAST, pos, s.positionEncoding)
	switch scope {
	case protoScopeFile, protoScopeUnknown:
		// Unknown lands here too: it's the safer fallback. If
		// descend couldn't pin a scope (e.g. the cursor is after
		// EOF or in a stretch of whitespace the parser collapsed
		// out of the AST), file-scope keywords are still the most
		// useful thing to offer — the user is plausibly about to
		// start a new top-level declaration.
		return protoCompletionList(protoFileScopeKeywords())
	case protoScopeMessage, protoScopeOneof:
		// Message body (and oneof body, which lives inside a
		// message and has a near-identical grammar) gets keywords
		// + the full type universe. Client narrows by typed prefix:
		// `mes…` → message keyword, `int…` → int32/int64, `Foo` →
		// in-file or imported Foo. Returning everything is cheaper
		// than guessing the user's intent from line position.
		items := protoMessageScopeKeywords(scope == protoScopeMessage)
		items = append(items, protoBuiltinTypeItems()...)
		items = append(items, protoInFileTypeItems(doc.protoAST, cursorChain)...)
		items = append(items, s.protoImportedTypeItems(doc.protoAST)...)
		return protoCompletionList(items)
	case protoScopeEnum:
		return protoCompletionList(protoEnumScopeKeywords())
	case protoScopeService:
		return protoCompletionList(protoServiceScopeKeywords())
	case protoScopeRFC001Param:
		// Parameter-type slot of a `function` or `annotation`
		// declaration: the legal tokens are exactly the
		// field-type universe — scalars, in-file and imported
		// types — with no declaration keywords.
		items := protoBuiltinTypeItems()
		items = append(items, protoInFileTypeItems(doc.protoAST, cursorChain)...)
		items = append(items, s.protoImportedTypeItems(doc.protoAST)...)
		return protoCompletionList(items)
	}
	return nil
}

// protoScope is the enclosing block kind at a cursor position.
// Determines which keyword set + which symbol set we emit.
type protoScope int

const (
	protoScopeUnknown protoScope = iota
	protoScopeFile
	protoScopeMessage
	protoScopeEnum
	protoScopeService
	protoScopeOneof
	// protoScopeRFC001Param is the parameter list of a `function` or
	// `annotation` declaration — a type position.
	protoScopeRFC001Param
)

// protoScopeAt classifies the cursor position by walking the descent
// path from leaf to root and returning the innermost scope-defining
// node. Falls back to file scope when no inner block contains the
// cursor (the common case at top-level, between declarations).
func protoScopeAt(parsed *protoast.File, pos protocol.Position, enc positions.Encoding) protoScope {
	path := protoFindAt(parsed, pos, enc)
	if path == nil || parsed == nil {
		return protoScopeUnknown
	}
	// The RFC-001 param checks below need the raw offset: the descent
	// path alone can't tell "inside the parens" from "on the
	// declaration's own name" when the cursor sits between tokens.
	offset := parsed.OffsetIn(pos, enc)
	// Walk leaf → root so the innermost scope wins. A field inside a
	// oneof inside a message returns oneof, not message — that's the
	// scope where the next token will live.
	for i := len(path.Nodes) - 1; i >= 0; i-- {
		switch n := path.Nodes[i].(type) {
		case ast.DeclDef:
			switch n.Classify() {
			case ast.DefKindOneof:
				return protoScopeOneof
			case ast.DefKindMessage:
				return protoScopeMessage
			case ast.DefKindEnum:
				return protoScopeEnum
			case ast.DefKindService:
				return protoScopeService
			}
		case ast.DeclFunctionParam, ast.DeclAnnotationParam:
			return protoScopeRFC001Param
		case ast.DeclFunction:
			// Inside the parens but between param nodes (e.g. the
			// whitespace after `s:` in a mid-edit `function f(s: )`)
			// descent stops at the declaration itself; the parens
			// span disambiguates from a cursor on the name.
			if protoast.Contains(n.Parens().Span(), offset) {
				return protoScopeRFC001Param
			}
		case ast.DeclAnnotation:
			if protoast.Contains(n.Parens().Span(), offset) {
				return protoScopeRFC001Param
			}
		case *ast.File:
			return protoScopeFile
		}
	}
	return protoScopeUnknown
}

func protoCompletionList(items []protocol.CompletionItem) *protocol.CompletionList {
	if len(items) == 0 {
		return nil
	}
	return &protocol.CompletionList{IsIncomplete: false, Items: items}
}

func protoKeywordItem(label, detail string) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:      label,
		Kind:       protocol.CompletionItemKindKeyword,
		Detail:     detail,
		InsertText: label,
	}
}

// protoFileScopeKeywords are the legal top-level declarations a
// .proto file can carry. `edition` is the proto-editions analogue
// of `syntax`; both are valid as the first non-comment line. We
// don't try to enforce "syntax must come first" — that's a parse
// error the user gets from protocompile, not completion's job.
func protoFileScopeKeywords() []protocol.CompletionItem {
	return []protocol.CompletionItem{
		protoKeywordItem("syntax", "syntax declaration"),
		protoKeywordItem("edition", "edition declaration (proto editions)"),
		protoKeywordItem("package", "package declaration"),
		protoKeywordItem("import", "import statement"),
		protoKeywordItem("option", "file-level option"),
		protoKeywordItem("message", "message declaration"),
		protoKeywordItem("enum", "enum declaration"),
		protoKeywordItem("service", "service declaration"),
		protoKeywordItem("extend", "extension declaration"),
		protoKeywordItem("type", "type alias declaration (RFC-001)"),
		protoKeywordItem("function", "function declaration (RFC-001)"),
		protoKeywordItem("annotation", "annotation declaration (RFC-001)"),
	}
}

// protoMessageScopeKeywords are the legal openings of a declaration
// inside a message body. `includeNested` filters out keywords that
// don't apply inside a oneof — `oneof`, `reserved`, `extensions`,
// the cardinality modifiers — even though the rest of the keyword
// set is shared. (Oneofs can't nest a oneof, can't carry reserved
// ranges, and every field inside a oneof is implicitly singular
// presence-tracked: no `optional`/`repeated`.)
func protoMessageScopeKeywords(includeNested bool) []protocol.CompletionItem {
	items := []protocol.CompletionItem{
		protoKeywordItem("message", "nested message"),
		protoKeywordItem("enum", "nested enum"),
		protoKeywordItem("option", "field/message option"),
		protoKeywordItem("map", "map<KeyType, ValueType>"),
	}
	if includeNested {
		items = append(items,
			protoKeywordItem("oneof", "oneof"),
			protoKeywordItem("reserved", "reserved tags / names"),
			protoKeywordItem("extensions", "extension range"),
			protoKeywordItem("optional", "field modifier"),
			protoKeywordItem("repeated", "field modifier"),
		)
	}
	return items
}

func protoEnumScopeKeywords() []protocol.CompletionItem {
	return []protocol.CompletionItem{
		protoKeywordItem("option", "enum option"),
		protoKeywordItem("reserved", "reserved tags / names"),
	}
}

func protoServiceScopeKeywords() []protocol.CompletionItem {
	return []protocol.CompletionItem{
		protoKeywordItem("rpc", "rpc method"),
		protoKeywordItem("option", "service option"),
		protoKeywordItem("stream", "streaming-direction modifier"),
	}
}

// protoScalarTypes is the protobuf built-in scalar set. Order is
// stable so completion lists are deterministic across requests
// (helpful for snapshot tests). Matches isProtoBuiltinType's
// vocabulary.
var protoScalarTypes = []string{
	"bool", "bytes", "string",
	"double", "float",
	"int32", "int64",
	"uint32", "uint64",
	"sint32", "sint64",
	"fixed32", "fixed64",
	"sfixed32", "sfixed64",
}

func protoBuiltinTypeItems() []protocol.CompletionItem {
	items := make([]protocol.CompletionItem, 0, len(protoScalarTypes))
	for _, t := range protoScalarTypes {
		items = append(items, protocol.CompletionItem{
			Label:      t,
			Kind:       protocol.CompletionItemKindClass,
			Detail:     "scalar type",
			InsertText: t,
		})
	}
	return items
}

// protoDefBareName returns a definition's declared name when it is a
// single bare identifier, or "" otherwise (broken parse, or an
// unusual multi-component name). Mirrors the old AST's `m.Name ==
// nil` guard + `.Val` read at every completion emit site.
func protoDefBareName(def ast.DeclDef) string {
	return def.Name().AsIdent().Text()
}

// protoInFileTypeItems emits a CompletionItem for every message and
// enum declared in the current file, plus every RFC-001 `type` alias
// (top-level only — aliases can't nest). No package prefix — the
// cursor is already in the file's package scope and protobuf permits
// intra-package references unqualified. Nested types carry the
// shortest scope path that resolves from the cursor: bare
// `Inner` when the cursor sits inside its enclosing message,
// `Outer.Inner` elsewhere — always a name the strict scope resolver
// binds to exactly this declaration.
//
// cursorChain is the enclosing-message portion of the cursor's
// lexical scope (protoCursorChain); "" degrades every nested label
// to its full rooted path, which resolves from anywhere.
func protoInFileTypeItems(parsed *protoast.File, cursorChain string) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	walkScopedTypeDefs(parsed, func(scoped string, def ast.DeclDef) bool {
		items = append(items, protoScopedTypeItem(def, protoMinimalScopedLabel(parsed, scoped, cursorChain), "this file"))
		return true
	})
	for _, name := range protoTypeAliasNames(parsed) {
		items = append(items, protoTypeAliasItem(name, "type alias (this file)"))
	}
	return items
}

// protoCursorChain returns the enclosing-message portion of the
// cursor's lexical scope — refScope (protoRefScopeAt) minus the
// file's package prefix.
func protoCursorChain(refScope, pkg string) string {
	if pkg == "" {
		return refScope
	}
	if refScope == pkg {
		return ""
	}
	return strings.TrimPrefix(refScope, pkg+".")
}

// protoMinimalScopedLabel returns the shortest dotted suffix of
// `scoped` that, resolved from the cursor's enclosing-message chain
// under the resolver's innermost-first candidate order, binds
// exactly the declaration at `scoped`. A suffix that an
// inner scope shadows is skipped — emitting it would insert a
// reference that binds a different declaration — so the shadowed
// item keeps a longer (or full) path.
func protoMinimalScopedLabel(f *protoast.File, scoped, cursorChain string) string {
	segs := strings.Split(scoped, ".")
	for i := len(segs) - 1; i > 0; i-- {
		suffix := strings.Join(segs[i:], ".")
		if protoResolveInFileChain(f, suffix, cursorChain) == scoped {
			return suffix
		}
	}
	return scoped
}

// protoResolveInFileChain resolves `name` against the file's rooted
// declarations through the cursor's enclosing-message scopes,
// innermost first — the in-file half of resolveProtoTypeInFile's
// candidate order — and returns the scoped path of the winning
// declaration, or "" when nothing binds.
func protoResolveInFileChain(f *protoast.File, name, cursorChain string) string {
	for scope := cursorChain; ; {
		cand := name
		if scope != "" {
			cand = scope + "." + name
		}
		if def := findDeclByScopedPath(f, cand); !def.IsZero() {
			return cand
		}
		if scope == "" {
			return ""
		}
		if dot := strings.LastIndex(scope, "."); dot >= 0 {
			scope = scope[:dot]
		} else {
			scope = ""
		}
	}
}

// protoImportedTypeItems emits CompletionItems for messages, enums,
// and RFC-001 `type` aliases declared in files this file imports.
// Qualified with the imported file's package plus any enclosing-
// message segments (e.g. `acme.demo.v1.Outer.Inner`) because
// cross-package references require the full path — the earlier
// `pkg.leaf` shape for a nested type is a name the resolver
// (correctly) rejects.
//
// Iteration is bounded by the import list, not by the workspace
// index. Walking the whole index would offer types the current file
// can't reach without first adding an `import` statement; better to
// keep the suggestion set honest.
func (s *Workspace) protoImportedTypeItems(currentFile *protoast.File) []protocol.CompletionItem {
	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return nil
	}
	var items []protocol.CompletionItem
	for _, importPath := range protoImports(currentFile) {
		imported := idx.Lookup(importPath)
		if imported == nil {
			continue
		}
		pkg := filePackage(imported)
		walkScopedTypeDefs(imported, func(scoped string, def ast.DeclDef) bool {
			items = append(items, protoScopedTypeItem(def, qualifyProtoName(pkg, scoped), importPath))
			return true
		})
		for _, name := range protoTypeAliasNames(imported) {
			items = append(items, protoTypeAliasItem(qualifyProtoName(pkg, name), "type alias ("+importPath+")"))
		}
	}
	return items
}

// walkScopedTypeDefs visits every message- and enum-classified def in
// the file with its scope path rooted at the top level ("Outer.Inner")
// — the label shape the strict scope resolver binds from any scope.
// proto2 group bodies contribute path segments (a group declares a
// message named after it) but groups themselves aren't emitted. The
// callback returns false to stop the walk.
func walkScopedTypeDefs(f *protoast.File, visit func(scoped string, def ast.DeclDef) bool) {
	if f == nil || f.AST == nil {
		return
	}
	var walk func(prefix string, decls seq.Inserter[ast.DeclAny]) bool
	walk = func(prefix string, decls seq.Inserter[ast.DeclAny]) bool {
		for decl := range seq.Values(decls) {
			def := decl.AsDef()
			if def.IsZero() {
				continue
			}
			kind := def.Classify()
			if kind == ast.DefKindExtend {
				// Extend blocks aren't scopes: types declared
				// inside (proto2 groups, and anything nested in them)
				// belong to the enclosing scope — walk through without
				// a path segment.
				if body := def.Body(); !body.IsZero() {
					if !walk(prefix, body.Decls()) {
						return false
					}
				}
				continue
			}
			if kind != ast.DefKindMessage && kind != ast.DefKindEnum && kind != ast.DefKindGroup {
				continue
			}
			name := protoDefBareName(def)
			if name == "" {
				continue
			}
			scoped := name
			if prefix != "" {
				scoped = prefix + "." + name
			}
			if kind != ast.DefKindGroup && !visit(scoped, def) {
				return false
			}
			if kind != ast.DefKindEnum {
				if body := def.Body(); !body.IsZero() {
					if !walk(scoped, body.Decls()) {
						return false
					}
				}
			}
		}
		return true
	}
	walk("", f.AST.Decls())
}

// protoScopedTypeItem builds the CompletionItem for a message or
// enum under its scoped label. provenance is the human-readable
// origin ("this file" or the import path).
func protoScopedTypeItem(def ast.DeclDef, label, provenance string) protocol.CompletionItem {
	kind, what := protocol.CompletionItemKindClass, "message"
	if def.Classify() == ast.DefKindEnum {
		kind, what = protocol.CompletionItemKindEnum, "enum"
	}
	return protocol.CompletionItem{
		Label:      label,
		Kind:       kind,
		Detail:     what + " (" + provenance + ")",
		InsertText: label,
	}
}

// protoTypeAliasItem builds the CompletionItem for an RFC-001 `type`
// alias name.
func protoTypeAliasItem(label, detail string) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label: label,
		// No dedicated "alias" kind exists in LSP; Interface reads
		// closest — a named type standing in for another — and keeps
		// aliases visually distinct from messages (Class) and enums.
		Kind:       protocol.CompletionItemKindInterface,
		Detail:     detail,
		InsertText: label,
	}
}

func qualifyProtoName(pkg, leaf string) string {
	if pkg == "" {
		return leaf
	}
	return pkg + "." + leaf
}

// ─── Dot completion (phase 4b) ───────────────────────────────────

// protoDotCompletionPrefix detects whether the cursor sits in a
// `<qualifier>.<partial>` position and returns the qualifier when
// so. A qualifier is one or more dotted identifiers ending at the
// dot immediately before the in-progress (or empty) leaf the user
// is typing.
//
// Examples (cursor at <c>):
//
//	Outer.<c>          → ("Outer", true)
//	Outer.In<c>        → ("Outer", true)      // leaf "In" filters client-side
//	Outer.Inner.<c>    → ("Outer.Inner", true)
//	acme.demo.v1.<c>   → ("acme.demo.v1", true)
//	Outer<c>           → ("", false)          // no preceding dot
//	"  Outer.<c>"      → ("Outer", true)      // leading whitespace ignored
//
// Cursor positions inside string literals or comments aren't
// excluded here — protolsp's pxf completion handler takes the same
// permissive line-scan approach (see fieldAtCursorRHS) and the
// false-positive cost is low (a request returning irrelevant
// completions, suppressed by the client's prefix filter).
func protoDotCompletionPrefix(source []byte, pos protocol.Position, enc positions.Encoding) (string, bool) {
	off, err := positions.Offset(source, pos, enc)
	if err != nil || off < 0 || off > len(source) {
		return "", false
	}
	// Walk back through any in-progress leaf identifier (the part
	// the user is currently typing). The qualifier ends one byte
	// before whatever's at this boundary.
	leafStart := off
	for leafStart > 0 && isProtoIdentByte(source[leafStart-1]) {
		leafStart--
	}
	// Dot completion only fires when the leaf is preceded by a `.`.
	// Plain identifier completion (no dot) is handled by the scope
	// fallback.
	if leafStart == 0 || source[leafStart-1] != '.' {
		return "", false
	}
	qualEnd := leafStart - 1
	// Walk back across one or more `.identifier` segments.
	qualStart := qualEnd
	for qualStart > 0 {
		idStart := qualStart
		for idStart > 0 && isProtoIdentByte(source[idStart-1]) {
			idStart--
		}
		if idStart == qualStart {
			// No identifier here — the chain ended at the previous
			// iteration. Stop without consuming whatever non-ident
			// byte sits at qualStart-1.
			break
		}
		qualStart = idStart
		if qualStart > 0 && source[qualStart-1] == '.' {
			qualStart--
			continue
		}
		break
	}
	if qualStart == qualEnd {
		return "", false
	}
	return string(source[qualStart:qualEnd]), true
}

func isProtoIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

// protoQualifiedCompletions resolves `qualifier` the way the type
// resolver would resolve a reference written at the cursor —
// interpreting it through the reference site's enclosing scopes
// (refScope, protoRefScopeAt) against the current file and each
// import — then returns the child entities of whatever it pointed
// at:
//
//   - `Outer` (or `Middle` written inside Outer) → nested
//     messages + enums of the resolved message
//   - `acme.demo.v1` (a package, by any scope interpretation) →
//     top-level types of every reachable file with that package
//   - `acme.demo.v1.Outer` → nested decls of Outer in the imported
//     file with that package
//
// Returns nil when the qualifier doesn't resolve — that signals the
// caller to fall back to scope-based completion. Distinct from
// returning an empty slice, which would suppress completion entirely.
func (s *Workspace) protoQualifiedCompletions(currentFile *protoast.File, qualifier, refScope string) []protocol.CompletionItem {
	if qualifier == "" {
		return nil
	}
	// A message-valued qualifier resolves in-file first — mirrors
	// resolveProtoTypeRef's priority.
	for _, cand := range protoScopedCandidates(qualifier, refScope, filePackage(currentFile)) {
		def := findDeclByScopedPath(currentFile, cand)
		if def.IsZero() || def.Classify() == ast.DefKindEnum {
			continue
		}
		if items := messageChildTypeItems(def, "nested in "+cand); items != nil {
			return items
		}
	}
	items := s.crossFileQualifiedCompletionItems(currentFile, qualifier, refScope)
	// A qualifier naming the current file's own package contributes
	// its top-level types too — packages span files, so these merge
	// with whatever same-package imports surfaced above.
	if protoScopeQualifiesPackage(qualifier, refScope, filePackage(currentFile)) {
		items = append(fileTopLevelTypeItems(currentFile, "this file"), items...)
	}
	// Package-segment continuations: a qualifier that is a
	// strict prefix of a reachable package (under any enclosing-scope
	// interpretation) offers the next segment, so the user can walk
	// `acme.` → demo → v1 → types. Merges with the above — a package
	// can hold types AND deeper sub-packages at once.
	items = append(items, s.protoPackageSegmentItems(currentFile, qualifier, refScope)...)
	if len(items) == 0 {
		return nil
	}
	return items
}

// protoPackageSegmentItems emits one Module-kind item per distinct
// next-segment continuation of `qualifier` toward a reachable
// package — the current file's own and each import's. The
// import-bounded posture matches the type emitters: a package the
// file can't reach without a new import isn't suggested.
func (s *Workspace) protoPackageSegmentItems(currentFile *protoast.File, qualifier, refScope string) []protocol.CompletionItem {
	var items []protocol.CompletionItem
	seen := make(map[string]bool)
	add := func(pkg string) {
		seg, ok := protoPackageContinuation(qualifier, refScope, pkg)
		if !ok || seen[seg] {
			return
		}
		seen[seg] = true
		items = append(items, protocol.CompletionItem{
			Label:      seg,
			Kind:       protocol.CompletionItemKindModule,
			Detail:     "package",
			InsertText: seg,
		})
	}
	add(filePackage(currentFile))
	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return items
	}
	for _, importPath := range protoImports(currentFile) {
		if imported := idx.Lookup(importPath); imported != nil {
			add(filePackage(imported))
		}
	}
	return items
}

// protoPackageContinuation returns the package segment that follows
// `qualifier` on the way to targetPkg, when some enclosing-scope
// interpretation of the qualifier (same peeling as
// protoScopeQualifiesPackage, innermost first) is a strict
// segment-aligned prefix of it. ok=false when no interpretation
// leads into targetPkg.
func protoPackageContinuation(qualifier, refScope, targetPkg string) (string, bool) {
	if qualifier == "" || targetPkg == "" {
		return "", false
	}
	for scope := refScope; ; {
		fqn := qualifier
		if scope != "" {
			fqn = scope + "." + qualifier
		}
		if strings.HasPrefix(targetPkg, fqn+".") {
			rest := targetPkg[len(fqn)+1:]
			if dot := strings.Index(rest, "."); dot >= 0 {
				rest = rest[:dot]
			}
			return rest, true
		}
		if scope == "" {
			return "", false
		}
		if dot := strings.LastIndex(scope, "."); dot >= 0 {
			scope = scope[:dot]
		} else {
			scope = ""
		}
	}
}

// crossFileQualifiedCompletionItems handles qualifiers that resolve
// into an imported file: a package name (under any enclosing-scope
// interpretation — `demo.v1.` written in package acme names
// acme.demo.v1) emits the file's top-level types; a qualifier
// that resolves to a message through the scope chain emits its
// nested types.
//
// Iteration is bounded by the import list — same posture as
// protoImportedTypeItems. A type that's reachable in the workspace
// but not imported by the current file doesn't surface, because
// suggesting it would have the user type something that fails to
// compile until they add an `import` statement.
func (s *Workspace) crossFileQualifiedCompletionItems(currentFile *protoast.File, qualifier, refScope string) []protocol.CompletionItem {
	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return nil
	}
	var items []protocol.CompletionItem
	for _, importPath := range protoImports(currentFile) {
		imported := idx.Lookup(importPath)
		if imported == nil {
			continue
		}
		pkg := filePackage(imported)
		if protoScopeQualifiesPackage(qualifier, refScope, pkg) {
			items = append(items, fileTopLevelTypeItems(imported, importPath)...)
			continue
		}
		for _, cand := range protoScopedCandidates(qualifier, refScope, pkg) {
			def := findDeclByScopedPath(imported, cand)
			if def.IsZero() || def.Classify() == ast.DefKindEnum {
				continue
			}
			if children := messageChildTypeItems(def, "nested in "+importPath); children != nil {
				items = append(items, children...)
				break
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	return items
}

// protoScopeQualifiesPackage reports whether some enclosing-scope
// interpretation of `qualifier` (per the refScope chain — same
// peeling as protoScopedCandidates) names targetPkg exactly. The
// resolver skips these interpretations (a package isn't a type);
// completion wants them (the package's types are the completions).
func protoScopeQualifiesPackage(qualifier, refScope, targetPkg string) bool {
	if qualifier == "" || targetPkg == "" {
		return false
	}
	for scope := refScope; ; {
		fqn := qualifier
		if scope != "" {
			fqn = scope + "." + qualifier
		}
		if fqn == targetPkg {
			return true
		}
		if scope == "" {
			return false
		}
		if dot := strings.LastIndex(scope, "."); dot >= 0 {
			scope = scope[:dot]
		} else {
			scope = ""
		}
	}
}

// messageChildTypeItems emits CompletionItems for the nested
// messages + enums directly inside `msg`. Caller supplies the
// detail-suffix string so the user sees where the type lives
// (in-file vs imported file path).
func messageChildTypeItems(msg ast.DeclDef, detail string) []protocol.CompletionItem {
	body := msg.Body()
	if body.IsZero() {
		return nil
	}
	var items []protocol.CompletionItem
	for decl := range seq.Values(body.Decls()) {
		def := decl.AsDef()
		name := protoDefBareName(def)
		if name == "" {
			continue
		}
		switch def.Classify() {
		case ast.DefKindMessage:
			items = append(items, protocol.CompletionItem{
				Label:      name,
				Kind:       protocol.CompletionItemKindClass,
				Detail:     "message (" + detail + ")",
				InsertText: name,
			})
		case ast.DefKindEnum:
			items = append(items, protocol.CompletionItem{
				Label:      name,
				Kind:       protocol.CompletionItemKindEnum,
				Detail:     "enum (" + detail + ")",
				InsertText: name,
			})
		}
	}
	return items
}

// fileTopLevelTypeItems emits CompletionItems for the top-level
// messages, enums, and RFC-001 `type` aliases of `file`. Used when
// the qualifier resolves to a package (no nested suffix). Labels are
// bare names — the qualifier was already typed by the user, so a
// fully-qualified label would duplicate it.
func fileTopLevelTypeItems(file *protoast.File, importPath string) []protocol.CompletionItem {
	if file == nil || file.AST == nil {
		return nil
	}
	var items []protocol.CompletionItem
	for decl := range seq.Values(file.AST.Decls()) {
		def := decl.AsDef()
		name := protoDefBareName(def)
		if name == "" {
			continue
		}
		switch def.Classify() {
		case ast.DefKindMessage:
			items = append(items, protocol.CompletionItem{
				Label:      name,
				Kind:       protocol.CompletionItemKindClass,
				Detail:     "message (" + importPath + ")",
				InsertText: name,
			})
		case ast.DefKindEnum:
			items = append(items, protocol.CompletionItem{
				Label:      name,
				Kind:       protocol.CompletionItemKindEnum,
				Detail:     "enum (" + importPath + ")",
				InsertText: name,
			})
		}
	}
	for _, name := range protoTypeAliasNames(file) {
		items = append(items, protoTypeAliasItem(name, "type alias ("+importPath+")"))
	}
	return items
}
