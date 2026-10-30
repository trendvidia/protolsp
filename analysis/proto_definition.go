// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Go-to-definition for .proto documents. Phase 2 of the proto-
// authoring plan: within-file only. A cursor on a type reference in
// a field declaration jumps to the matching message or enum
// declaration in the same file. Cross-file resolution (jumping to
// `import "foo.proto"` targets) is Phase 3.

package analysis

import (
	"context"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/source"
	"github.com/trendvidia/protocompile/token"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/protoast"
)

// protoDefinitionFor returns the Location a .proto cursor target
// resolves to, or nil for "no definition." Phase 3: cross-file
// type references and import directives both resolve through the
// workspace index, in addition to Phase 2's in-file resolution.
//
// Cursor targets handled:
//   - Field type identifier (the `Foo` in `Foo my_field = 1;`) →
//     jump to the matching message or enum declaration in this
//     file OR an imported file (cross-file via the workspace
//     index).
//   - Import directive (`import "foo/bar.proto";`) → jump to the
//     imported file at its start. New in Phase 3.
//   - Message / enum / field / enum-value name identifier → jump
//     to itself (degenerate but harmless: a Cmd+click on a
//     definition stays put rather than failing).
func (s *Workspace) protoDefinitionFor(docURI uri.URI, pos protocol.Position) *protocol.Location {
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

	// A registry:// preview resolves its cross-file targets against
	// the sub-workspace it was opened from; wsDir carries that
	// scope from the shadow Document (OpenShadowProtoForWorkspace) to
	// the registry resolvers. "" for the root workspace (and every
	// on-disk file, which doesn't reach the registry paths anyway).
	wsDir := doc.WorkspaceDir()

	// Import directive jump (Phase 3). The cursor sits inside the
	// quoted import string, whose leaf is the string literal token.
	// Check the ancestors for a DeclImport and resolve the import
	// path — through the namespace lookup for a registry:// preview
	// (goed#119), else the on-disk workspace index.
	if imp, ok := importAncestor(path); ok {
		if IsRegistryProtoURI(uri.URI(docURI)) {
			if ns, _, ok := ParseRegistryURI(uri.URI(docURI)); ok {
				return s.resolveRegistryProtoImport(context.Background(), ns, wsDir, imp)
			}
			return nil
		}
		return s.resolveProtoImport(imp)
	}

	// RFC-001 positions: annotation use
	// sites and function references jump to their declarations
	// (in-file or in an imported file); type slots resolve like field
	// types; declaration names jump to themselves. Checked before the
	// ident classification below because these shapes have no DeclDef
	// in the ancestry slots that path expects (and the `@` sigil's
	// leaf isn't an ident token at all).
	if loc := s.protoRFC001DefinitionFor(docURI, wsDir, doc.protoAST, path); loc != nil {
		return loc
	}

	// Function calls inside opaque engine-expression fragments:
	// the capture has no AST value nodes for the path walk above to
	// classify, but the compile pass resolved every call site — hit-
	// test the cursor against those instead.
	if loc := s.rfc001OpaqueCallDefinition(docURI, wsDir, doc, pos); loc != nil {
		return loc
	}

	// Idents inside declarations descend through the declaration's
	// name / type paths — mirror the hover handler's classification:
	// the parent is the ast.Path the ident is a component of, and
	// the enclosing DeclDef says which slot that path fills. Multi-
	// component paths (`acme.demo.v1.Foo`) resolve on the whole
	// path's canonical text so the qualifier participates.
	tok, ok := leaf.(token.Token)
	if !ok || tok.Kind() != token.Ident {
		return nil
	}
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
		// Cursor is on a declaration's own name — its definition is
		// the declaration itself. Return that location for message /
		// enum / field / enum-value declarations.
		switch def.Classify() {
		case ast.DefKindField, ast.DefKindMessage, ast.DefKindEnum, ast.DefKindEnumValue:
			return &protocol.Location{
				URI:   docURI,
				Range: nodeLSPRange(doc.protoAST, def.Name(), s.positionEncoding),
			}
		}
		return nil
	}

	// Not the name — for a field-classified def the remaining path
	// position is the type slot. Resolve via Phase 3's cross-file
	// lookup (which still tries in-file first).
	if def.Classify() == ast.DefKindField {
		return s.resolveProtoTypeDefinition(docURI, wsDir, doc.protoAST, refPath.Canonicalized(), protoRefScopeAt(doc.protoAST, path))
	}
	return nil
}

