// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C5 of the chameleon-integration design: go-to-definition for
// ${field.path} references. Two resolution modes:
//
//   1. Chain-aware: when the document is a PER_ENV chain layer,
//      walk the env's resolve order in REVERSE (deepest first) and
//      return the location of the first layer file that sets the
//      path. "Last write wins" — matches chameleon's runtime merge,
//      so the jump lands on the layer whose value actually shows
//      up at resolve time.
//
//   2. Schema fallback: when no layer sets the path (or the
//      document isn't part of a chain), jump to the schema's
//      defining .proto location, same as today's
//      definitionForFieldAtCursor.
//
// Soft failures: encrypted ancestors are opaque (consistent with
// C3's required-field handling — we never read sops streams);
// missing layer files are skipped; non-PER_ENV layers (ONCE, etc.)
// fall through to schema fallback. References whose first segment
// is env / ctx are runtime context interpolation; they have no
// edit-time definition and the handler returns nil.

package analysis

import (
	"path/filepath"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/interp"
	"github.com/trendvidia/protolsp/safeio"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// fieldRefDefinitionMode reports whether the cursor sits on a
// ${...} reference token AND, when so, returns the resolved
// Location. The bool gates the caller's fall-through to the
// existing definition handlers: once we identify the cursor as
// being inside a ${...}, that branch OWNS the response. Returning
// `(nil, true)` means "we recognize the context but have no
// resolution" — which is the right answer for env/ctx context
// refs and for paths that don't exist on the schema. Falling
// through in those cases would surface the surrounding
// assignment's definition (`api_url` for cursor inside
// `api_url = "${env.HOST}"`), which doesn't match user intent.
func (s *Workspace) fieldRefDefinitionMode(doc *Document, pos protocol.Position) (loc *protocol.Location, handled bool) {
	if s == nil || doc == nil {
		return nil, false
	}
	path, ok := refTokenAtCursor(doc.Source, pos)
	if !ok {
		return nil, false
	}
	// env. / ctx. prefixes are runtime context interpolation — no
	// edit-time location to jump to. Match the gate used by the
	// C4 resolver so all three (diagnostics, completion,
	// definition) treat these the same.
	if first := firstSegment(path); first == "env" || first == "ctx" {
		return nil, true
	}
	if l := s.definitionFromChainLayers(doc, path); l != nil {
		return l, true
	}
	return s.definitionFromSchema(doc, path), true
}

// definitionFromChainLayers walks the document's env resolve
// order in reverse, returning the first layer that sets the path.
// Returns nil when:
//   - the document isn't a PER_ENV chain layer (ONCE layers and
//     standalone files take the schema-fallback path)
//   - no readable ancestor / leaf sets the path
//   - workspaceRoot isn't a file:// scheme (no disk to read from)
func (s *Workspace) definitionFromChainLayers(doc *Document, path string) *protocol.Location {
	if doc.LayerInfo == nil || !doc.LayerInfo.IsLayer {
		return nil
	}
	if doc.LayerInfo.Strategy != "PER_ENV" || doc.LayerInfo.Environment == "" {
		return nil
	}
	docChain := s.ChainForDoc(doc)
	if docChain == nil {
		return nil
	}
	rootPath := s.workspaceDirForDoc(doc)
	if rootPath == "" {
		return nil
	}
	order := docChain.ResolveOrder(doc.LayerInfo.Environment)
	segments := strings.Split(path, ".")
	// Reverse iteration: deepest layer wins. Last-write semantics
	// match chameleon's runtime merge.
	for i := len(order) - 1; i >= 0; i-- {
		layer := order[i]
		if layer.Encrypted {
			continue // sops streams are opaque to the LSP
		}
		absPath := layer.ResolvedPath
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(rootPath, filepath.FromSlash(layer.ResolvedPath))
		}
		layerURI := uri.File(absPath)
		// Prefer the open-document source over disk: an unsaved
		// edit that introduced the assignment should still drive
		// the jump.
		var content []byte
		if openDoc := s.docs.Get(layerURI); openDoc != nil {
			content = openDoc.Source
		} else {
			b, err := safeio.ReadFileLimit(absPath, safeio.MaxConfigFileBytes)
			if err != nil {
				continue
			}
			content = b
		}
		pos, found := findAssignmentByPath(content, segments)
		if !found {
			continue
		}
		return &protocol.Location{
			URI:   layerURI,
			Range: pxfPosToRange(pos),
		}
	}
	return nil
}

