// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// I1 of the incremental-sync design: apply LSP incremental edits to
// a document's source bytes. Each edit either describes a range to
// replace (incremental form) or carries the whole new document
// (full-document fallback — clients may send this even with
// incremental advertised, e.g. on format-on-save).
//
// Position handling uses LSP's default UTF-16 code-unit convention.
// Position-encoding negotiation (preference for UTF-8) is I2 — out
// of scope here.
//
// Edits in a single notification apply IN ORDER. Each subsequent
// edit's range is relative to the document state AFTER the previous
// edit. Per the LSP spec. Getting this wrong silently corrupts the
// server's view; the apply loop here is the single source of truth.

package analysis

import (
	"encoding/json"
	"fmt"

	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

// IncrementalChange mirrors LSP's TextDocumentContentChangeEvent but
// uses *Range so we can tell "range omitted" (full-document
// replacement) from "range at origin" (incremental edit at line
// 0:col 0). The upstream protocol type uses a value Range which
// makes this ambiguous after unmarshal — hence the custom shape.
//
// Text is the replacement bytes (or the full document when Range
// is nil).
type IncrementalChange struct {
	Range *ProtocolRangeShape `json:"range,omitempty"`
	Text  string              `json:"text"`
}

// ProtocolRangeShape mirrors protocol.Range purely so its zero
// value isn't ambiguous with "absent". We can't use protocol.Range
// directly here because it's a struct (zero-valued when absent is
// indistinguishable from zero-valued when present); via this local
// alias we use a pointer.
type ProtocolRangeShape struct {
	Start PositionShape `json:"start"`
	End   PositionShape `json:"end"`
}

type PositionShape struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// ApplyContentChanges walks a list of incremental changes and
// returns the resulting source bytes. Each change applies relative
// to the state after the previous one — matching the LSP spec.
//
// Errors are non-fatal at the protocol layer; the dispatcher logs
// and continues with whatever state the apply loop produced. We
// still return them here so the dispatcher can log specific
// offending ranges.
func ApplyContentChanges(initial []byte, changes []IncrementalChange, enc positions.Encoding) ([]byte, []error) {
	src := append([]byte(nil), initial...) // defensive copy
	var errs []error
	for i, ch := range changes {
		next, err := applyOneChange(src, ch, enc)
		if err != nil {
			errs = append(errs, fmt.Errorf("change %d: %w", i, err))
			continue
		}
		src = next
	}
	return src, errs
}

// applyOneChange applies a single change. Range == nil means
// "replace the entire document with Text" (full-document fallback).
// Otherwise it's an incremental edit: extract byte offsets, splice.
//
// enc selects how Position.character is interpreted (UTF-16 code
// units vs UTF-8 byte offsets). The negotiated value comes from
// initialize; see Server.positionEncoding.
func applyOneChange(src []byte, ch IncrementalChange, enc positions.Encoding) ([]byte, error) {
	if ch.Range == nil {
		return []byte(ch.Text), nil
	}
	startByte, err := positions.Offset(src, protocol.Position{
		Line: ch.Range.Start.Line, Character: ch.Range.Start.Character,
	}, enc)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	endByte, err := positions.Offset(src, protocol.Position{
		Line: ch.Range.End.Line, Character: ch.Range.End.Character,
	}, enc)
	if err != nil {
		return nil, fmt.Errorf("end: %w", err)
	}
	if startByte > endByte {
		return nil, fmt.Errorf("inverted range: start byte %d > end byte %d", startByte, endByte)
	}
	out := make([]byte, 0, len(src)-(endByte-startByte)+len(ch.Text))
	out = append(out, src[:startByte]...)
	out = append(out, ch.Text...)
	out = append(out, src[endByte:]...)
	return out, nil
}

// UnmarshalIncrementalChanges parses the contentChanges array from
// a didChange notification's params. We unmarshal via the local
// IncrementalChange type (which uses *Range) so we can distinguish
// full-document events from incremental ones — the upstream
// protocol.TextDocumentContentChangeEvent uses a value Range and
// loses that distinction.
func UnmarshalIncrementalChanges(raw json.RawMessage) ([]IncrementalChange, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []IncrementalChange
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("contentChanges: %w", err)
	}
	return out, nil
}
