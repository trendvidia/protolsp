// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Keyed-entry rename (draft-trendvidia-protowire-01 §3.13). A keyed
// repeated field's entry is identified by its collection's dotted path
// plus its key — e.g. children[loginButton] — so renaming it propagates
// across the workspace exactly like the path-based field rename in
// rename.go: every occurrence of the same entry (chain layers that
// define or override it) is rewritten in one edit. Each rewrite is
// keyed-aware: the entry name token is quoted or unquoted to match
// `pxf fmt` canonicalization, and any redundant explicit key assignment
// inside the entry is retargeted atomically.
package analysis

import (
	"context"
	"os"
	"strconv"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// RenameTargetKind classifies what a rename request at a position would
// affect, so the dispatch layer can validate the new name against the
// right grammar: an ordinary field/block path needs a PXF identifier; a
// keyed entry accepts any non-empty single-line key (quoted when it
// isn't identifier-safe).
type RenameTargetKind int

const (
	// RenameTargetNone: the cursor isn't on anything renameable.
	RenameTargetNone RenameTargetKind = iota
	// RenameTargetField: an ordinary field or block whose dotted path
	// the path-based rename rewrites across the workspace.
	RenameTargetField
	// RenameTargetKeyedEntry: a keyed repeated field's entry name.
	RenameTargetKeyedEntry
)

// keyedEntryRef locates one keyed entry: the entry node, its
// collection's key field name (for retargeting a redundant explicit key
// assignment), and the dotted path of the keyed field from the document
// root (the entry's identity across files).
type keyedEntryRef struct {
	entry        keyedEntry
	keyFieldName string
	fieldPath    string
}

// keyedRenameSpec identifies a keyed entry workspace-wide: the dotted
// path of its keyed field, the entry key, and the element's key field
// name. newName is the target key.
type keyedRenameSpec struct {
	fieldPath    string
	key          string
	keyFieldName string
	newName      string
}

// RenameTargetKind reports what a rename at (docURI, pos) would target.
// Used by the dispatch layer to choose new-name validation.
func (s *Workspace) RenameTargetKind(docURI protocol.DocumentURI, pos protocol.Position) RenameTargetKind {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return RenameTargetNone
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	if doc.Parsed == nil {
		return RenameTargetNone
	}
	if _, ok := s.keyedEntryAtLocked(doc, pos); ok {
		return RenameTargetKeyedEntry
	}
	if _, _, _, ok := pathAtPosition(doc.Source, s.positionEncoding, doc.Parsed.Entries, pos); ok {
		return RenameTargetField
	}
	return RenameTargetNone
}

// keyedEntryAtLocked resolves the document's bound root descriptor and
// locates the keyed entry (if any) whose name the cursor is on. Caller
// holds doc.mu.
func (s *Workspace) keyedEntryAtLocked(doc *Document, pos protocol.Position) (keyedEntryRef, bool) {
	typeURL := s.effectiveTypeURL(doc)
	if typeURL == "" {
		return keyedEntryRef{}, false
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(typeURL))
	if err != nil || mt == nil {
		return keyedEntryRef{}, false
	}
	return keyedEntryAt(doc.Source, s.positionEncoding, doc.Parsed.Entries, mt.Descriptor(), pos)
}

// keyedEntryAt walks entries against md and returns the keyed entry the
// cursor is on. A cursor inside the entry's name token matches exactly;
// otherwise the first keyed entry on the cursor's line matches (so a
// cursor at line start still resolves, matching the line-granular
// behavior of the path-based rename).
func keyedEntryAt(
	source []byte,
	enc positions.Encoding,
	entries []pxf.Entry,
	md protoreflect.MessageDescriptor,
	cursor protocol.Position,
) (keyedEntryRef, bool) {
	var refs []keyedEntryRef
	collectKeyedEntries(entries, md, "", &refs)
	if len(refs) == 0 {
		return keyedEntryRef{}, false
	}
	cursorOff, err := positions.Offset(source, cursor, enc)
	if err != nil {
		return keyedEntryRef{}, false
	}
	cursorLine := cursor.Line + 1 // pxf lines are 1-indexed
	var (
		lineBest    keyedEntryRef
		lineBestSet bool
	)
	for i := range refs {
		r := refs[i]
		end := nameTokenEnd(source, r.entry.pos.Offset, r.entry.quoted)
		if end < 0 {
			continue
		}
		if cursorOff >= r.entry.pos.Offset && cursorOff <= end {
			return r, true // exact token containment
		}
		if !lineBestSet && uint32(r.entry.pos.Line) == cursorLine { // #nosec G115 -- parser lines non-negative
			lineBest = r
			lineBestSet = true
		}
	}
	if lineBestSet {
		return lineBest, true
	}
	return keyedEntryRef{}, false
}

// collectKeyedEntries gathers every keyed entry reachable from entries
// (bound to md) with its keyed field's dotted path (prefix + field
// name), descending through ordinary message blocks and through keyed
// entries' bodies so nested keyed collections (recursive UI trees) are
// covered. The path convention — every block name a segment — matches
// the schema-less collector so an entry resolved here in one file finds
// its twins in others.
func collectKeyedEntries(entries []pxf.Entry, md protoreflect.MessageDescriptor, prefix string, out *[]keyedEntryRef) {
	if md == nil {
		return
	}
	for _, e := range entries {
		b, ok := e.(*pxf.Block)
		if !ok {
			continue
		}
		path := joinPath(prefix, b.Name)
		fd := md.Fields().ByName(protoreflect.Name(b.Name))
		if keyFd := pxf.KeyField(fd); keyFd != nil {
			elem := fd.Message()
			keyName := string(keyFd.Name())
			for _, ce := range b.Entries {
				ke, ok := asKeyedEntry(ce)
				if !ok {
					continue
				}
				*out = append(*out, keyedEntryRef{entry: ke, keyFieldName: keyName, fieldPath: path})
				collectKeyedEntries(ke.body, elem, joinPath(path, ke.key), out)
			}
			continue
		}
		if fd != nil && fd.Message() != nil {
			collectKeyedEntries(b.Entries, fd.Message(), path, out)
		}
	}
}

// collectKeyedRenameEditsAcrossWorkspace gathers the keyed-entry rename
// edits matching spec across every .pxf file under scopeDir plus the
// open-doc set — the same corpus, precedence, sub-workspace boundaries,
// and truncation the path-based rename walks (see references.go), so a
// keyed rename and a field rename cover the same files. Matching is
// schema-less and structural (by dotted path + key), so a file that
// can't be read or parsed is simply skipped.
//
// scopeDir == "" (a document with no workspace context) falls back to
// the open documents that also have no workspace, so the originating
// document is still covered.
func (s *Workspace) collectKeyedRenameEditsAcrossWorkspace(ctx context.Context, scopeDir string, spec keyedRenameSpec) map[protocol.DocumentURI][]protocol.TextEdit {
	out := map[protocol.DocumentURI][]protocol.TextEdit{}
	add := func(u uri.URI, source []byte, entries []pxf.Entry) {
		edits := keyedRenameEditsInFile(source, s.positionEncoding, entries, spec)
		if len(edits) > 0 {
			du := protocol.DocumentURI(u)
			out[du] = append(out[du], edits...)
		}
	}
	// tryOpenDoc applies the open-doc buffer (so unsaved edits win) and
	// reports whether the URI was an open, parsed document.
	tryOpenDoc := func(u uri.URI) bool {
		doc := s.docs.Get(u)
		if doc == nil {
			return false
		}
		doc.mu.RLock()
		defer doc.mu.RUnlock()
		if doc.Parsed == nil {
			return false
		}
		add(doc.URI, doc.Source, doc.Parsed.Entries)
		return true
	}

	if scopeDir == "" {
		for _, u := range s.docs.uris() {
			doc := s.docs.Get(u)
			if doc == nil {
				continue
			}
			doc.mu.RLock()
			if doc.WorkspaceDir() == "" && doc.Parsed != nil {
				add(doc.URI, doc.Source, doc.Parsed.Entries)
			}
			doc.mu.RUnlock()
		}
		return out
	}

	candidates, truncated := s.walkReferencesCandidates(scopeDir)
	visited := make(map[uri.URI]struct{}, len(candidates))
	for _, c := range candidates {
		visited[c.uri] = struct{}{}
		if tryOpenDoc(c.uri) {
			continue // open buffer wins over the on-disk copy
		}
		content, err := os.ReadFile(c.path) // #nosec G304 -- candidate paths are workspace-rooted
		if err != nil {
			continue
		}
		parsed, err := pxf.Parse(content)
		if err != nil {
			continue
		}
		add(c.uri, content, parsed.Entries)
	}
	// Open docs whose path isn't on disk yet (new, unsaved tabs).
	for _, u := range s.docs.uris() {
		if _, seen := visited[u]; seen {
			continue
		}
		doc := s.docs.Get(u)
		if doc == nil {
			continue
		}
		doc.mu.RLock()
		if doc.WorkspaceDir() == scopeDir && doc.Parsed != nil {
			add(doc.URI, doc.Source, doc.Parsed.Entries)
		}
		doc.mu.RUnlock()
	}
	if truncated {
		s.notifyReferencesTruncated(ctx, scopeDir)
	}
	return out
}

// keyedRenameEditsInFile returns the rename edits for every keyed entry
// in one file's AST that matches spec (dotted path + key).
func keyedRenameEditsInFile(source []byte, enc positions.Encoding, entries []pxf.Entry, spec keyedRenameSpec) []protocol.TextEdit {
	var out []protocol.TextEdit
	collectKeyedRenameEdits(source, enc, entries, "", spec, &out)
	return out
}

// collectKeyedRenameEdits matches keyed entries structurally: at the
// block whose dotted path equals spec.fieldPath, every child entry whose
// key equals spec.key gets rename edits. Schema-less by design — the
// keyed field name and key field name were resolved once against the
// originating document's schema, and layers share that schema.
func collectKeyedRenameEdits(source []byte, enc positions.Encoding, entries []pxf.Entry, prefix string, spec keyedRenameSpec, out *[]protocol.TextEdit) {
	for _, e := range entries {
		b, ok := e.(*pxf.Block)
		if !ok {
			continue
		}
		path := joinPath(prefix, b.Name)
		if path == spec.fieldPath {
			for _, ce := range b.Entries {
				ke, ok := asKeyedEntry(ce)
				if !ok || ke.key != spec.key {
					continue
				}
				*out = append(*out, keyedEntryRenameEdits(source, enc, ke, spec.keyFieldName, spec.newName)...)
			}
		}
		collectKeyedRenameEdits(source, enc, b.Entries, path, spec, out)
	}
}

// keyedEntryRenameEdits produces the TextEdits that rename one keyed
// entry: the name token (quoted when newName isn't identifier-safe, per
// the canonical rule) plus any redundant `keyFieldName = "oldKey"`
// assignment inside the body, retargeted to the new key so it keeps
// agreeing with the entry name rather than becoming a conflict.
func keyedEntryRenameEdits(
	source []byte,
	enc positions.Encoding,
	entry keyedEntry,
	keyFieldName string,
	newName string,
) []protocol.TextEdit {
	end := nameTokenEnd(source, entry.pos.Offset, entry.quoted)
	if end < 0 {
		return nil
	}
	newSpelling := newName
	if !identSafeEntryName(newName) {
		newSpelling = strconv.Quote(newName)
	}
	edits := []protocol.TextEdit{{
		Range:   offsetSpanToRange(source, entry.pos.Offset, end, enc),
		NewText: newSpelling,
	}}
	for _, be := range entry.body {
		a, ok := be.(*pxf.Assignment)
		if !ok || a.KeyQuoted || a.Key != keyFieldName {
			continue
		}
		sv, ok := a.Value.(*pxf.StringVal)
		if !ok || sv.Value != entry.key {
			continue // absent or disagreeing — not redundant; leave it
		}
		edits = append(edits, protocol.TextEdit{
			Range:   offsetSpanToRange(source, sv.Pos.Offset, sv.End.Offset, enc),
			NewText: strconv.Quote(newName),
		})
	}
	return edits
}

// offsetSpanToRange converts a [startOff, endOff) byte span to an LSP
// range in the negotiated encoding.
func offsetSpanToRange(source []byte, startOff, endOff int, enc positions.Encoding) protocol.Range {
	sl, sc := positions.LineCol(source, startOff, enc)
	el, ec := positions.LineCol(source, endOff, enc)
	return protocol.Range{
		Start: protocol.Position{Line: sl, Character: sc},
		End:   protocol.Position{Line: el, Character: ec},
	}
}

// IsValidKeyedEntryKey reports whether name is acceptable as a keyed
// entry key. Any non-empty string that fits on one line qualifies: an
// identifier-unsafe key is simply quoted when written. Newlines and
// other control characters are rejected because they can't appear in a
// single-line entry name even quoted.
func IsValidKeyedEntryKey(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return false
		}
	}
	return true
}
