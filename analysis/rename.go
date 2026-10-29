// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"

	"go.lsp.dev/protocol"
)

// RenameEditFor builds the WorkspaceEdit for a rename request, or nil
// when the cursor doesn't sit on a renameable target.
//
// Two kinds of rename are served, both propagating across the workspace:
//   - A keyed repeated field's entry (draft -01 §3.13), identified by
//     its collection path + key. Every occurrence of that same entry
//     (chain layers that define or override it) is rewritten, quoting or
//     unquoting the key to match `pxf fmt` and retargeting any redundant
//     explicit key assignment. Tried first.
//   - Otherwise the path-based field rename: every Assignment / Block
//     whose dotted path matches the cursor's is rewritten.
func (s *Workspace) RenameEditFor(ctx context.Context, docURI protocol.DocumentURI, pos protocol.Position, newName string) *protocol.WorkspaceEdit {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	if doc.Parsed == nil {
		doc.mu.RUnlock()
		return nil
	}
	// Resolve both candidate targets while holding the lock (schema
	// resolution + AST reads), then release before the cross-workspace
	// walk, which re-locks documents — including this one via the
	// open-doc overlay. Holding the lock across that walk would risk a
	// self-deadlock against a queued writer.
	keyedRef, keyedOK := s.keyedEntryAtLocked(doc, pos)
	cursorPath, _, _, pathOK := pathAtPosition(doc.Source, s.positionEncoding, doc.Parsed.Entries, pos)
	workspaceDir := doc.WorkspaceDir()
	doc.mu.RUnlock()

	if keyedOK {
		changes := s.collectKeyedRenameEditsAcrossWorkspace(ctx, workspaceDir, keyedRenameSpec{
			fieldPath:    keyedRef.fieldPath,
			key:          keyedRef.entry.key,
			keyFieldName: keyedRef.keyFieldName,
			newName:      newName,
		})
		if len(changes) == 0 {
			return nil
		}
		return &protocol.WorkspaceEdit{Changes: changes}
	}

	if !pathOK || cursorPath == "" {
		return nil
	}
	hits := s.collectPathHitsAcrossWorkspace(ctx, workspaceDir, cursorPath)
	if len(hits) == 0 {
		return nil
	}
	changes := make(map[protocol.DocumentURI][]protocol.TextEdit, 1)
	for _, h := range hits {
		changes[protocol.DocumentURI(h.uri)] = append(changes[protocol.DocumentURI(h.uri)], protocol.TextEdit{
			Range:   h.rang,
			NewText: newName,
		})
	}
	return &protocol.WorkspaceEdit{Changes: changes}
}
