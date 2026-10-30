// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"fmt"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// CodeActionsFor builds the list of quick-fix CodeActions that apply
// to the given diagnostics. The client passes the diagnostics it
// considers active at the cursor; we just need to recognize ours
// (by Data.kind) and produce the corresponding edits.
//
// The caller must hold doc.RLock: edits are computed against
// doc.Source, and the lock keeps that snapshot consistent with the
// diagnostics the client is acting on.
func (s *Workspace) CodeActionsFor(doc *Document, diags []protocol.Diagnostic) []protocol.CodeAction {
	var out []protocol.CodeAction
	for _, d := range diags {
		if data := DecodeUnknownFieldData(d.Data); data != nil {
			out = append(out, removeFieldAction(doc, d, *data))
			continue
		}
		if data := DecodeLiteralSecretData(d.Data); data != nil {
			out = append(out, replaceLiteralWithEnvRefAction(doc, d, *data))
			continue
		}
		if data := DecodeUnresolvedNameData(d.Data); data != nil {
			out = append(out, replaceUnresolvedNameActions(doc, d, *data)...)
			continue
		}
	}
	return out
}

// DecodeUnresolvedNameData turns a Diagnostic.Data round-tripped
// through the LSP client into the typed UnresolvedNameData.
// Returns nil when the payload isn't ours.
func DecodeUnresolvedNameData(v interface{}) *UnresolvedNameData {
	v = diagDataValue(v)
	switch d := v.(type) {
	case UnresolvedNameData:
		if d.Kind == DiagDataKindUnresolvedName {
			return &d
		}
	case *UnresolvedNameData:
		if d != nil && d.Kind == DiagDataKindUnresolvedName {
			return d
		}
	case map[string]interface{}:
		kind, _ := d["kind"].(string)
		if kind != DiagDataKindUnresolvedName {
			return nil
		}
		name, _ := d["name"].(string)
		var candidates []string
		if list, ok := d["candidates"].([]interface{}); ok {
			for _, c := range list {
				if s, ok := c.(string); ok {
					candidates = append(candidates, s)
				}
			}
		}
		return &UnresolvedNameData{Kind: kind, Name: name, Candidates: candidates}
	}
	return nil
}

// replaceUnresolvedNameActions builds one quick-fix per candidate
// declared function, closest first (the first is marked preferred),
// each replacing the diagnostic's range — the reference exactly as
// written — with the candidate's resolvable spelling.
func replaceUnresolvedNameActions(doc *Document, diag protocol.Diagnostic, data UnresolvedNameData) []protocol.CodeAction {
	out := make([]protocol.CodeAction, 0, len(data.Candidates))
	for i, candidate := range data.Candidates {
		out = append(out, protocol.CodeAction{
			Title:       fmt.Sprintf("Replace %q with %q", data.Name, candidate),
			Kind:        quickFixKind(),
			Diagnostics: []protocol.Diagnostic{diag},
			IsPreferred: boolPtr(i == 0),
			Edit: &protocol.WorkspaceEdit{
				Changes: map[uri.URI][]protocol.TextEdit{
					doc.URI: {{Range: diag.Range, NewText: candidate}},
				},
			},
		})
	}
	return out
}

// DecodeUnknownFieldData turns a Diagnostic.Data round-tripped through
// the LSP client (which usually delivers it as map[string]any after
// JSON unmarshalling) into the typed UnknownFieldData. Returns nil
// when the payload isn't ours.
func DecodeUnknownFieldData(v interface{}) *UnknownFieldData {
	v = diagDataValue(v)
	if v == nil {
		return nil
	}
	// Two shapes show up: the typed struct (when CodeAction handlers
	// are invoked within the same process, e.g. tests) and the
	// map[string]any the JSON-RPC client produces.
	if d, ok := v.(UnknownFieldData); ok && d.Kind == DiagDataKindUnknownField {
		return &d
	}
	if dp, ok := v.(*UnknownFieldData); ok && dp != nil && dp.Kind == DiagDataKindUnknownField {
		return dp
	}
	if m, ok := v.(map[string]interface{}); ok {
		kind, _ := m["kind"].(string)
		if kind != DiagDataKindUnknownField {
			return nil
		}
		name, _ := m["field_name"].(string)
		line, _ := m["line"].(float64) // JSON numbers come back as float64
		return &UnknownFieldData{
			Kind:      kind,
			FieldName: name,
			Line:      uint32(line), // #nosec G115 -- bounded by LSP line range
		}
	}
	return nil
}

// removeFieldAction builds a quick-fix CodeAction that deletes the
// entire line containing the unknown-field assignment. The diagnostic
// is included in the action so clients can group / annotate properly.
func removeFieldAction(doc *Document, diag protocol.Diagnostic, data UnknownFieldData) protocol.CodeAction {
	edit := protocol.TextEdit{
		Range:   lineRange(doc.Source, data.Line),
		NewText: "",
	}
	return protocol.CodeAction{
		Title:       fmt.Sprintf("Remove field %q", data.FieldName),
		Kind:        quickFixKind(),
		Diagnostics: []protocol.Diagnostic{diag},
		Edit: &protocol.WorkspaceEdit{
			Changes: map[uri.URI][]protocol.TextEdit{
				doc.URI: {edit},
			},
		},
	}
}

// lineRange returns the LSP Range covering line `line` (0-indexed)
// from column 0 to column 0 of the next line — i.e. the line plus its
// trailing newline. Editing that range to "" removes the line cleanly
// without leaving a blank.
func lineRange(source []byte, line uint32) protocol.Range {
	lineStart := offsetForLineCol(source, int(line)+1, 1)
	if lineStart < 0 {
		return protocol.Range{
			Start: protocol.Position{Line: line, Character: 0},
			End:   protocol.Position{Line: line, Character: 0},
		}
	}
	// End: start of the next line, or EOF if this is the last line.
	end := lineStart
	for end < len(source) && source[end] != '\n' {
		end++
	}
	if end < len(source) {
		end++ // include the trailing newline
	}
	// Compute the end as (line+1, 0) when we crossed a newline,
	// otherwise (line, lineLength).
	if end > lineStart && source[end-1] == '\n' {
		return protocol.Range{
			Start: protocol.Position{Line: line, Character: 0},
			End:   protocol.Position{Line: line + 1, Character: 0},
		}
	}
	return protocol.Range{
		Start: protocol.Position{Line: line, Character: 0},
		End:   protocol.Position{Line: line, Character: uint32(end - lineStart)}, // #nosec G115 -- bounded by source length
	}
}
