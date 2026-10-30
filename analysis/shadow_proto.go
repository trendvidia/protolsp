// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Shadow documents for read-only registry:// .proto previews. The
// schema resolver serves the source of a registry-only .proto file
// through SourceContent; an embedder (goed#26) shows those bytes in a
// read-only buffer keyed by the file's registry:// URI. To make that
// buffer navigable — hover on a field type, jump to the message it
// names in the same file — the buffer needs a tracked Document with a
// parsed AST, exactly like an on-disk .proto file has.
//
// It can't go through the normal analyze pipeline: that pipeline runs
// protocompile (which resolves imports and emits diagnostics) keyed
// off a filesystem path, and registry:// URIs have no filesystem path
// — URI.FsPath() panics on the scheme. So this is a parse-only
// "shadow" path: it populates doc.protoAST and nothing else, publishes
// no diagnostics, and never touches the disk. HoverFor / DefinitionFor
// route registry:// .proto URIs here (see the IsRegistryProtoURI
// branches in hover.go / definition.go).

package analysis

import (
	"strings"

	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/protoast"
)

// IsRegistryProtoURI reports whether the URI is a registry:// preview
// of a .proto source — the read-only, lookup-backed buffers an
// embedder opens for schema browsing and go-to-definition landings.
//
// These are deliberately NOT IsProtoURI: that predicate gates the
// diagnostics-publishing analyze pipeline, which requires a file on
// disk (protocompile + URI.FsPath()). Registry previews have no
// disk path and never validate. Hover and go-to-definition, which
// only need the parsed AST, do route on this predicate so a preview
// is navigable (goed#26).
func IsRegistryProtoURI(u uri.URI) bool {
	s := string(u)
	return strings.HasPrefix(s, registryURIScheme) && strings.HasSuffix(s, ".proto")
}

// OpenShadowProto tracks a read-only registry:// .proto source as a
// Document and parses it into an AST so hover + go-to-definition
// resolve inside the read-only buffer. It is the "shadow document"
// support registry previews need: the navigation handlers read
// doc.protoAST, which the normal pipeline only builds for on-disk
// .proto files.
//
// No diagnostics are published and the analyze pipeline is bypassed
// entirely. The buffer is an immutable snapshot the resolver produced
// (its bytes change only when the schema lookup rebuilds, at which
// point the embedder re-opens it), and its URI is not a
// client-editable file. Parsing happens directly, with a filename
// derived from the registry path — never URI.FsPath(), which panics
// on the registry:// scheme.
//
// Idempotent: re-opening the same URI refreshes the source + AST in
// place. Non-registry / non-.proto URIs are a no-op, so embedders can
// route indiscriminately.
//
// This opens the preview against the ROOT lookup's scope. Multi-root
// embedders use OpenShadowProtoForWorkspace to bind the preview to the
// sub-workspace it was opened from, so its cross-file navigation
// resolves against that sub-workspace's registry.
func (s *Workspace) OpenShadowProto(docURI uri.URI, source string) {
	s.OpenShadowProtoForWorkspace(docURI, source, "")
}

// OpenShadowProtoForWorkspace is OpenShadowProto with an explicit
// sub-workspace binding: workspaceDir is the absolute directory of the
// sub-workspace the preview was opened from. It records that on the
// shadow Document (SetWorkspaceDir) so hover + go-to-definition route
// the preview's cross-file registry resolution to the sub-workspace's
// own registry rather than the root's. An empty workspaceDir
// leaves the doc root-scoped — the OpenShadowProto behavior.
func (s *Workspace) OpenShadowProtoForWorkspace(docURI uri.URI, source, workspaceDir string) {
	if !IsRegistryProtoURI(docURI) {
		return
	}
	doc := s.docs.Get(docURI)
	if doc == nil {
		doc = &Document{URI: docURI}
		s.docs.Open(doc)
	}
	if workspaceDir != "" {
		doc.SetWorkspaceDir(workspaceDir)
	}
	doc.mu.Lock()
	defer doc.mu.Unlock()
	doc.Version++
	doc.Source = []byte(source)

	// protocompile's filename is the registry path component
	// (e.g. "acme/billing/v1/invoice.proto"), never URI.FsPath().
	// Fall back to the raw URI if the shape is unexpected — the
	// parser only uses the filename for error positions, which this
	// path discards.
	filename := string(docURI)
	if _, filePath, ok := ParseRegistryURI(docURI); ok {
		filename = filePath
	}

	doc.protoAST = parseProtoSilently(filename, doc.Source)
}

// parseProtoSilently parses .proto source into an AST, swallowing all
// parse errors (the caller wants the AST for hover / go-to-definition,
// not diagnostics — those come from the analyze pipeline for on-disk
// files, and never for read-only registry sources). A partial AST is
// fine: navigation degrades to "no info" on the parts that didn't
// parse. filename feeds parser error positions, which this path
// discards, so a registry file path (not an absolute disk path) is
// acceptable.
func parseProtoSilently(filename string, source []byte) *protoast.File {
	return protoast.Parse(filename, source)
}
