// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// R2 of the dataset-regions design. After R1's line-based pre-pass
// populates Document.DatasetRegions, this pass walks the parser's
// already-extracted pxf.Document.Datasets to fill in RowCount and
// Resolvable. Falls through silently when the parse failed or the
// resolver doesn't know the dataset's type — R1's region table
// stays valid in either case (just without row counts).
//
// Also defines the protolsp/datasetRegionsChanged notification and
// the diff helper that decides when to emit. Same shape as S2's
// secretRangesChanged.

package analysis

import (
	"context"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

const (
	// MethodDatasetRegionsChanged is the push notification emitted
	// when an edit shifts the dataset-region table for a document.
	// Editors refresh their masking / placeholder decoration from
	// this without polling.
	MethodDatasetRegionsChanged = "protolsp/datasetRegionsChanged"

	// defaultHugeRegionRows is the row-count threshold above which
	// a region is marked Huge by default. Per the dataset-regions
	// design: 1000 rows or 100 KB, whichever hits first. R2 adds
	// the row criterion; R1 only had bytes.
	defaultHugeRegionRows = 1000
)

// DatasetRegionsChangedParams is the notification shape. Mirrors
// SecretRangesChangedParams so editor implementations can share
// notification-handling code between the two.
type DatasetRegionsChangedParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
	Version      int32                           `json:"version"`
	Regions      []DatasetRegion                 `json:"regions"`
}

// enrichDatasetRegionsTyped walks the parser's Datasets slice and
// fills RowCount + Resolvable on the matching DatasetRegion entries.
// Matching is by start line (0-indexed LSP). When parsed and line-
// based passes disagree (e.g. parser dropped a malformed dataset),
// the line-based pass is authoritative for what exists — AST
// enrichment only fills in additional fields when the parser
// confirms.
//
// Also re-evaluates Huge once RowCount is known (the design's
// threshold = rows OR bytes, whichever hits first). Pass 0 for
// thresholds to use the defaults. typeOf names each dataset's row
// type: its own, or the anonymous @proto bound to it.
func enrichDatasetRegionsTyped(regions []DatasetRegion, parsed *pxf.Document, lookup datasetLookup, hugeRows, hugeBytes int, typeOf func(*pxf.DatasetDirective) string) []DatasetRegion {
	if hugeRows <= 0 {
		hugeRows = defaultHugeRegionRows
	}
	if hugeBytes <= 0 {
		hugeBytes = defaultHugeRegionBytes
	}
	if parsed == nil || len(parsed.Datasets) == 0 {
		return regions
	}
	// Index parsed datasets by their start line, converted from
	// pxf's 1-indexed to LSP's 0-indexed form.
	byLine := make(map[uint32]*pxf.DatasetDirective, len(parsed.Datasets))
	for i := range parsed.Datasets {
		d := &parsed.Datasets[i]
		var line uint32
		if d.Pos.Line > 0 {
			line = uint32(d.Pos.Line - 1) // #nosec G115 -- pxf line numbers fit in uint32 by document bounds
		}
		byLine[line] = d
	}
	for i := range regions {
		d, ok := byLine[regions[i].Range.Start.Line]
		if !ok {
			continue
		}
		// AST is authoritative for type — the R1 line scanner
		// only knows up to the next delimiter, which can omit
		// trailing qualifications. Overwrite with the canonical
		// parsed value.
		if d.Type != "" {
			regions[i].TypeFQN = d.Type
		}
		regions[i].RowCount = len(d.Rows)
		// A dataset bound to an anonymous @proto has no Type; typeOf
		// names its synthetic type, and TypeFQN stays empty.
		if name := typeOf(d); lookup != nil && name != "" {
			if _, err := lookup.FindMessageByName(protoreflect.FullName(name)); err == nil {
				regions[i].Resolvable = true
			}
		}
		// Recompute Huge now that RowCount is known.
		regions[i].Huge = regions[i].RowCount >= hugeRows || regions[i].ByteSize >= hugeBytes
	}
	return regions
}

// datasetLookup is a narrow interface for the resolver lookup
// needed by the AST pass. Defined locally so tests can pass a
// minimal fake without standing up a full schema.Lookup.
type datasetLookup interface {
	FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error)
}

// datasetRegionsEqual reports whether two DatasetRegion slices have
// the same content in the same order. Used by the notification
// emit path to skip same-shape changes.
func datasetRegionsEqual(a, b []DatasetRegion) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// notifyDatasetRegionsChanged emits the notification when current
// differs from previous. Skipped when the snapshots are
// byte-identical.
func (s *Workspace) notifyDatasetRegionsChanged(ctx context.Context, docURI uri.URI, version int32, previous, current []DatasetRegion) {
	if datasetRegionsEqual(previous, current) {
		return
	}
	s.pub.DatasetRegionsChanged(ctx, docURI, version, current)
}
