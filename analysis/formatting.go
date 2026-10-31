// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"bytes"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// ProtoFormattingEditsFor formats the .proto document with
// protocompile's printer (see FormatProto) and returns a
// whole-document TextEdit when that changes the source. Returns nil
// when:
//
//   - the doc isn't open or has no source,
//   - the source doesn't parse (typically mid-edit), or the printer's
//     output would not keep every token and comment of the input;
//     a log line goes out via slog so users can dig into it via the
//     trace panel.
//
// Returns an empty (non-nil) edit slice when the source is already
// canonical — mirrors FormattingEditsFor's contract so "format on
// save" doesn't dirty the buffer unnecessarily.
func (s *Workspace) ProtoFormattingEditsFor(docURI uri.URI) []protocol.TextEdit {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	src := append([]byte(nil), doc.Source...)
	doc.mu.RUnlock()
	if len(src) == 0 {
		return nil
	}
	formatted, err := FormatProto(string(docURI), src)
	if err != nil {
		s.log.Warn("proto formatting failed", "uri", docURI, "err", err)
		return nil
	}
	if bytes.Equal(formatted, src) {
		return []protocol.TextEdit{}
	}
	endLine, endCharacter := positions.LineCol(src, len(src), s.positionEncoding)
	return []protocol.TextEdit{{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: endLine, Character: endCharacter},
		},
		NewText: string(formatted),
	}}
}

// FormattingEditsFor returns the TextEdits needed to bring a document
// to canonical form, or nil when there's nothing to apply (doc not
// open, doc didn't parse, doc already canonical).
//
// "Already canonical" returns an empty (non-nil) slice rather than
// nil so the client treats it as "formatter ran, no edits needed"
// instead of "formatter unavailable" — the difference matters for
// editor commands like "format on save" that want to suppress
// no-op feedback.
func (s *Workspace) FormattingEditsFor(docURI uri.URI) []protocol.TextEdit {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	if doc.Parsed == nil {
		return nil
	}
	formatted := pxf.FormatDocument(doc.Parsed)
	if bytes.Equal(formatted, doc.Source) {
		return []protocol.TextEdit{}
	}
	endLine, endCharacter := positions.LineCol(doc.Source, len(doc.Source), s.positionEncoding)
	return []protocol.TextEdit{{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: endLine, Character: endCharacter},
		},
		NewText: string(formatted),
	}}
}
