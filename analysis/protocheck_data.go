// Phase 1 of the protocheck → protolsp integration: runtime
// validation of .pxf data against schema-declared constraints.
// After validate(doc) unmarshals the source into a proto.Message,
// runProtocheck invokes the per-Server *protocheck.Validator and
// stores each violation as a protocheckFinding with a resolved
// source position; diagnosticsFor turns those into LSP diagnostics
// with source "protocheck".
//
// Complements Phase 0 (which validates the constraint expressions
// themselves at .proto-edit time) by surfacing red squiggles on
// the data file when concrete values violate the resolved rules —
// `age: 200` against `lte: 150`, etc.
//
// MVP scope for path resolution:
//   - Dotted descent through pxf.Block + pxf.Assignment(BlockVal)
//     for nested-message access (`address.zip_code`).
//   - Bracket-indexed `[N]` for ListVal-backed repeated fields
//     (`items[3].sku`).
//   - Map-key brackets (`labels["env"].value`) are NOT yet
//     resolved — those violations land at the parent assignment's
//     position. Acceptable for v1; map-targeted constraints are
//     rare in practice.
//   - Anything that doesn't resolve falls back to the document
//     origin (line 1, col 1) so the diagnostic still surfaces.
//     ConstraintID + Message tell the user what failed even when
//     the range is imprecise.
package analysis

