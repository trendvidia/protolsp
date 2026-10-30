// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C4 of the chameleon-integration design (first slice): recognize
// ${field.path} interpolation references in scalar string values
// and emit unresolvable-reference diagnostics against the bound
// message descriptor.
//
// This slice covers recognition + the resolvable check. Cycle
// detection across reference graphs and completion for cursor-
// inside-${} are follow-ups (C4b — separate slices to keep each
// reviewable).
//
// Recognition runs against the AST: we walk every Assignment /
// MapEntry / Block whose value is a *pxf.StringVal and scan that
// value for ${X.Y.Z} patterns. The position arithmetic anchors on
// the StringVal's source position (the opening quote) and adds the
// content-offset of each match. Triple-quoted multi-line strings
// are handled best-effort (the column may be off when the
// reference is on a line after the opening quote; the line is
// computed from in-content newlines).
//
// Path resolution walks the bound descriptor: each dotted segment
// must match a Field name on the enclosing MessageDescriptor.
// Non-message segments terminate the walk — a path of length 1
// resolves to any field; length > 1 requires the parent to be
// MessageKind. Maps and lists are not yet supported in the path
// syntax (would require `[key]` / `[i]` parsing); they currently
// resolve as if the field were scalar — which is wrong but errs on
// the side of "no false positive" since the literal field name
// lookup still succeeds.

package analysis

import (
	"fmt"
	"strings"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/interp"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// Scanning for ${path} references uses the shared
// internal/interp matcher so workspaceconfig (env-substitution)
// and the server (field-ref analyze) agree on what counts as a
// reference token. Path shape: one-or-more identifier segments
// separated by dots; identifiers start with a letter or
// underscore. Empty `${}` doesn't match (no diagnostic emitted
// — it's not a reference, just inert text the runtime would
// also reject downstream).
//
// `${ctx.X}` and `${env.X}` first-segment forms match
// syntactically (they're valid `${path}` shapes) but are
// classified downstream:
//   - `ctx.*` is context-keyed; isContextRef skips it on the
//     diagnostic path.
//   - `env.*` is workspace-config env interpolation; resolved
//     at .protolsp.pxf parse time by workspaceconfig.interpolate
//     before the value ever reaches analyze, so the field-ref
//     pass only sees env references in user content that didn't
//     flow through the config parser (chameleon layer files,
//     etc.) — those are also skipped.

// fieldReference is one ${path} occurrence found in source. Range
// covers the full reference including the `${` and `}` delimiters
// so editors highlight the whole token.
type fieldReference struct {
	Path  string
	Range protocol.Range
}

// collectFieldReferences walks the parsed AST and returns every
// ${path} reference found in scalar string values, with source
// positions computed off the enclosing StringVal's Pos. Returns
// nil for nil / unparsed documents — caller falls through to "no
// diagnostics."
func collectFieldReferences(parsed *pxf.Document) []fieldReference {
	if parsed == nil {
		return nil
	}
	var refs []fieldReference
	walkStringValues(parsed.Entries, func(v *pxf.StringVal) {
		refs = append(refs, scanStringForRefs(v)...)
	})
	return refs
}

// walkStringValues recursively walks entries and invokes fn for
// every Assignment / MapEntry whose value is a *pxf.StringVal,
// descending into Blocks. List elements aren't walked in this
// slice — covering lists is straightforward (range ListVal.Elements)
// but the realistic case is rare enough that we defer until users
// ask for it. ListVal handling tracks alongside the C4b cycle-
// detection slice.
func walkStringValues(entries []pxf.Entry, fn func(*pxf.StringVal)) {
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if sv, ok := n.Value.(*pxf.StringVal); ok {
				fn(sv)
			}
		case *pxf.MapEntry:
			if sv, ok := n.Value.(*pxf.StringVal); ok {
				fn(sv)
			}
		case *pxf.Block:
			walkStringValues(n.Entries, fn)
		}
	}
}

// scanStringForRefs extracts ${path} matches from a StringVal and
// returns a fieldReference per match. Positions are computed off
// the StringVal's Pos (which points at the opening quote in
// source): line = sv.Pos.Line + newlines-before-match;
// column = sv.Pos.Column + 1 (quote) + offset-on-current-line.
//
// Triple-quoted multi-line strings are handled approximately: the
// per-match line offset is correct (we count `\n` bytes), but the
// column for matches on lines after the opener doesn't account for
// triple-quote indentation. C4 v1 tolerates that; downstream
// editors render the diagnostic on the right LINE which is the
// critical UX.
func scanStringForRefs(sv *pxf.StringVal) []fieldReference {
	if sv == nil || sv.Value == "" {
		return nil
	}
	refs := interp.FindAll(sv.Value)
	if len(refs) == 0 {
		return nil
	}
	out := make([]fieldReference, 0, len(refs))
	for _, r := range refs {
		startLine, startCol := offsetToLineCol(sv, r.Start)
		endLine, endCol := offsetToLineCol(sv, r.End)
		out = append(out, fieldReference{
			Path: r.Path,
			Range: protocol.Range{
				Start: protocol.Position{Line: startLine, Character: startCol},
				End:   protocol.Position{Line: endLine, Character: endCol},
			},
		})
	}
	return out
}

