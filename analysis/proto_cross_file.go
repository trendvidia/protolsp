// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Cross-file symbol resolution for .proto documents. Phase 3 step 3
// of the proto-authoring plan: a type reference that doesn't match
// any declaration in the current file falls through to walking the
// files this file imports. The walk consults the workspace index
// (protoindex.Index) so cross-file lookups are O(imports * decls),
// not O(workspace files * decls).
//
// Scope is intentionally restrictive: we only walk files that the
// CURRENT file imports. Arbitrary cross-workspace name matching
// would resolve to whatever file happens to share a type name —
// fine for hover but wrong semantically (proto's resolution rules
// require an explicit import). Sticking to imported files matches
// what protocompile does internally and gives users the same answers
// across edit-time + compile-time.

package analysis

import (
	"net/url"
	"path/filepath"
	"strings"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/source"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// protoTypeLookup is the resolved declaration for a type-reference
// resolve attempt. Def is a message- or enum-classified DeclDef when
// File is set — unless the name resolved to an RFC-001 `type` alias,
// in which case Alias is set instead (aliases aren't DeclDefs). All
// zero when the name didn't resolve anywhere.
//
// File is the parsed file that defines the symbol (may be the
// current file for in-file matches, or an imported file for
// cross-file matches). nodeLSPRange + protoFileURI consume the pair
// to build either a hover card or an LSP Location.
type protoTypeLookup struct {
	File  *protoast.File
	Def   ast.DeclDef
	Alias ast.DeclType
}

// IsZero reports whether the lookup found nothing. Receivers
// guarantee non-zero results have File set, so an IsZero check is
// equivalent to "no declaration at any priority."
func (r protoTypeLookup) IsZero() bool {
	return r.File == nil
}

// IsEnum reports whether the resolved declaration is an enum (as
// opposed to a message). Only meaningful on non-zero lookups.
func (r protoTypeLookup) IsEnum() bool {
	return r.Def.Classify() == ast.DefKindEnum
}

// IsAlias reports whether the resolved declaration is an RFC-001
// `type` alias. Only meaningful on non-zero lookups.
func (r protoTypeLookup) IsAlias() bool {
	return !r.Alias.IsZero()
}

// Node returns the declaration's Name — the go-to-definition target
// — or nil for a zero lookup.
func (r protoTypeLookup) Node() source.Spanner {
	if !r.Alias.IsZero() {
		return r.Alias.Name()
	}
	if r.Def.IsZero() {
		return nil
	}
	return r.Def.Name()
}

// resolveProtoTypeRef finds the declaration for `name` reachable
// from `currentFile`. Priority:
//
//  1. In-file: always tried regardless of qualifier — proto allows
//     intra-package references with or without the package prefix.
//  2. Imports of the current file, in source order. The workspace
//     index resolves each import path to a parsed file; the resolver
//     then walks that file for the reference's scoped candidates.
//
// Qualifier handling: a name is matched per file by
// interpreting it against the reference site's enclosing scopes
// (refScope — package plus enclosing messages, innermost first),
// peeling the target file's package prefix, and resolving the
// remaining segments as enclosing-message scopes —
// protoScopedCandidates has the full scoping rules. Bare names keep
// a lenient any-depth, any-file fallback after the scoped attempts.
//
// refScope is the cursor's lexical scope from protoScopeAt; "" means
// "unknown" and falls back to the file's package (the earlier
// posture).
//
// Returns the zero protoTypeLookup when no declaration matches.
// Built-in scalars (string, int32, …) are NOT handled here — caller
// short-circuits on isProtoBuiltinType before invoking.
func (s *Workspace) resolveProtoTypeRef(currentFile *protoast.File, name, refScope string) protoTypeLookup {
	if currentFile == nil || name == "" {
		return protoTypeLookup{}
	}
	if refScope == "" {
		refScope = filePackage(currentFile)
	}

	// In-file pass first — proto users reference same-file types with
	// or without the package prefix, and either form wins over imports.
	if r := resolveProtoTypeInFile(currentFile, name, refScope); !r.IsZero() {
		return r
	}

	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return protoTypeLookup{}
	}
	for _, imp := range protoImports(currentFile) {
		importedFile := idx.Lookup(imp)
		if importedFile == nil {
			continue
		}
		if r := resolveProtoTypeInFile(importedFile, name, refScope); !r.IsZero() {
			return r
		}
	}
	return protoTypeLookup{}
}

// resolveProtoTypeInFile finds the declaration `name` denotes inside
// f when referenced from a site whose lexical scope is refScope. The
// scoped candidates carry the qualifier discipline: `pkg.Foo` doesn't
// resolve against an unrelated file that happens to declare `Foo`,
// while `pkg.Outer.Inner` peels the enclosing-message segments after
// the package prefix and a bare name prefers the declaration
// nearest the reference's enclosing scope.
func resolveProtoTypeInFile(f *protoast.File, name, refScope string) protoTypeLookup {
	return findTypeByScopedCandidates(f, protoScopedCandidates(name, refScope, filePackage(f)))
}

