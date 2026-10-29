// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// S2 of the secret-handling design. Exposes the Document.SecretRanges
// table to editors over two custom LSP methods so they can render
// their own masking decoration on the source ranges of pxf.Secret
// values. Mirrors the protolsp/datasetRegions shape — pull request
// + push notification.
//
// Detection is done in S1 (secret_ranges.go's collectSecretRanges).
// This file is pure protocol surface + change-detection logic.

package analysis

import (
	"context"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const (
	// MethodSecretRanges is the custom request returning the
	// current SecretRange list for a document.
	MethodSecretRanges = "protolsp/secretRanges"

	// MethodSecretRangesChanged is the push notification emitted
	// when an edit shifts SecretRanges. Editors refresh their
	// masking decoration from this without polling.
	MethodSecretRangesChanged = "protolsp/secretRangesChanged"
)

// SecretRangesParams is the request shape for [MethodSecretRanges].
type SecretRangesParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
}

// SecretRangeEntry is one element of the protocol-level response.
// Mirrors the in-memory SecretRange but uses string literals for
// the form field so the wire payload is self-describing without
// the client needing to know our int enum.
type SecretRangeEntry struct {
	Range     protocol.Range `json:"range"`
	FieldName string         `json:"fieldName"`
	Form      string         `json:"form"` // "scalar" | "block"
}

// SecretRangesResult is the response shape for [MethodSecretRanges].
type SecretRangesResult struct {
	Ranges []SecretRangeEntry `json:"ranges"`
}

// SecretRangesChangedParams is the notification shape. Tagged with
// the document version so the editor can discard out-of-order
// notifications.
type SecretRangesChangedParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
	Version      int32                           `json:"version"`
	Ranges       []SecretRangeEntry              `json:"ranges"`
}

// notifySecretRangesChanged emits protolsp/secretRangesChanged with
// the document's current snapshot. Called from analyze callers
// (didOpen / didChange) after the secret-range table has been
// rebuilt. Skipped when the previous and new tables are byte-
// identical — same-shape edits don't generate wire traffic.
//
// `previous` is the snapshot from before the edit applied.
// Callers capture it before mutating doc; we compare against
// doc.SecretRanges.
func (s *Workspace) notifySecretRangesChanged(ctx context.Context, docURI uri.URI, version int32, previous, current []SecretRange) {
	if secretRangesEqual(previous, current) {
		return
	}
	s.pub.SecretRangesChanged(ctx, docURI, version, current)
}

// ToSecretRangeEntries converts the server-internal SecretRange
// slice to the wire-format SecretRangeEntry slice. Always returns
// a non-nil empty slice (LSP convention: empty array, not null).
func ToSecretRangeEntries(ranges []SecretRange) []SecretRangeEntry {
	out := make([]SecretRangeEntry, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, SecretRangeEntry{
			Range:     r.Range,
			FieldName: r.Path,
			Form:      formString(r.Form),
		})
	}
	return out
}

// formString translates the internal SecretForm enum to its
// wire-format string.
func formString(f SecretForm) string {
	switch f {
	case SecretFormBlock:
		return "block"
	default:
		return "scalar"
	}
}

// secretRangesEqual reports whether two SecretRange slices have
// the same content in the same order. Cheap deep-equal for small
// slices (typically <10 entries per document).
func secretRangesEqual(a, b []SecretRange) bool {
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
