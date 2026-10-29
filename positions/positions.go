// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package positions converts between LSP positions and byte
// offsets in source bytes, in either of the position encodings
// the LSP spec allows: UTF-8 (LSP 3.17+) or UTF-16 (the
// pre-3.17 default).
//
// Why this package exists: LSP positions are
// (line, character) pairs where the unit of `character` depends
// on the negotiated `positionEncoding`. The default is UTF-16
// code units — every code point in the BMP counts as 1, every
// supplementary-plane code point (emoji, etc.) counts as 2.
// Modern clients negotiate UTF-8, which makes `character` a
// byte offset within the line; positions become a constant-time
// index into the source.
//
// All helpers here take a `src []byte` and an `Encoding`. The
// server's negotiated encoding is stored once and passed through
// every conversion site — that's what makes the UTF-8 path
// actually faster: no per-call branch is needed at the
// implementation level beyond the encoding dispatch.
//
// The package only depends on the LSP protocol's `Position` and
// `Range` types — no internal-server types — so it's reusable
// across cmd/ and tests without import cycles.
package positions

import (
	"fmt"
	"unicode/utf8"

	"go.lsp.dev/protocol"
)

// Encoding names the unit of LSP `Position.character`. The two
// realistic encodings appear; UTF-32 is in the LSP spec but no
// client we've seen advertises it, so we don't either.
type Encoding int

const (
	// UTF16 is the LSP <3.17 default: Position.character counts
	// UTF-16 code units. Characters outside the BMP (code point
	// > U+FFFF) consume two code units.
	UTF16 Encoding = iota
	// UTF8 is the LSP 3.17 alternative: Position.character is
	// the byte offset within the line. Faster on ASCII-heavy
	// sources and avoids surrogate-pair bookkeeping entirely.
	UTF8
)

// String renders an Encoding as the LSP capability string —
// matches the wire format the client + server exchange in
// initialize. Useful for logs + advertising in the server's
// initialize result.
func (e Encoding) String() string {
	switch e {
	case UTF8:
		return "utf-8"
	case UTF16:
		return "utf-16"
	}
	return "utf-16"
}

// Negotiate picks the best encoding the server supports from the
// client's advertised list. The client sends its list in
// `params.Capabilities.General.PositionEncodings`. We prefer
// UTF-8 when the client lists it (constant-time lookups; native
// to our source bytes); otherwise UTF-16 (the LSP default that
// EVERY client supports).
//
// Empty list / nil input → UTF-16 (the pre-3.17 default behavior
// every client implements implicitly).
func Negotiate(clientEncodings []string) Encoding {
	for _, e := range clientEncodings {
		if e == "utf-8" {
			return UTF8
		}
	}
	return UTF16
}

// Offset converts an LSP Position to a byte index into src.
// Out-of-range positions clamp gracefully:
//
//   - Line beyond doc → returns len(src)
//   - Character beyond end-of-line → returns the byte offset
//     of the line's newline (or len(src) on the last line)
//
// Returns an error only on invalid UTF-8 in src — LSP documents
// are UTF-8 on the wire, so a failure here means upstream
// corruption.
func Offset(src []byte, p protocol.Position, enc Encoding) (int, error) {
	lineStart := LineStart(src, p.Line)
	if lineStart < 0 {
		return len(src), nil
	}
	if enc == UTF8 {
		return offsetUTF8(src, lineStart, p.Character)
	}
	return offsetUTF16(src, lineStart, p.Character)
}

// LineCol is the inverse of Offset: given a byte index, return
// the (line, character) pair in the negotiated encoding. The
// line component is always the count of `\n` bytes preceding
// the offset; the character component depends on encoding.
//
// Out-of-range offsets clamp to the end of source.
func LineCol(src []byte, byteOffset int, enc Encoding) (line, character uint32) {
	if byteOffset < 0 {
		byteOffset = 0
	}
	if byteOffset > len(src) {
		byteOffset = len(src)
	}
	// Find the line that contains byteOffset.
	lastNL := -1
	for i := 0; i < byteOffset; i++ {
		if src[i] == '\n' {
			line++
			lastNL = i
		}
	}
	colByte := byteOffset - (lastNL + 1)
	if enc == UTF8 {
		return line, uint32(colByte) // #nosec G115 -- bounded by source length
	}
	// UTF-16: count code units from line start to byteOffset.
	character = countUTF16(src[lastNL+1 : byteOffset])
	return line, character
}

// LineStart returns the byte offset of the first byte of line N
// (0-indexed). Returns -1 when the line is past EOF, len(src)
// when the line is exactly the line past the trailing newline
// (the "virtual" final line LSP allows positions on).
//
// Encoding-agnostic: line boundaries are `\n` bytes regardless
// of how characters within a line are counted.
func LineStart(src []byte, line uint32) int {
	if line == 0 {
		return 0
	}
	var cur uint32
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			cur++
			if cur == line {
				return i + 1
			}
		}
	}
	// Past EOF. Return len(src) so callers can clamp end-of-doc
	// positions cleanly rather than handling a sentinel.
	return len(src)
}

// offsetUTF8 is the fast path: the LSP character index IS the
// byte offset within the line. Clamp order matters: a character
// past the line's newline must land ON the newline (not past it
// into the next line's text), and a character past EOF must
// land at len(src). The loop handles both by scanning for `\n`
// within the [lineStart, target) range — the first newline
// wins.
func offsetUTF8(src []byte, lineStart int, character uint32) (int, error) {
	target := lineStart + int(character) // #nosec G115 -- character is bounded by line length
	if target > len(src) {
		target = len(src)
	}
	for i := lineStart; i < target; i++ {
		if src[i] == '\n' {
			return i, nil
		}
	}
	return target, nil
}

// offsetUTF16 walks the line counting UTF-16 code units until
// we've counted `character` of them or hit the line's newline /
// EOF. Each non-BMP code point contributes 2 code units (the
// surrogate-pair size); BMP characters contribute 1.
//
// Returns an error on invalid UTF-8 in src. The LSP convention
// is that source is UTF-8 on the wire; an invalid sequence
// indicates upstream corruption.
func offsetUTF16(src []byte, lineStart int, character uint32) (int, error) {
	var codeUnits uint32
	i := lineStart
	for i < len(src) && codeUnits < character {
		if src[i] == '\n' {
			return i, nil
		}
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			return -1, fmt.Errorf("invalid UTF-8 at byte %d", i)
		}
		if r > 0xFFFF {
			codeUnits += 2
		} else {
			codeUnits++
		}
		i += size
	}
	return i, nil
}

// countUTF16 returns the number of UTF-16 code units in src.
// Used by LineCol when the negotiated encoding is UTF-16 to
// convert a byte span back to a character count.
func countUTF16(src []byte) uint32 {
	var n uint32
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8 — count the byte as one unit; the
			// caller's source is suspect but we don't want to
			// panic on position math.
			n++
			i++
			continue
		}
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
		i += size
	}
	return n
}
