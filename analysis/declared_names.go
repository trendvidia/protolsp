// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Workspace-level declared-name export. Editor hosts embed a
// line-based "middle path" highlighter (bellui x/editor's
// DeclaredNames) that colors @name annotation uses, bare type
// references, and bare function references only when the workspace
// actually declares them — keystroke-fresh where the semantic-token
// overlay is debounce-stale. The analyze pass already resolves
// declaredness (the RFC-001 declarations and the registry lookup);
// this file exports the resulting name sets so hosts don't re-derive
// them from token text, which would also bless unresolved names.

package analysis

import (
	"sort"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/protoast"
)

// DeclaredNames is the workspace's declared RFC-001 name sets:
// annotation, type, and function names some file in the workspace
// declares. Field shape mirrors the editor-side consumer
// (x/editor's DeclaredNames), so hosts convert field-for-field.
//
// Each declaration contributes its bare name and, when the declaring
// file has a package, the package-qualified form ("pkg.name") —
// dotted use sites like `@acme.demo.v1.audited` match against the
// qualified entry. The sets are workspace-level and advisory: a
// line-based highlighter can't check import edges or lexical scope,
// so a bare entry blesses use sites in every file, not only those
// whose package (or imports) would bind it.
//
// Slices are sorted and duplicate-free; empty sets are nil.
type DeclaredNames struct {
	// Annotations are declared annotation names, matched against
	// @name use sites (without the leading @).
	Annotations []string
	// Types are declared type names: RFC-001 `type` aliases plus
	// top-level messages, enums, and proto2 groups — the same type
	// namespace the reference resolvers search.
	Types []string
	// Functions are declared `function` names.
	Functions []string
}

// DeclaredNames returns the workspace's declared RFC-001 name sets,
// unioned from every source the analyze pass resolves against:
//
//   - workspace-local files in the proto index (the on-disk
//     declaration set — BSR cache deps are excluded here so the
//     accessor never mass-parses the module cache),
//   - open .proto documents' retained ASTs (keystroke-fresh: an
//     unsaved declaration counts immediately after its analyze
//     pass), and
//   - the open documents' imports — indexed imports by AST (this is
//     where a cache-resident dep the workspace actually references
//     enters), unindexed imports through the workspace's schema
//     lookup via the compiled RFC-001 carrier options, mirroring
//     resolveRFC001RemoteDecl's descriptor layer.
//
// Hosts refresh on the analyze-completion path, same cadence as the
// secret-range / dataset-region payloads; the result is a snapshot
// and stale between passes by design.
func (s *Workspace) DeclaredNames() DeclaredNames {
	if s == nil {
		return DeclaredNames{}
	}
	acc := newDeclaredNameAcc()
	idx := s.SnapshotProtoIndex()

	// folded tracks import paths already contributed, so a file
	// reachable both as a local index entry and as several documents'
	// import is walked once.
	folded := map[string]bool{}
	for _, p := range idx.LocalImportPaths() {
		folded[p] = true
		acc.addFile(idx.Lookup(p))
	}

	for _, u := range s.docs.uris() {
		doc := s.docs.Get(u)
		if doc == nil || !IsProtoURI(doc.URI) {
			continue
		}
		doc.mu.RLock()
		f := doc.protoAST
		doc.mu.RUnlock()
		// Safe past the RUnlock: analyze installs a freshly-parsed
		// AST on every pass and never mutates one after publication
		// (same contract snapshotParsed documents).
		if f == nil {
			continue
		}
		acc.addFile(f)
		for _, imp := range protoImports(f) {
			if folded[imp] {
				continue
			}
			folded[imp] = true
			if imported := idx.Lookup(imp); imported != nil {
				acc.addFile(imported)
				continue
			}
			acc.addDescriptor(s.remoteImportDescriptor(doc, imp))
		}
	}
	return acc.result()
}

// remoteImportDescriptor resolves an import the proto index does not
// hold through the document's workspace-scoped schema lookup. nil
// when no lookup is configured or the path is unknown there too —
// the import is then simply unresolved and contributes no names.
func (s *Workspace) remoteImportDescriptor(doc *Document, importPath string) protoreflect.FileDescriptor {
	lk := s.lookupForNamespaceInWorkspace("", s.workspaceDirForDoc(doc))
	if lk == nil {
		return nil
	}
	fd, _, err := lk.FindFileByPathWithOrigin(importPath)
	if err != nil {
		return nil
	}
	return fd
}

// declaredNameAcc accumulates the three name sets with bare +
// package-qualified entries, deduplicated.
type declaredNameAcc struct {
	annotations map[string]bool
	types       map[string]bool
	functions   map[string]bool
}

func newDeclaredNameAcc() *declaredNameAcc {
	return &declaredNameAcc{
		annotations: map[string]bool{},
		types:       map[string]bool{},
		functions:   map[string]bool{},
	}
}

// add records one declared name in both its bare and package-
// qualified forms. Empty names — a mid-edit declaration missing its
// name — are skipped.
func (a *declaredNameAcc) add(set map[string]bool, pkg, name string) {
	if name == "" {
		return
	}
	set[name] = true
	if pkg != "" {
		set[pkg+"."+name] = true
	}
}

// addFile folds one parsed file's top-level declarations into the
// sets. RFC-001 declarations are top-level only in well-formed
// files, and only top-level messages/enums/groups participate — a
// nested type's bare leaf doesn't resolve at top level, and
// its qualified use tokenizes segment-by-segment in a line-based
// highlighter anyway.
func (a *declaredNameAcc) addFile(f *protoast.File) {
	if f == nil || f.AST == nil {
		return
	}
	pkg := filePackage(f)
	for decl := range seq.Values(f.AST.Decls()) {
		switch decl.Kind() {
		case ast.DeclKindAnnotation:
			a.add(a.annotations, pkg, decl.AsAnnotation().Name().Text())
		case ast.DeclKindFunction:
			a.add(a.functions, pkg, decl.AsFunction().Name().Text())
		case ast.DeclKindType:
			a.add(a.types, pkg, decl.AsType().Name().Text())
		case ast.DeclKindDef:
			def := decl.AsDef()
			switch def.Classify() {
			case ast.DefKindMessage, ast.DefKindEnum, ast.DefKindGroup:
				a.add(a.types, pkg, def.Name().Canonicalized())
			}
		}
	}
}

// addDescriptor folds a compiled file's declarations into the sets:
// the RFC-001 carrier options (annotations, type aliases, functions)
// plus the descriptor's top-level messages and enums, matching
// addFile's type namespace. nil descriptors (unresolved imports)
// contribute nothing.
func (a *declaredNameAcc) addDescriptor(fd protoreflect.FileDescriptor) {
	if fd == nil {
		return
	}
	pkg := string(fd.Package())
	c := rfc001CarrierFromDescriptor(fd)
	for _, d := range c.Annotations {
		a.add(a.annotations, pkg, d.GetName())
	}
	for _, d := range c.TypeDecls {
		a.add(a.types, pkg, d.GetName())
	}
	for _, d := range c.Functions {
		a.add(a.functions, pkg, d.GetName())
	}
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		a.add(a.types, pkg, string(msgs.Get(i).Name()))
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		a.add(a.types, pkg, string(enums.Get(i).Name()))
	}
}

func (a *declaredNameAcc) result() DeclaredNames {
	return DeclaredNames{
		Annotations: sortedNames(a.annotations),
		Types:       sortedNames(a.types),
		Functions:   sortedNames(a.functions),
	}
}

func sortedNames(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