import (
	"strconv"
	"strings"

	"go.lsp.dev/protocol"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// protocheckFinding records one constraint violation surfaced by
// the runtime validator, anchored to the .pxf source position the
// path resolved to. Defined tag-free so Document.protocheckFindings
// and the diagnostic-emission pipeline compile in both the OSS and
// trendvidia builds; the field stays empty in the OSS build
// because the runProtocheck implementation lives behind a build
// tag (see protocheck_data_real.go / _stub.go).
type protocheckFinding struct {
	FieldPath    string
	ConstraintID string
	Message      string
	// ForKey marks a violation against a map entry's key rather
	// than its value (protocheck v2 EnrichedViolation.ForKey).
	ForKey bool
	Pos    pxf.Position
	// Rule points at the schema declaration that produced the
	// violation — the v2 EnrichedViolation Source/SourceRef
	// enrichment, resolved to an editor location at finding
	// time (runProtocheck holds the workspace context; this
	// renderer doesn't). Nil when the engine carried no source
	// info for the rule (protocheckpb Check rules today — only
	// RFC-001 carrier rules embed a source map).
	Rule *protocol.Location
}

// protocheckCodeFunctionUnimplemented mirrors protocheck/v2's
// CodeFunctionUnimplemented. Duplicated as a literal so this
// tag-free file stays off the tagged-only import graph; the tagged
// test suite asserts the two stay equal.
const protocheckCodeFunctionUnimplemented = "protowire.function.unimplemented"

// protocheckDataDiagnostics renders the stored findings as LSP
// diagnostics. Distinct source ("protocheck") so editors can color
// and filter them independently from "protolsp/schema" (structural)
// and "protocheck/expression" (Phase 0).
func protocheckDataDiagnostics(doc *Document) []protocol.Diagnostic {
	if len(doc.protocheckFindings) == 0 {
		return nil
	}
	out := make([]protocol.Diagnostic, 0, len(doc.protocheckFindings))
	for _, f := range doc.protocheckFindings {
		msg := f.Message
		if f.ForKey {
			// Same rendering as protocheck v2's Report.Format and
			// the runtime-violation overlay's for_key handling.
			msg += " (key)"
		}
		severity := protocol.DiagnosticSeverityError
		if f.ConstraintID == protocheckCodeFunctionUnimplemented {
			// The constraint calls a declared function protolsp has
			// no implementation for — the data isn't known to be
			// wrong, the LSP just can't evaluate the rule.
			severity = protocol.DiagnosticSeverityInformation
		}
		d := protocol.Diagnostic{
			Range:    pxfPosToRange(f.Pos),
			Severity: severity,
			Source:   optString("protocheck"),
			Code:     protocol.String(f.ConstraintID),
			Message:  protocol.String(msg),
		}
		if f.Rule != nil {
			// "Go to rule": editors render related information as a
			// clickable link from the data violation to the schema
			// declaration that imposed it.
			d.RelatedInformation = []protocol.DiagnosticRelatedInformation{{
				Location: *f.Rule,
				Message:  "constraint declared here",
			}}
		}
		out = append(out, d)
	}
	return out
}

// resolveProtocheckPath walks the pxf entries to find the source
// position for a protocheck FieldPath. Returns (origin, false)
// when the path doesn't resolve — the caller still emits the
// diagnostic, just at the document origin so it's visible.
func resolveProtocheckPath(entries []pxf.Entry, path string) (pxf.Position, bool) {
	origin := pxf.Position{Line: 1, Column: 1}
	if path == "" {
		return origin, false
	}
	segments := splitFieldPath(path)
	if len(segments) == 0 {
		return origin, false
	}
	current := entries
	var lastPos pxf.Position
	for i := 0; i < len(segments); i++ {
		seg := segments[i]
		entry := findEntry(current, seg.Name)
		if entry == nil {
			return origin, false
		}
		lastPos = entryPos(entry)
		isLast := i == len(segments)-1

		// No bracket: descend through the entry's children.
		if !seg.HasIndex && !seg.HasKey {
			if isLast {
				return lastPos, true
			}
			next, ok := childEntries(entry)
			if !ok {
				return lastPos, true
			}
			current = next
			continue
		}

		// Bracket-indexed list element.
		if seg.HasIndex {
			lv, ok := entryValue(entry).(*pxf.ListVal)
			if !ok || seg.Index < 0 || seg.Index >= len(lv.Elements) {
				return lastPos, true
			}
			elem := lv.Elements[seg.Index]
			lastPos = valuePos(elem)
			if isLast {
				return lastPos, true
			}
			bv, ok := elem.(*pxf.BlockVal)
			if !ok {
				return lastPos, true
			}
			current = bv.Entries
			continue
		}

		// Map-key descent. PXF spells maps as
		// `labels = { key: "value" … }` — an Assignment whose
		// value is a BlockVal containing MapEntries. Find the
		// MapEntry by key; protocheck always emits ".value" or
		// ".key" after a map-key segment to disambiguate the
		// anchor, so we peek the next segment and consume it.
		bv, ok := entryValue(entry).(*pxf.BlockVal)
		if !ok {
			return lastPos, true
		}
		me := findMapEntryByKey(bv.Entries, seg.Key)
		if me == nil {
			return lastPos, true
		}
		lastPos = me.Pos
		if isLast {
			return lastPos, true
		}
		next := segments[i+1]
		if next.Name == "key" {
			// MapEntry.Pos already starts at the key token; nothing
			// more to do. Consume next and stop.
			return lastPos, true
		}
		if next.Name == "value" {
			lastPos = valuePos(me.Value)
			// If more segments follow, the map's value type is a
			// message (BlockVal); descend its entries.
			if i+2 < len(segments) {
				sub, ok := me.Value.(*pxf.BlockVal)
				if !ok {
					return lastPos, true
				}
				current = sub.Entries
				i++ // consume the "value" segment; the loop's ++ advances past it
				continue
			}
			return lastPos, true
		}
		// Unknown sub-key after a map-key segment — protocheck
		// doesn't emit anything besides ".key" / ".value", so
		// this is defensive. Surface at the MapEntry position.
		return lastPos, true
	}
	return lastPos, true
}

// findMapEntryByKey looks up the first *pxf.MapEntry with the
// given key in a flat list of entries. Returns nil when no entry
// matches; the caller falls back to the parent's position.
func findMapEntryByKey(entries []pxf.Entry, key string) *pxf.MapEntry {
	for _, e := range entries {
		if me, ok := e.(*pxf.MapEntry); ok && me.Key == key {
			return me
		}
	}
	return nil
}

// pathSegment is one dotted-path component plus an optional
// bracket suffix.
type pathSegment struct {
	Name     string
	HasIndex bool
	Index    int
	HasKey   bool   // reserved for map-key resolution
	Key      string // reserved for map-key resolution
}

// splitFieldPath splits a protocheck path on '.' at the top level,
// respecting brackets. `items[3].sku` → [{items, idx=3}, {sku}].
// Not a general-purpose parser; protocheck's path generator is the
// only producer we target.
func splitFieldPath(path string) []pathSegment {
	var segs []pathSegment
	depth := 0
	start := 0
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '.':
			if depth == 0 {
				if seg, ok := parseSegment(path[start:i]); ok {
					segs = append(segs, seg)
				}
				start = i + 1
			}
		}
	}
	if start < len(path) {
		if seg, ok := parseSegment(path[start:]); ok {
			segs = append(segs, seg)
		}
	}
	return segs
}

