// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Cross-file symbol resolution for read-only registry:// .proto
// previews (goed#119). The on-disk cross-file resolver
// (proto_cross_file.go) walks the workspace's protoindex, which holds
// files on disk only. A registry preview's imports point at OTHER
// registry files that were never written to disk — so on-disk
// resolution misses and cross-file go-to-definition falls through.
//
// This path supplements it: for a registry preview, resolve the
// imports through the SAME schema lookup the preview came from
// (lookupForNamespaceInWorkspace — the preview's sub-workspace
// registry, or the root when it has none), fetching each
// imported file's source
// (Lookup.GetSource) and parsing it for the declaration's position.
// Targets are emitted as registry:// Locations, so a cross-file jump
// lands in another read-only preview rather than an editable disk file
// — consistent with the "browsing the schema" model.
//
// Ranges come from parsing the served source (nodeLSPRange), not from
// descriptor SourceCodeInfo: a live registry's published descriptors
// commonly omit source info, so parsing is the only reliable way to
// point at the exact declaration line for every origin.

package analysis

import (
	"context"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/source"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// resolveRegistryProtoTypeLookup resolves a (possibly qualified) type
// name referenced from a registry preview to the declaration in one of
// its imported registry files. Returns the zero lookup (and "") when
// nothing matches. The second return is the import path the match came
// from — the definition path needs it to build the registry:// URI;
// the hover path ignores it and renders from the returned AST node.
//
// Mirrors the on-disk cross-file walk in resolveProtoTypeRef, but
// sources imported files from the preview's schema lookup instead of
// the disk index. workspaceDir scopes that lookup to the preview's
// sub-workspace; "" selects the root. In-file resolution is the
// caller's job (they handle it and stamp the request URI); this only
// walks imports.
//
// refScope is the reference site's lexical scope (protoRefScopeAt,
// #239); "" falls back to the file's package.
func (s *Workspace) resolveRegistryProtoTypeLookup(ctx context.Context, namespace, workspaceDir string, currentFile *protoast.File, name, refScope string) (protoTypeLookup, string) {
	if currentFile == nil || name == "" {
		return protoTypeLookup{}, ""
	}
	if refScope == "" {
		refScope = filePackage(currentFile)
	}
	for _, imp := range protoImports(currentFile) {
		importedFile := s.registryImportAST(ctx, namespace, workspaceDir, imp)
		if importedFile == nil {
			continue
		}
		// Scoped candidates carry the same qualifier discipline as the
		// on-disk resolver — `pkg.Foo` doesn't resolve against an
		// unrelated file that happens to declare `Foo`, and
		// `pkg.Outer.Inner` peels the enclosing-message segments after
		// the package prefix.
		if r := resolveProtoTypeInFile(importedFile, name, refScope); !r.IsZero() {
			return r, imp
		}
	}
	return protoTypeLookup{}, ""
}

// resolveRegistryProtoTypeRef is the go-to-definition wrapper around
// resolveRegistryProtoTypeLookup: it turns the resolved declaration
// into a registry:// Location, or nil when nothing matches.
func (s *Workspace) resolveRegistryProtoTypeRef(ctx context.Context, namespace, workspaceDir string, currentFile *protoast.File, name, refScope string) *protocol.Location {
	result, imp := s.resolveRegistryProtoTypeLookup(ctx, namespace, workspaceDir, currentFile, name, refScope)
	if result.IsZero() {
		return nil
	}
	return registryProtoLocation(namespace, imp, result.File, result.Node(), s.positionEncoding)
}

// resolveRegistryProtoImport resolves an `import "path";` directive in
// a registry preview to the imported registry file, at its start.
// Returns nil when the import doesn't resolve through the lookup (typo,
// missing dep), matching the on-disk resolveProtoImport's contract.
func (s *Workspace) resolveRegistryProtoImport(ctx context.Context, namespace, workspaceDir string, imp ast.DeclImport) *protocol.Location {
	path := imp.ImportPath().AsLiteral().AsString().Text()
	if path == "" {
		return nil
	}
	// Confirm the import actually resolves before offering a jump —
	// a fetch/parse miss means there's nothing to navigate to.
	if s.registryImportAST(ctx, namespace, workspaceDir, path) == nil {
		return nil
	}
	return &protocol.Location{
		URI: protocol.DocumentURI(BuildRegistryURI(namespace, path)),
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 0, Character: 0},
		},
	}
}

// registryProtoLocation builds a registry:// Location for a node in an
// imported registry file. Unlike protoLocation it can't derive the URI
// from the parsed file (protoFileURI is file://-only and rejects the
// relative registry path), so it composes the URI from the namespace +
// import path directly.
func registryProtoLocation(namespace, importPath string, fileNode *protoast.File, n source.Spanner, enc positions.Encoding) *protocol.Location {
	if fileNode == nil || n == nil {
		return nil
	}
	return &protocol.Location{
		URI:   protocol.DocumentURI(BuildRegistryURI(namespace, importPath)),
		Range: nodeLSPRange(fileNode, n, enc),
	}
}

// registryImportAST fetches and parses an imported registry .proto
// file, memoized per lookup epoch. On a cache hit (including a cached
// miss — a present nil) it returns immediately; otherwise it resolves
// the namespace to a schema lookup, fetches the source, parses it, and
// records the result (nil on any failure, so a known-absent import
// isn't refetched until the next lookup swap clears the cache).
func (s *Workspace) registryImportAST(ctx context.Context, namespace, workspaceDir, importPath string) *protoast.File {
	// workspaceDir participates in the key: two sub-workspaces can
	// share a namespace but bind different registries, so a
	// parse cached under one must not satisfy a fetch scoped to the
	// other. Root-scoped requests (workspaceDir=="") get their own
	// slot, distinct from any sub-workspace's.
	key := workspaceDir + "\x00" + namespace + "\x00" + importPath

	s.registryASTMu.RLock()
	cached, ok := s.registryAST[key]
	s.registryASTMu.RUnlock()
	if ok {
		return cached
	}

	fileNode := s.fetchAndParseRegistryProto(ctx, namespace, workspaceDir, importPath)

	s.registryASTMu.Lock()
	if s.registryAST == nil {
		s.registryAST = make(map[string]*protoast.File)
	}
	s.registryAST[key] = fileNode
	s.registryASTMu.Unlock()
	return fileNode
}

// fetchAndParseRegistryProto is registryImportAST's cache-miss body:
// namespace → lookup → GetSource → parse. Returns nil on any failure
// (no lookup, fetch error, unparseable source); callers treat nil as
// "import doesn't resolve."
func (s *Workspace) fetchAndParseRegistryProto(ctx context.Context, namespace, workspaceDir, importPath string) *protoast.File {
	lk := s.lookupForNamespaceInWorkspace(namespace, workspaceDir)
	if lk == nil {
		return nil
	}
	src, err := lk.GetSource(ctx, importPath)
	if err != nil || len(src) == 0 {
		return nil
	}
	return parseProtoSilently(importPath, src)
}

// clearRegistryASTCache drops every memoized registry AST. Called from
// the LookupManager onChanged seam: a lookup swap can change what an
// import path resolves to, so cached parses from the old lookup must
// not leak into resolution against the new one.
func (s *Workspace) clearRegistryASTCache() {
	s.registryASTMu.Lock()
	s.registryAST = nil
	s.registryASTMu.Unlock()
}