// findTypeByScopedCandidates tries each scoped candidate against f's
// declarations, first match wins. Every candidate resolves as a
// nested-scope path rooted at the file's top level (protoc's
// rule; the earlier any-depth walk resolved references protoc
// rejects and could land on the wrong declaration when names collide
// across scopes). Single-segment candidates additionally try RFC-001
// `type` aliases, which are top-level-only, after message → enum —
// the established priority.
func findTypeByScopedCandidates(f *protoast.File, candidates []string) protoTypeLookup {
	for _, scoped := range candidates {
		if def := findDeclByScopedPath(f, scoped); !def.IsZero() {
			return protoTypeLookup{File: f, Def: def}
		}
		if !strings.Contains(scoped, ".") {
			if alias := findTypeAliasByName(f, scoped); !alias.IsZero() {
				return protoTypeLookup{File: f, Alias: alias}
			}
		}
	}
	return protoTypeLookup{}
}

// protoScopedCandidates returns the scope paths — dot-separated,
// relative to a target file's top level — under which `name`, written
// at a reference site whose lexical scope is refScope, could denote a
// declaration in a file whose package is targetPkg. Empty when
// the reference can't land in targetPkg at all.
//
// refScope is the full dotted scope at the reference site: the
// referencing file's package followed by the enclosing message names
// (protoScopeAt) — or just the package when the caller has no
// cursor context.
//
// Candidates follow protobuf's C++-style scope peeling: the reference
// is interpreted against each enclosing scope of refScope, innermost
// first (refScope, its parents segment by segment, then the root),
// and every interpretation that lands inside targetPkg is returned
// with the package prefix peeled — so "pkg.Outer.Inner" against
// package "pkg" yields "Outer.Inner", and a bare "Inner" written
// inside pkg.Outer.Middle yields "Outer.Middle.Inner", "Outer.Inner",
// "Inner", in that order. That chain is exhaustive: a
// reference protoc can't resolve produces no candidates, so a bare
// name no longer leaks across packages the way the earlier lenient
// fallback allowed.
func protoScopedCandidates(name, refScope, targetPkg string) []string {
	name = strings.TrimPrefix(name, ".")
	if name == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	add := func(scoped string) {
		if scoped != "" && !seen[scoped] {
			seen[scoped] = true
			out = append(out, scoped)
		}
	}
	for scope := refScope; ; {
		fqn := name
		if scope != "" {
			fqn = scope + "." + name
		}
		switch {
		case targetPkg == "":
			add(fqn)
		case fqn == targetPkg:
			// The interpretation names the package itself, not a type.
		case strings.HasPrefix(fqn, targetPkg+"."):
			add(fqn[len(targetPkg)+1:])
		}
		if scope == "" {
			break
		}
		if dot := strings.LastIndex(scope, "."); dot >= 0 {
			scope = scope[:dot]
		} else {
			scope = ""
		}
	}
	return out
}

// protoRefScopeAt returns the reference site's full lexical scope —
// the file's package followed by the names of the message (or proto2
// group, whose body is the declared message's scope)
// declarations enclosing the cursor, outermost first. With no
// enclosing messages this is just the package, so earlier call
// shapes are the degenerate case. (Not to be confused with
// protoScopeAt, completion's block-kind classifier.)
func protoRefScopeAt(f *protoast.File, path *protoPath) string {
	scope := filePackage(f)
	if path == nil {
		return scope
	}
	for _, n := range path.Nodes {
		def, ok := n.(ast.DeclDef)
		if !ok || (def.Classify() != ast.DefKindMessage && def.Classify() != ast.DefKindGroup) {
			continue
		}
		name := def.Name().Canonicalized()
		if name == "" {
			continue
		}
		if scope == "" {
			scope = name
		} else {
			scope += "." + name
		}
	}
	return scope
}

// protoTopLevelNameCandidates returns the names under which `name`,
// referenced from a site whose lexical scope is refScope, could
// denote a TOP-LEVEL declaration in a file whose package is
// targetPkg. It is the scoped-candidate chain filtered to
// undotted entries: annotations, functions, and type aliases can't
// nest, so only interpretations landing directly in the package
// participate — a bare name binds only in files whose package is an
// enclosing scope of the reference site, exactly like the type
// resolver's scoping.
func protoTopLevelNameCandidates(name, refScope, targetPkg string) []string {
	var out []string
	for _, cand := range protoScopedCandidates(name, refScope, targetPkg) {
		if !strings.Contains(cand, ".") {
			out = append(out, cand)
		}
	}
	return out
}

