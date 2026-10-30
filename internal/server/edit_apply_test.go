// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

// pos is a tiny helper for the UTF-16 position tests below —
// LSP positions are 0-indexed (line, character) pairs that
// most tests build literally.
func pos(line, char uint32) protocol.Position {
	return protocol.Position{Line: line, Character: char}
}

// offsetUTF16 is the canonical entry point exercised by these
// tests; previously they called a Server-package utf16ByteOffset
// helper that has since been removed (Z2 of the hygiene
// cluster) once positions.Offset covered the same surface.
func offsetUTF16(t *testing.T, src []byte, line, char uint32) (int, error) {
	t.Helper()
	return positions.Offset(src, pos(line, char), positions.UTF16)
}

func TestOffsetUTF16_BasicASCII(t *testing.T) {
	src := []byte("hello\nworld")
	cases := []struct {
		line, char uint32
		want       int
	}{
		{0, 0, 0},
		{0, 5, 5},  // end of "hello" (newline byte)
		{1, 0, 6},  // start of "world"
		{1, 5, 11}, // end of "world"
		{2, 0, 11}, // past EOL on last line → clamp to EOF
		{5, 0, 11}, // way past EOF → clamp
	}
	for _, c := range cases {
		got, err := offsetUTF16(t, src, c.line, c.char)
		require.NoError(t, err)
		assert.Equal(t, c.want, got, "line=%d char=%d", c.line, c.char)
	}
}

func TestOffsetUTF16_ClampsCharBeyondLine(t *testing.T) {
	src := []byte("hello\nworld")
	got, err := offsetUTF16(t, src, 0, 999)
	require.NoError(t, err)
	assert.Equal(t, 5, got, "line 0 char 999 clamps to '\\n' position")
}

func TestOffsetUTF16_SurrogatePair(t *testing.T) {
	// "😀" is U+1F600 — outside BMP → 2 UTF-16 code units, 4 UTF-8 bytes.
	src := []byte("a😀b")
	off, err := offsetUTF16(t, src, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, off)
	off, err = offsetUTF16(t, src, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, off, "after 'a' (1 byte, 1 code unit)")
	off, err = offsetUTF16(t, src, 0, 3)
	require.NoError(t, err)
	assert.Equal(t, 5, off, "after 😀 (1+2 code units → byte 5)")
	off, err = offsetUTF16(t, src, 0, 4)
	require.NoError(t, err)
	assert.Equal(t, 6, off, "after 'b' → EOF")
}

// TestOffsetUTF16_CRLFSplitsAtLF (TC8 half) — a `\r\n` line
// break advances curLine at the `\n`; the `\r` is part of the
// preceding line's content from the LSP's perspective.
func TestOffsetUTF16_CRLFSplitsAtLF(t *testing.T) {
	src := []byte("hello\r\nworld")
	off, err := offsetUTF16(t, src, 1, 0)
	require.NoError(t, err)
	assert.Equal(t, 7, off,
		"line 1 char 0 lands at byte 7 (one past the '\\n' at byte 6)")
}

// TestOffsetUTF16_BOMCountsAsOneCodeUnit (TC8 half) — the
// UTF-8 BOM (EF BB BF = U+FEFF) is one rune contributing one
// UTF-16 code unit; char=1 lands one rune past the BOM.
func TestOffsetUTF16_BOMCountsAsOneCodeUnit(t *testing.T) {
	src := []byte("\xef\xbb\xbfhello")
	off, err := offsetUTF16(t, src, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, off, "char=0 points at the BOM byte")
	off, err = offsetUTF16(t, src, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, 3, off,
		"BOM counts as one UTF-16 code unit; char=1 points at 'h'")
}

func TestApplyContentChanges_MultipleEditsAppliedInOrder(t *testing.T) {
	// Each subsequent edit applies to the state AFTER the previous.
	// 1) Insert "beautiful " at col 6 → "hello beautiful world"
	// 2) Replace "world" (now at col 16-21) with "Earth" → "hello beautiful Earth"
	src := []byte("hello world")
	changes := []IncrementalChange{
		{
			Range: &ProtocolRangeShape{
				Start: PositionShape{Line: 0, Character: 6},
				End:   PositionShape{Line: 0, Character: 6},
			},
			Text: "beautiful ",
		},
		{
			Range: &ProtocolRangeShape{
				Start: PositionShape{Line: 0, Character: 16},
				End:   PositionShape{Line: 0, Character: 21},
			},
			Text: "Earth",
		},
	}
	got, errs := ApplyContentChanges(src, changes, positions.UTF16)
	assert.Empty(t, errs)
	assert.Equal(t, "hello beautiful Earth", string(got))
}

