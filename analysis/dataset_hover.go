// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// R3 of the dataset-regions design: region-aware hover. When the
// cursor lands inside a @dataset region, hover surfaces the
// region's row type plus a row-count summary — useful both for
// pure-@dataset documents (which have no @type to bind hover
// against today) and as a quick "what is this" affordance over
// any directive line.
//
// R3 v1 is intentionally narrow: hover on the directive line
// itself, not on individual row cells. Cell-level hover requires
// column-position parsing which is bigger scope; the
// RegionAtPosition helper added here is the wiring future
// phases (R3+, R4) will build on.

package analysis

import (
	"fmt"
	"strings"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// RegionAtPosition returns the DatasetRegion that contains pos,
// or nil when pos is outside every region. Containment is
// half-open in line/column space: a region's End position is
// exclusive (the byte after the closing `}` or the start of the
// first non-tuple line). Comparison is lexicographic on
// (Line, Character).
func (s *Workspace) RegionAtPosition(doc *Document, pos protocol.Position) *DatasetRegion {
	if doc == nil {
		return nil
	}
	for i := range doc.DatasetRegions {
		r := &doc.DatasetRegions[i]
		if positionInRange(pos, r.Range) {
			return r
		}
	}
	return nil
}

// positionInRange reports whether pos falls inside [Start, End).
// Lexicographic comparison: line is primary, character secondary.
// End is exclusive.
func positionInRange(pos protocol.Position, r protocol.Range) bool {
	if pos.Line < r.Start.Line {
		return false
	}
	if pos.Line == r.Start.Line && pos.Character < r.Start.Character {
		return false
	}
	if pos.Line > r.End.Line {
		return false
	}
	if pos.Line == r.End.Line && pos.Character >= r.End.Character {
		return false
	}
	return true
}

// renderDatasetHover builds the hover markdown for a cursor that
// fell inside a region. Today only renders meaningful content
// when the cursor is on the directive line — the line containing
// `@dataset`. Cursor inside the body (rows / block entries)
// returns "" so the caller falls through to today's entry-walk
// hover or returns nil.
//
// The directive-line hover includes:
//   - The region's TypeFQN (canonical name from the AST when
//     parse succeeded; the line-scanner's approximation when
//     parse failed)
//   - Row count when known (R2's AST pass; "-1" → "unknown" hint)
//   - Resolvability status — when the type isn't in the resolver
//     we say so plainly so users see the misconfiguration
//   - The schema-defined leading doc comment when the type
//     resolves
func (s *Workspace) renderDatasetHover(doc *Document, region *DatasetRegion, pos protocol.Position) string {
	if pos.Line != region.Range.Start.Line {
		// Cell-level hover is out of scope for R3; cursor inside
		// the body falls through to today's entry-walk handler,
		// which is currently a no-op for @dataset documents.
		return ""
	}
	// The row type as validation resolves it: a dataset bound to an
	// anonymous @proto has no TypeFQN, and its synthetic name is not
	// shown (#287).
	name, anonymous := region.TypeFQN, false
	if doc.Parsed != nil {
		if d := indexParsedDatasets(doc.Parsed.Datasets)[region.Range.Start.Line]; d != nil {
			name = doc.datasetType(d)
			anonymous = d.Type == "" && name != ""
		}
	}
	var b strings.Builder
	if anonymous {
		b.WriteString("**@dataset** of the anonymous `@proto` row type\n\n")
	} else {
		fmt.Fprintf(&b, "**@dataset** `%s`\n\n", region.TypeFQN)
	}
	if region.RowCount >= 0 {
		fmt.Fprintf(&b, "%d row", region.RowCount)
		if region.RowCount != 1 {
			b.WriteByte('s')
		}
		if region.Huge {
			b.WriteString(" · huge")
		}
		b.WriteString("\n\n")
	}
	if !region.Resolvable {
		// "Not known to resolver" only makes sense when the user
		// actually configured one. Without a user lookup,
		// surfacing this line would be misleading — users opted
		// out of schema validation by not configuring a registry.
		// Datasets always bind to user-domain types (never to
		// protolsp's internal descriptors), so the internal
		// lookup never helps here.
		if !s.hasUserLookup() {
			return ""
		}
		fmt.Fprintf(&b, "Type `%s` is not known to the bound schema resolver.\n", region.TypeFQN)
		return b.String()
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(name))
	if err != nil || mt == nil {
		// Resolvable was true a moment ago but the resolver
		// disagrees now — probably mid-refresh. Treat as
		// unknown for this hover; the next analyze fixes it.
		fmt.Fprintf(&b, "Type `%s` is not known to the bound schema resolver.\n", region.TypeFQN)
		return b.String()
	}
	desc := mt.Descriptor()
	file := desc.ParentFile()
	if doc.inline.findMessage(desc.FullName()) != nil {
		// Its file is the virtual one the @proto compiled to.
		b.WriteString("Defined in this document's `@proto`\n")
		return b.String()
	}
	if file != nil {
		fmt.Fprintf(&b, "Defined in `%s`", file.Path())
		if origin, ok := s.fileOriginFor(file.Path()); ok && origin != "" {
			fmt.Fprintf(&b, " (namespace `%s`)", origin)
		}
		b.WriteString("\n")
		// Leading doc comment from the proto source.
		if loc := file.SourceLocations().ByDescriptor(desc); loc.LeadingComments != "" {
			fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(loc.LeadingComments))
		}
	}
	return b.String()
}

// fileOriginFor returns the namespace ID that contributed the
// given .proto file, when the bound lookup attributes it. Empty
// for ad-hoc parents whose namespace identity isn't tracked.
// Falls back gracefully on Lookups that don't implement
// FindFileByPathWithOrigin.
func (s *Workspace) fileOriginFor(path string) (string, bool) {
	lk := s.lookupMgr.Snapshot()
	if lk == nil {
		return "", false
	}
	_, origin, err := lk.FindFileByPathWithOrigin(path)
	if err != nil {
		return "", false
	}
	return origin, true
}