// findDeclByScopedPath resolves a dotted scope path rooted at the
// file's top level: every segment but the last names an enclosing
// message (or proto2 group — a group declares a message named after
// it), the last names a message, group, or enum nested
// directly inside it. Returns the zero DeclDef when any segment
// fails to match — including an intermediate segment that names a
// non-message.
func findDeclByScopedPath(f *protoast.File, scoped string) ast.DeclDef {
	if f == nil || f.AST == nil || scoped == "" {
		return ast.DeclDef{}
	}
	segs := strings.Split(scoped, ".")
	decls := f.AST.Decls()
	for i, seg := range segs {
		def := protoImmediateTypeDef(decls, seg)
		if def.IsZero() {
			return ast.DeclDef{}
		}
		if i == len(segs)-1 {
			return def
		}
		if def.Classify() == ast.DefKindEnum {
			return ast.DeclDef{}
		}
		body := def.Body()
		if body.IsZero() {
			return ast.DeclDef{}
		}
		decls = body.Decls()
	}
	return ast.DeclDef{}
}

// protoImmediateTypeDef finds the message-, group-, or enum-
// classified DeclDef named `name` among a scope's immediate
// declarations — one level of scope, no recursion into nested
// scopes. Fields, oneofs, and other def kinds don't participate:
// they can't be referenced as types. Extend blocks are NOT scopes
// : a proto2 group declared inside one defines a message in
// the enclosing scope, so extend bodies are scanned transparently.
func protoImmediateTypeDef(decls seq.Inserter[ast.DeclAny], name string) ast.DeclDef {
	for decl := range seq.Values(decls) {
		def := decl.AsDef()
		if def.IsZero() {
			continue
		}
		switch def.Classify() {
		case ast.DefKindMessage, ast.DefKindEnum, ast.DefKindGroup:
			if def.Name().Canonicalized() == name {
				return def
			}
		case ast.DefKindExtend:
			if body := def.Body(); !body.IsZero() {
				if found := protoImmediateTypeDef(body.Decls(), name); !found.IsZero() {
					return found
				}
			}
		}
	}
	return ast.DeclDef{}
}

// filePackage returns the package name declared in the file, or ""
// when the file has no `package` directive (legal in proto2 / proto3
// but rare — files default to the implicit empty package).
func filePackage(f *protoast.File) string {
	if f == nil || f.AST == nil {
		return ""
	}
	pkg := f.AST.Package()
	if pkg.IsZero() {
		return ""
	}
	return pkg.Path().Canonicalized()
}

// protoImports returns every import path declared at the top of
// the file. Order is preserved so the cross-file walk respects the
// source's declaration order (which doesn't matter semantically in
// proto, but keeps logs / test output stable).
func protoImports(f *protoast.File) []string {
	if f == nil || f.AST == nil {
		return nil
	}
	var out []string
	for imp := range f.AST.Imports() {
		path := imp.ImportPath().AsLiteral().AsString().Text()
		if path != "" {
			out = append(out, path)
		}
	}
	return out
}

// protoFileURI builds the file:// LSP URI for a parsed file whose
// Name() carries an absolute on-disk path. Used by the cross-file
// definition handler to point Locations at the right document.
// Registry shadow parses carry relative registry paths and are
// deliberately refused — their Locations compose registry:// URIs
// separately.
func protoFileURI(f *protoast.File) uri.URI {
	if f == nil {
		return ""
	}
	abs := f.Name()
	if abs == "" {
		return ""
	}
	// Ensure forward-slash path even on Windows.
	abs = filepath.ToSlash(abs)
	if !filepath.IsAbs(filepath.FromSlash(abs)) {
		// Shouldn't happen for indexed files — protoindex hands
		// abs paths to the parser — but guard anyway. Returning
		// the raw string would yield a relative URI the client
		// can't resolve.
		return ""
	}
	u := &url.URL{Scheme: "file", Path: abs}
	return uri.URI(u.String())
}

// protoLocation builds an LSP Location for a node inside a parsed
// file. Combines protoFileURI + nodeLSPRange — the two-step
// composition exists so callers can pick either piece in isolation
// when the other doesn't apply (e.g., a definition that points at a
// file header rather than a node).
func protoLocation(f *protoast.File, n source.Spanner, enc positions.Encoding) *protocol.Location {
	if f == nil || n == nil {
		return nil
	}
	u := protoFileURI(f)
	if u == "" {
		return nil
	}
	return &protocol.Location{
		URI:   uri.URI(u),
		Range: nodeLSPRange(f, n, enc),
	}
}

// protoLocationAtFileStart is the analogue of protoLocation for
// import-directive go-to-def: the user Cmd+clicks an `import
// "foo.proto"` line and the editor jumps to the start of foo.proto.
// We point at (line 0, col 0) rather than the file's first
// declaration — a header file with only comments would otherwise
// jump weirdly past them.
func protoLocationAtFileStart(f *protoast.File) *protocol.Location {
	if f == nil {
		return nil
	}
	u := protoFileURI(f)
	if u == "" {
		return nil
	}
	return &protocol.Location{
		URI: uri.URI(u),
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 0, Character: 0},
		},
	}
}