// definitionFromSchema returns the .proto descriptor location for
// the path's leaf field. Used as the fallback when chain lookup
// produces nothing — and as the only resolver for standalone
// (non-chain) documents.
func (s *Workspace) definitionFromSchema(doc *Document, path string) *protocol.Location {
	ast := doc.Parsed
	if ast == nil {
		ast = doc.LastGoodParse
	}
	if ast == nil || ast.TypeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(ast.TypeURL))
	if err != nil || mt == nil {
		return nil
	}
	fd := findLeafFieldDescriptor(path, mt.Descriptor())
	if fd == nil {
		return nil
	}
	return s.locationForDescriptor(doc, fd)
}

// refTokenAtCursor returns the full ${...} path content when the
// cursor sits anywhere within a complete reference token on its
// line, or "" otherwise. "Anywhere within" is inclusive of the
// `${` and `}` delimiters — placing the cursor on either end of
// the token still resolves to its path.
//
// Single-line scan only — multi-line references aren't part of
// this slice (consistent with the completion detector). Uses the
// same shared scanner (internal/interp) as the C4 recognition
// path so accepted shapes stay in sync.
func refTokenAtCursor(source []byte, pos protocol.Position) (string, bool) {
	lineStart := offsetForLineCol(source, int(pos.Line)+1, 1)
	if lineStart < 0 {
		return "", false
	}
	lineEnd := lineStart
	for lineEnd < len(source) && source[lineEnd] != '\n' {
		lineEnd++
	}
	line := string(source[lineStart:lineEnd])
	cursorRel := int(pos.Character)
	for _, r := range interp.FindAll(line) {
		if cursorRel >= r.Start && cursorRel <= r.End {
			return r.Path, true
		}
	}
	return "", false
}

// firstSegment returns the substring of path before the first `.`,
// or path itself when there's no dot. Used to gate context-ref
// detection (env.X / ctx.X) without splitting the whole path.
func firstSegment(path string) string {
	if dot := strings.IndexByte(path, '.'); dot >= 0 {
		return path[:dot]
	}
	return path
}

// findLeafFieldDescriptor walks the dotted path against the
// descriptor and returns the FieldDescriptor for the leaf segment,
// or nil when any segment doesn't resolve. Mirrors resolveFieldPath
// but returns the descriptor for downstream location lookup.
//
// Intermediate segments must be MessageKind; the leaf segment can
// be any kind. Returns nil for empty path / nil descriptor.
func findLeafFieldDescriptor(path string, desc protoreflect.MessageDescriptor) protoreflect.FieldDescriptor {
	if path == "" || desc == nil {
		return nil
	}
	segments := strings.Split(path, ".")
	var fd protoreflect.FieldDescriptor
	current := desc
	for i, seg := range segments {
		if current == nil {
			return nil
		}
		fd = current.Fields().ByName(protoreflect.Name(seg))
		if fd == nil {
			return nil
		}
		if i == len(segments)-1 {
			return fd
		}
		if fd.Kind() != protoreflect.MessageKind {
			return nil
		}
		current = fd.Message()
	}
	return fd
}

// findAssignmentByPath returns the source position of the
// Assignment / Block that sets `segments` in the given PXF
// content. Walks AST top-level → block-by-block, matching each
// segment in order. Returns (zero, false) when:
//   - the content doesn't parse
//   - no path matches (the layer doesn't set this field)
//
// Position semantics: the returned Pos is the start of the
// Assignment/Block. For a scalar `api_url = "x"` that's the `a`
// of `api_url`. For a Block `db { ... }` matched at its leaf
// (e.g. path "db" alone), that's the `d` of `db`. For a nested
// assignment `db { host = "..." }` matched at "db.host", that's
// the `h` of `host`.
func findAssignmentByPath(content []byte, segments []string) (pxf.Position, bool) {
	parsed, err := pxf.Parse(content)
	if err != nil || parsed == nil {
		return pxf.Position{}, false
	}
	return walkEntriesByPath(parsed.Entries, segments)
}

// walkEntriesByPath is findAssignmentByPath's recursive worker.
// Separated so callers with an already-parsed document don't
// re-parse; testable on its own.
func walkEntriesByPath(entries []pxf.Entry, segments []string) (pxf.Position, bool) {
	if len(segments) == 0 {
		return pxf.Position{}, false
	}
	target := segments[0]
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if n.Key == target && len(segments) == 1 {
				return n.Pos, true
			}
		case *pxf.Block:
			if n.Name != target {
				continue
			}
			if len(segments) == 1 {
				return n.Pos, true
			}
			if pos, ok := walkEntriesByPath(n.Entries, segments[1:]); ok {
				return pos, true
			}
		}
	}
	return pxf.Position{}, false
}
