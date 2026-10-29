// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// keyedDiagnostics renders the keyed-repeated-field violations the
// validate pass stashed on the document (draft -01 §3.13) as LSP
// diagnostics: duplicate entry names within a keyed block, empty keys,
// explicit key-field assignments disagreeing with their entry name, and
// quoted entry names used outside a keyed field. Error severity — each
// makes the document fail a strict decode.
//
// Caller holds doc.mu (read or write); this reads doc.keyedFindings and
// doc.Source without re-locking.
func (s *Workspace) keyedDiagnostics(doc *Document) []protocol.Diagnostic {
	if len(doc.keyedFindings) == 0 {
		return nil
	}
	out := make([]protocol.Diagnostic, 0, len(doc.keyedFindings))
	for i := range doc.keyedFindings {
		k := doc.keyedFindings[i]
		out = append(out, protocol.Diagnostic{
			Range:    keyedErrorRange(doc.Source, k.Pos, s.positionEncoding),
			Severity: protocol.DiagnosticSeverityError,
			Source:   "protolsp/keyed",
			Message:  k.Msg,
		})
	}
	return out
}

// keyedErrorRange builds the squiggle range for a keyed diagnostic. The
// error's position points at the offending token — an entry name or a
// conflicting key-field value — so the range spans that whole token:
// the identifier bytes for a bare name, or the full string literal
// (quotes included) when the source spelled it quoted. Falls back to a
// single-character range when the token can't be delimited.
func keyedErrorRange(src []byte, pos pxf.Position, enc positions.Encoding) protocol.Range {
	off := pos.Offset
	startLine, startChar := positions.LineCol(src, off, enc)
	start := protocol.Position{Line: startLine, Character: startChar}
	quoted := off >= 0 && off < len(src) && src[off] == '"'
	if end := nameTokenEnd(src, off, quoted); end >= 0 {
		endLine, endChar := positions.LineCol(src, end, enc)
		return protocol.Range{Start: start, End: protocol.Position{Line: endLine, Character: endChar}}
	}
	end := start
	end.Character++
	return protocol.Range{Start: start, End: end}
}