// offsetToLineCol converts a byte offset within a StringVal's value
// to an (LSP line, LSP column) pair anchored at the StringVal's
// source position.
//
// The +1 column adjustment skips the opening quote — StringVal.Pos
// points at `"`, the content starts one column later. For matches
// on a line AFTER the opener (multi-line strings), the column
// starts at 0 from the line origin (we lose the leading-indent
// information that the parser stripped from Value).
func offsetToLineCol(sv *pxf.StringVal, offset int) (uint32, uint32) {
	// Count newlines + last-newline column within sv.Value[:offset].
	content := sv.Value[:offset]
	newlines := strings.Count(content, "\n")
	lastNL := strings.LastIndexByte(content, '\n')
	var lineByteOffset int
	if lastNL >= 0 {
		lineByteOffset = offset - (lastNL + 1)
	} else {
		lineByteOffset = offset
	}
	// pxf line/col are 1-indexed; LSP is 0-indexed.
	lspLine := uint32(sv.Pos.Line - 1)  // #nosec G115 -- pxf.Pos.Line is positive
	lspCol := uint32(sv.Pos.Column - 1) // #nosec G115 -- pxf.Pos.Column is positive
	if newlines > 0 {
		lspLine += uint32(newlines)     // #nosec G115 -- newline count fits
		lspCol = uint32(lineByteOffset) // #nosec G115 -- byte offset within line is bounded
	} else {
		// Same line as the opening quote: skip the quote.
		lspCol += 1 + uint32(lineByteOffset) // #nosec G115 -- byte offset fits
	}
	return lspLine, lspCol
}

// resolveFieldPath walks a dotted path against the descriptor,
// returning (ok, isContextRef):
//   - ok=true: path resolves cleanly; no diagnostic.
//   - ok=false, isContextRef=true: the first segment is `env` or
//     `ctx` — runtime-interpolated context keys, NOT field
//     references. Caller skips these.
//   - ok=false, isContextRef=false: the path is a real field
//     reference that doesn't exist on the schema. Caller emits a
//     diagnostic.
//
// Path syntax accepted by this C4 slice: `a`, `a.b`, `a.b.c`.
// Map / list element subscripts (`a[key]`, `a[0]`) are not in v1
// — they'd require additional grammar; until then references that
// use them won't match fieldRefPattern at all (the regex rejects
// `[` and `]`).
func resolveFieldPath(path string, desc protoreflect.MessageDescriptor) (ok bool, isContextRef bool) {
	if path == "" || desc == nil {
		return false, false
	}
	segments := strings.Split(path, ".")
	if len(segments) > 0 {
		switch segments[0] {
		case "env", "ctx":
			return false, true // context interpolation, not a field reference
		}
	}
	currentMd := desc
	for i, seg := range segments {
		if currentMd == nil {
			return false, false
		}
		fd := currentMd.Fields().ByName(protoreflect.Name(seg))
		if fd == nil {
			return false, false
		}
		// Last segment: any kind is fine.
		if i == len(segments)-1 {
			return true, false
		}
		// Intermediate segment: must be a message kind to descend.
		if fd.Kind() != protoreflect.MessageKind {
			return false, false
		}
		currentMd = fd.Message()
	}
	return true, false
}

// fieldReferenceDiagnostics is the entry point appended to
// diagnosticsFor. For every ${path} reference in the document
// that's a real field reference (not a context interpolation) and
// doesn't resolve against the bound message descriptor, emits an
// Error.
//
// Returns nil when: no schema bound, no @type directive, the
// document didn't parse. The diagnostic is silent in those modes
// rather than firing speculative errors — there's no descriptor to
// resolve against.
func (s *Workspace) fieldReferenceDiagnostics(doc *Document) []protocol.Diagnostic {
	if s == nil || doc == nil || doc.Parsed == nil {
		return nil
	}
	if doc.Parsed.TypeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(doc.Parsed.TypeURL))
	if err != nil || mt == nil {
		return nil
	}
	desc := mt.Descriptor()
	refs := collectFieldReferences(doc.Parsed)
	if len(refs) == 0 {
		return nil
	}
	var out []protocol.Diagnostic
	for _, ref := range refs {
		ok, isContext := resolveFieldPath(ref.Path, desc)
		if ok || isContext {
			continue
		}
		out = append(out, protocol.Diagnostic{
			Range:    ref.Range,
			Severity: protocol.DiagnosticSeverityError,
			Source:   optString("protolsp/schema"),
			Message: protocol.String(fmt.Sprintf(
				"reference ${%s} does not name a field on %q",
				ref.Path, desc.FullName())),
		})
	}
	return out
}