func TestApplyContentChanges_InvertedRangeIsAnError(t *testing.T) {
	src := []byte("hello")
	changes := []IncrementalChange{{
		Range: &ProtocolRangeShape{
			Start: PositionShape{Line: 0, Character: 4},
			End:   PositionShape{Line: 0, Character: 2},
		},
		Text: "x",
	}}
	_, errs := ApplyContentChanges(src, changes, positions.UTF16)
	require.Len(t, errs, 1, "inverted range is reported as an error")
	assert.Contains(t, errs[0].Error(), "inverted range")
}

func TestUnmarshalIncrementalChanges_AbsentRangeMeansFullDocument(t *testing.T) {
	// Mirrors what a real LSP client sends for a full-document fallback:
	// { contentChanges: [{ text: "..." }] } with no range key.
	raw := json.RawMessage(`[{"text":"new content"}]`)
	changes, err := UnmarshalIncrementalChanges(raw)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	assert.Nil(t, changes[0].Range,
		"absent range key → Range field is nil (full-document fallback)")
	assert.Equal(t, "new content", changes[0].Text)
}

func TestUnmarshalIncrementalChanges_PresentRangeMeansIncremental(t *testing.T) {
	raw := json.RawMessage(`[{
		"range": {"start":{"line":0,"character":0},"end":{"line":0,"character":3}},
		"text": "xyz"
	}]`)
	changes, err := UnmarshalIncrementalChanges(raw)
	require.NoError(t, err)
	require.Len(t, changes, 1)
	require.NotNil(t, changes[0].Range)
	assert.Equal(t, uint32(0), changes[0].Range.Start.Line)
	assert.Equal(t, uint32(0), changes[0].Range.Start.Character)
	assert.Equal(t, uint32(3), changes[0].Range.End.Character)
	assert.Equal(t, "xyz", changes[0].Text)
}

// End-to-end through the dispatcher.

func TestDidChange_IncrementalEditUpdatesDocumentSource(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	const initialSrc = "@type acme.demo.v1.Greeting\n\nname = \"world\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, initialSrc)))

	// Replace "world" (line 2, chars 8-13) with "Earth".
	change := didChangeIncrementalCall(t, "file:///work/g.pxf", 2,
		protocol.Range{
			Start: protocol.Position{Line: 2, Character: 8},
			End:   protocol.Position{Line: 2, Character: 13},
		},
		"Earth")
	require.NoError(t, testHandle(srv, context.Background(), noopReply, change))

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	expected := "@type acme.demo.v1.Greeting\n\nname = \"Earth\"\n"
	assert.Equal(t, expected, string(doc.Source),
		"incremental edit applied against previous source")
}

func TestDidChange_FullDocumentFallback(t *testing.T) {
	// Even with incremental advertised, clients may send full-document
	// events (e.g. on format-on-save). The new dispatch path handles
	// them via Range == nil.
	srv, _, _ := newServerWithLookup(t)
	const initialSrc = "@type acme.demo.v1.Greeting\n\nname = \"world\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, initialSrc)))

	const newSrc = "@type acme.demo.v1.Greeting\n\nname = \"replacement\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 2, newSrc)))

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	assert.Equal(t, newSrc, string(doc.Source),
		"full-document fallback (no range) replaces source wholesale")
}

func TestDidChange_OutOfBoundsRangeIsBestEffort(t *testing.T) {
	// An out-of-bounds range shouldn't crash the server; we log
	// and leave the document in its pre-apply state for that change.
	srv, _, _ := newServerWithLookup(t)
	const initialSrc = "@type acme.demo.v1.Greeting\n\nname = \"world\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, initialSrc)))

	// Range with start > end on the same line → invalid.
	change := didChangeIncrementalCall(t, "file:///work/g.pxf", 2,
		protocol.Range{
			Start: protocol.Position{Line: 2, Character: 10},
			End:   protocol.Position{Line: 2, Character: 5},
		},
		"oops")
	require.NoError(t, testHandle(srv, context.Background(), noopReply, change),
		"out-of-bounds range is logged + skipped; dispatcher still returns nil")

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	assert.Equal(t, initialSrc, string(doc.Source),
		"invalid range → source unchanged (the bad edit was skipped)")
}
