// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Helpers for the optional and any-typed fields of go.lsp.dev/protocol.
// An empty string stays unset, so these fields are omitted from the
// wire rather than sent as "".

package analysis

import (
	"encoding/json"

	"go.lsp.dev/protocol"
)

func optString(s string) protocol.Optional[string] {
	if s == "" {
		return protocol.Optional[string]{}
	}
	return protocol.NewOptional(s)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func boolPtr(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

// mustAny encodes one of this package's diagnostic data structs. Those
// are plain string fields and slices, so the encoding cannot fail.
func mustAny(v any) protocol.LSPAny {
	b, err := json.Marshal(v)
	if err != nil {
		panic("analysis: encoding diagnostic data: " + err.Error())
	}
	return b
}

func quickFixKind() *protocol.CodeActionKind {
	k := protocol.CodeActionKindQuickFix
	return &k
}

func isTrue(b *bool) bool { return b != nil && *b }

// MessageText returns a diagnostic message as text, whichever form it
// takes on the wire.
func MessageText(m protocol.InlayHintTooltip) string {
	switch v := m.(type) {
	case protocol.String:
		return string(v)
	case *protocol.MarkupContent:
		if v != nil {
			return v.Value
		}
	}
	return ""
}

// hoverText returns the markdown body of a hover built by this package.
func hoverText(h *protocol.Hover) string {
	if h == nil {
		return ""
	}
	if c, ok := h.Contents.(*protocol.MarkupContent); ok && c != nil {
		return c.Value
	}
	return ""
}

// diagDataValue decodes a Diagnostic.Data value that came back from the
// client, so the Decode* functions see a map as they do for any other
// JSON payload. Values that aren't raw JSON pass through.
func diagDataValue(v any) any {
	raw, ok := v.(protocol.LSPAny)
	if !ok {
		return v
	}
	if len(raw) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
