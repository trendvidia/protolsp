// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/schema"
)

// DefinitionFor computes the Location the cursor's target resolves to,
// or returns nil for "no definition available."
func (s *Workspace) DefinitionFor(docURI uri.URI, pos protocol.Position) *protocol.Location {
	// Phase 2 of proto authoring: .proto documents have their own
	// AST-based definition resolution (within-file only for now).
	// Routing them through the dedicated handler keeps the pxf-
	// specific resolution path (field-ref → chain-aware jump,
	// @type → registry FQN lookup) from accidentally claiming a
	// proto position. Registry:// .proto previews (goed#26) also
	// route here: they're read-only shadow documents (see
	// OpenShadowProto) with a parsed AST but no disk path, so the
	// proto handler's in-file resolution applies while the analyze
	// pipeline stays file-only.
	if IsProtoURI(uri.URI(docURI)) || IsRegistryProtoURI(uri.URI(docURI)) {
		return s.protoDefinitionFor(docURI, pos)
	}
	// No s.lookup gate: findType consults internal descriptors
	// too, so go-to-def on a `.protolsp.pxf` @type works even
	// without a user registry. User-domain types return nil from
	// findType and the existing flow handles that gracefully.
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()

	// Target kinds (checked in order; first match wins):
	//   1. ${field.path} reference under the cursor → chain-aware
	//      jump (deepest layer setting the path), else schema
	//      fallback. C5 of the chameleon-integration design. If
	//      the cursor IS on a ref token, this branch owns the
	//      response — it doesn't fall through to the line-match
	//      handler, which would otherwise return the surrounding
	//      `api_url` assignment for cursor inside `${env.HOST}`.
	//   2. @type FQN on this line → jump to the message's definition.
	//   3. field assignment / block name → jump to the field's
	//      definition.
	if loc, handled := s.fieldRefDefinitionMode(doc, pos); handled {
		return loc
	}
	if isTypeDirectiveLine(doc.Source, pos.Line) {
		return s.definitionForTypeDirective(doc, doc.Source, pos)
	}
	return s.definitionForFieldAtCursor(doc, pos)
}

// definitionForTypeDirective parses the FQN out of the `@type` line
// at the cursor and resolves it against the schema. The FQN may be
// partially typed; we use whatever's there.
func (s *Workspace) definitionForTypeDirective(doc *Document, source []byte, pos protocol.Position) *protocol.Location {
	fqn := fqnFromTypeDirective(source, pos.Line)
	if fqn == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(fqn))
	if err != nil {
		return nil
	}
	return s.locationForDescriptor(doc, mt.Descriptor())
}

// definitionForFieldAtCursor uses the AST to figure out which field
// the cursor is on, then resolves to its FieldDescriptor's location.
// Falls back to LastGoodParse during mid-edit invalid states.
func (s *Workspace) definitionForFieldAtCursor(doc *Document, pos protocol.Position) *protocol.Location {
	ast := doc.Parsed
	if ast == nil {
		ast = doc.LastGoodParse
	}
	if ast == nil {
		return nil
	}
	// effectiveTypeURL applies chameleon.schema_fqn for layer files
	// without their own @type; without it, field-level go-to-def
	// silently returns nothing on every layer file in a chameleon-
	// managed workspace.
	typeURL := s.effectiveTypeURL(doc)
	if typeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(typeURL))
	if err != nil {
		return nil
	}
	target := findHoverTarget(ast.Entries, mt.Descriptor(), pos.Line)
	if target == nil || target.descriptor == nil {
		return nil
	}
	return s.locationForDescriptor(doc, target.descriptor)
}

// locationForDescriptor builds an LSP Location for any descriptor
// (field, message, enum, …) using its containing FileDescriptor's
// path and source-info-derived range. doc is the document the
// navigation started from — it routes the per-workspace lookup
// reads (a sub-workspace doc resolves against ITS proto_files /
// registry, not the root's).
//
// URI selection precedence:
//  1. Exact proto_files attribution: when the doc's proto_files
//     compile recorded the descriptor path's disk location, file://
//     to that path. Descriptor paths are search-path-relative, not
//     workspace-relative, so only this map — no probe — finds the
//     working copy under a configured search path.
//  2. file:// against the workspace root when that file exists on
//     disk — keeps the user editing their working copy when one is
//     present.
//  3. registry://<namespace>/<path> when the resolver attributes
//     the file to a tracked namespace. The editor's content
//     provider for `registry:` fetches bytes via the
//     protolsp/sourceContent custom request.
//  4. Blind file:// (pre-phase-2 behavior) when a workspace root is
//     set but none of the above applies — surfaces a real
//     misconfiguration as the client's "file not found" rather
//     than silently dropping the navigation.
//
// Returns nil when the descriptor's file path is empty
// (synthesized descriptors) or when none of the resolution paths
// produces a URI.
func (s *Workspace) locationForDescriptor(doc *Document, d protoreflect.Descriptor) *protocol.Location {
	if d == nil {
		return nil
	}
	file := d.ParentFile()
	if file == nil {
		return nil
	}
	target, ok := s.resolveDefinitionURI(doc, file.Path())
	if !ok {
		return nil
	}
	return &protocol.Location{
		URI:   target,
		Range: rangeFromSourceLocation(file.SourceLocations().ByDescriptor(d)),
	}
}