// resolveProtoTypeDefinition looks up `name` as a message or enum
// declaration reachable from the current file (in-file first, then
// imported files via the workspace index) and returns its Name
// token's location. Built-in scalars and unresolved names return
// nil — the LSP renders "no definition found" for both, which is
// honest when we don't have a target to jump to.
//
// docURI is the request URI. In-file resolutions stamp it directly
// rather than re-deriving the URI from the parsed file: for on-disk
// files the two are identical (both the absolute file:// URI), but a
// registry:// shadow document (goed#26) is parsed under a relative
// registry path that protoFileURI can't turn back into a URI — using
// docURI lands the jump on the read-only preview instead of nil.
// Cross-file results keep protoLocation (the imported file's own
// on-disk URI).
//
// workspaceDir scopes the registry:// cross-file resolution to the
// preview's sub-workspace registry; it is unused on the
// on-disk path.
//
// refScope is the reference site's lexical scope (protoRefScopeAt);
// "" falls back to the file's package inside the resolvers.
func (s *Workspace) resolveProtoTypeDefinition(docURI uri.URI, workspaceDir string, currentFile *protoast.File, name, refScope string) *protocol.Location {
	if isProtoBuiltinType(name) {
		return nil
	}
	// Mirrors the same trim in renderProtoFieldTypeHover.
	if len(name) > 0 && name[0] == '.' {
		name = name[1:]
	}
	result := s.resolveProtoTypeRef(currentFile, name, refScope)

	// In-file match stamps the request URI: for on-disk files it equals
	// protoFileURI(currentFile); for a registry:// preview (parsed under
	// a relative path protoFileURI can't turn into a URI) it lands the
	// jump on the preview itself.
	if !result.IsZero() && result.File == currentFile {
		return &protocol.Location{URI: docURI, Range: nodeLSPRange(currentFile, result.Node(), s.positionEncoding)}
	}

	// Registry:// preview (goed#119): cross-file targets are other
	// registry files, resolved through the preview's namespace lookup —
	// NOT the on-disk index, whose file:// hits would jump out of the
	// read-only browsing view. resolveProtoTypeRef's on-disk cross-file
	// result (if any) is deliberately discarded here.
	if IsRegistryProtoURI(uri.URI(docURI)) {
		if ns, _, ok := ParseRegistryURI(uri.URI(docURI)); ok {
			if loc := s.resolveRegistryProtoTypeRef(context.Background(), ns, workspaceDir, currentFile, name, refScope); loc != nil {
				return loc
			}
		}
		return s.rfc001RemoteLocation(workspaceDir, currentFile, rfc001KindType, name, refScope)
	}

	// On-disk cross-file match.
	if !result.IsZero() {
		return protoLocation(result.File, result.Node(), s.positionEncoding)
	}

	// Remote fallback: a message, enum, or
	// RFC-001 `type` alias in an import the index doesn't hold —
	// served source or the compiled descriptor via the schema lookup.
	return s.rfc001RemoteLocation(workspaceDir, currentFile, rfc001KindType, name, refScope)
}

// importAncestor returns the DeclImport containing the cursor's
// descent path, or ok=false when the cursor isn't inside an import
// directive. The leaf is typically the quoted import path's string
// literal, whose parent is the DeclImport.
func importAncestor(path *protoPath) (ast.DeclImport, bool) {
	n := path.Ancestor(func(n source.Spanner) bool {
		_, ok := n.(ast.DeclImport)
		return ok
	})
	if n == nil {
		return ast.DeclImport{}, false
	}
	imp, ok := n.(ast.DeclImport)
	return imp, ok
}

// resolveProtoImport jumps to the imported file at its start. The
// import string carries the workspace-relative path the index keys
// on; a hit returns the file's URI at (0, 0). A miss (import doesn't
// resolve through the index — typo, missing dep, naive-fallback
// gap) returns nil, surfacing as the editor's "no definition" UI.
func (s *Workspace) resolveProtoImport(imp ast.DeclImport) *protocol.Location {
	importPath := imp.ImportPath().AsLiteral().AsString().Text()
	if importPath == "" {
		return nil
	}
	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return nil
	}
	importedFile := idx.Lookup(importPath)
	if importedFile == nil {
		return nil
	}
	return protoLocationAtFileStart(importedFile)
}