func parseSegment(s string) (pathSegment, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return pathSegment{}, false
	}
	open := strings.IndexByte(s, '[')
	if open < 0 {
		return pathSegment{Name: s}, true
	}
	closeIdx := strings.LastIndexByte(s, ']')
	if closeIdx < open {
		return pathSegment{Name: s}, true
	}
	name := strings.TrimSpace(s[:open])
	inner := strings.TrimSpace(s[open+1 : closeIdx])
	if n, err := strconv.Atoi(inner); err == nil {
		return pathSegment{Name: name, HasIndex: true, Index: n}, true
	}
	// Strip one layer of quotes for future map-key descent.
	if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
		inner = inner[1 : len(inner)-1]
	}
	return pathSegment{Name: name, HasKey: true, Key: inner}, true
}

func findEntry(entries []pxf.Entry, name string) pxf.Entry {
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if n.Key == name {
				return n
			}
		case *pxf.Block:
			if n.Name == name {
				return n
			}
		case *pxf.MapEntry:
			if n.Key == name {
				return n
			}
		}
	}
	return nil
}

func entryValue(e pxf.Entry) pxf.Value {
	switch n := e.(type) {
	case *pxf.Assignment:
		return n.Value
	case *pxf.MapEntry:
		return n.Value
	}
	return nil
}

// childEntries returns the entries to descend into for a
// non-indexed path segment. Block's own Entries, or the inner
// Entries of a BlockVal-valued Assignment/MapEntry. Anything else
// (scalar value, list value) is not descendable.
func childEntries(e pxf.Entry) ([]pxf.Entry, bool) {
	switch n := e.(type) {
	case *pxf.Block:
		return n.Entries, true
	case *pxf.Assignment:
		if bv, ok := n.Value.(*pxf.BlockVal); ok {
			return bv.Entries, true
		}
	case *pxf.MapEntry:
		if bv, ok := n.Value.(*pxf.BlockVal); ok {
			return bv.Entries, true
		}
	}
	return nil, false
}

func valuePos(v pxf.Value) pxf.Position {
	switch n := v.(type) {
	case *pxf.BlockVal:
		return n.Pos
	case *pxf.ListVal:
		return n.Pos
	case *pxf.StringVal:
		return n.Pos
	case *pxf.IntVal:
		return n.Pos
	case *pxf.FloatVal:
		return n.Pos
	case *pxf.BoolVal:
		return n.Pos
	case *pxf.BytesVal:
		return n.Pos
	case *pxf.NullVal:
		return n.Pos
	case *pxf.IdentVal:
		return n.Pos
	case *pxf.TimestampVal:
		return n.Pos
	case *pxf.DurationVal:
		return n.Pos
	}
	return pxf.Position{Line: 1, Column: 1}
}