// resolveDefinitionURI picks the right URI for the given .proto
// path per the precedence documented on [locationForDescriptor].
func (s *Workspace) resolveDefinitionURI(doc *Document, protoPath string) (uri.URI, bool) {
	if protoPath == "" {
		return "", false
	}
	// Exact attribution: the proto_files compile scanned these
	// files from disk and kept each one's absolute path. Resolving
	// through it is precise where the workspace-root probe below is
	// not — descriptor paths are search-path-relative, so the probe
	// misses files under a configured `proto_files` path (and can
	// hit the WRONG file when the same relative path exists in two
	// places).
	if abs, ok := schema.ProtoFilesLocalPath(s.protoFilesLookupForDoc(doc), protoPath); ok {
		return uri.File(abs), true
	}
	// Prefer the local working copy when one exists.
	if s.ws.workspaceRoot != "" && s.localFileExists(protoPath) {
		return s.resolveFileURI(protoPath)
	}
	// Fall back to the registry-backed virtual document. Routed
	// per-doc so a sub-workspace's registry attribution (not the
	// root's) names the origin.
	if lk := s.LookupForDoc(doc); lk != nil {
		if _, origin, err := lk.FindFileByPathWithOrigin(protoPath); err == nil && origin != "" {
			return BuildRegistryURI(origin, protoPath), true
		}
	}
	// Internally-shipped types (`chameleon.v1.ChainSpec`,
	// `protolsp.v1.WorkspaceConfig`) live in protolsp's binary, not
	// on disk and not in the user's registry. Without this step,
	// go-to-definition on them falls through to blind file:// and
	// the editor finds nothing to open. The sourceContent handler
	// recognizes the `_internal_` namespace and serves the embedded
	// .proto bytes — see SchemaService.GetSourceContent.
	if s.internalLookup != nil {
		if _, origin, err := s.internalLookup.FindFileByPathWithOrigin(protoPath); err == nil && origin != "" {
			return BuildRegistryURI(origin, protoPath), true
		}
	}
	// Last resort: blind file:// (matches pre-phase-2 behavior).
	return s.resolveFileURI(protoPath)
}

// localFileExists reports whether protoPath resolves to an actual
// file under the workspace root. Cheap stat — go-to-definition is a
// human-paced action, the syscall is well under perceptual latency.
//
// Workspace root URIs are file:// in practice; non-file schemes
// (rare, e.g. remote workspaces) return false and the caller falls
// back to registry:// or blind file:// as appropriate.
func (s *Workspace) localFileExists(protoPath string) bool {
	if s.ws.workspaceRoot == "" {
		return false
	}
	rootURL, err := url.Parse(string(s.ws.workspaceRoot))
	if err != nil || rootURL.Scheme != "file" {
		return false
	}
	abs := filepath.Join(rootURL.Path, protoPath)
	_, err = os.Stat(abs)
	return err == nil
}

// resolveFileURI turns a relative `.proto` path from a FileDescriptor
// into a file:// URI. Resolved against the workspace root the client
// sent at initialize time. Returns (zero, false) when the path can't
// be turned into a sensible URI.
//
// Absolute paths (rare, but possible if the descriptor was loaded
// from a non-default file system) are returned as-is via uri.File.
func (s *Workspace) resolveFileURI(protoPath string) (uri.URI, bool) {
	if protoPath == "" {
		return "", false
	}
	if strings.HasPrefix(protoPath, "/") {
		return uri.File(protoPath), true
	}
	if s.ws.workspaceRoot == "" {
		// Without a workspace root we have nothing sensible to resolve
		// against. Returning the raw relative path would just produce
		// an "file not found" pop-up on most clients. Decline cleanly.
		return "", false
	}
	root := s.ws.workspaceRoot
	// Concatenate root + path. uri.URI is a string under the hood; a
	// trailing slash on either side is fine.
	sep := "/"
	rootStr := string(root)
	if strings.HasSuffix(rootStr, "/") {
		sep = ""
	}
	return uri.URI(rootStr + sep + protoPath), true
}

// rangeFromSourceLocation converts the line/column info SourceLocations
// returns (1-indexed, *inclusive* end) into an LSP Range (0-indexed,
// exclusive end). The Path field of SourceLocation isn't used — that's
// for cross-file navigation, not the per-element range.
//
// When the location is empty (no source-info data on the descriptor's
// file), defaults to (0, 0) so the editor at least opens the file.
func rangeFromSourceLocation(loc protoreflect.SourceLocation) protocol.Range {
	startLine := uint32(0)
	startCol := uint32(0)
	endLine := uint32(0)
	endCol := uint32(0)
	if loc.StartLine > 0 {
		startLine = uint32(loc.StartLine) // #nosec G115 -- proto-source line ranges fit in uint32 by far
	}
	if loc.StartColumn > 0 {
		startCol = uint32(loc.StartColumn) // #nosec G115
	}
	if loc.EndLine > 0 {
		endLine = uint32(loc.EndLine) // #nosec G115
	} else {
		endLine = startLine
	}
	if loc.EndColumn > 0 {
		endCol = uint32(loc.EndColumn) // #nosec G115
	} else {
		endCol = startCol + 1
	}
	return protocol.Range{
		Start: protocol.Position{Line: startLine, Character: startCol},
		End:   protocol.Position{Line: endLine, Character: endCol},
	}
}

// fqnFromTypeDirective extracts the FQN written after `@type` on the
// given source line. Returns "" when the line isn't a type directive
// or no FQN has been typed yet.
func fqnFromTypeDirective(source []byte, line uint32) string {
	start := offsetForLineCol(source, int(line)+1, 1)
	if start < 0 {
		return ""
	}
	end := start
	for end < len(source) && source[end] != '\n' {
		end++
	}
	text := string(source[start:end])
	text = strings.TrimSpace(text)
	const prefix = "@type"
	if !strings.HasPrefix(text, prefix) {
		return ""
	}
	rest := strings.TrimSpace(text[len(prefix):])
	// Stop at the first whitespace — only the first token is the FQN.
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
