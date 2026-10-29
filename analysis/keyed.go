// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Keyed repeated fields (draft-trendvidia-protowire-01 §3.13,
// trendvidia/protowire#116): a `repeated <Message>` field carrying the
// (pxf.key) option is written as a block of named blocks — entry name =
// key-field value, document order = list order. This file holds the
// small primitives the editor surfaces share when they need to treat a
// keyed field's block specially: detecting a keyed field, pulling the
// entries out of its block, the canonical identifier-safe rule for
// deciding whether a rewritten key needs quoting, and the on-source
// span of an entry's name token.
//
// The schema-layer machinery (typed decode errors, the tolerant
// diagnostics walker, canonicalization) lives in protowire-go's
// encoding/pxf; the descriptor helpers we build on are pxf.KeyField /
// pxf.IsKeyed / pxf.KeyFieldName.
package analysis

import (
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// keyedField returns the field descriptor of the keyed repeated field
// named `name` on md, or nil when md has no such field or the field is
// not keyed. Thin wrapper over pxf.KeyField that also does the by-name
// lookup the surfaces all need.
func keyedField(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	if md == nil {
		return nil
	}
	fd := md.Fields().ByName(protoreflect.Name(name))
	if pxf.KeyField(fd) != nil {
		return fd
	}
	return nil
}

// keyedEntry is one entry of a keyed repeated field's block: its key
// (the entry name, always the denoted/unquoted value), whether the
// source spelled the name as a quoted string, the entry body (bound to
// the element message), and the AST position of the name token.
type keyedEntry struct {
	key    string
	quoted bool
	body   []pxf.Entry
	pos    pxf.Position
}

// asKeyedEntry interprets one child of a keyed block as an entry. A
// keyed block's direct children are entries in one of two surface
// spellings: `key { ... }` (a Block whose Name is the key) or the
// `key = { ... }` form (an Assignment whose value is a BlockVal). Any
// other shape is a malformed entry the schema validator reports
// elsewhere; ok is false for those.
func asKeyedEntry(e pxf.Entry) (keyedEntry, bool) {
	switch n := e.(type) {
	case *pxf.Block:
		return keyedEntry{key: n.Name, quoted: n.NameQuoted, body: n.Entries, pos: n.Pos}, true
	case *pxf.Assignment:
		if bv, ok := n.Value.(*pxf.BlockVal); ok {
			return keyedEntry{key: n.Key, quoted: n.KeyQuoted, body: bv.Entries, pos: n.Pos}, true
		}
	}
	return keyedEntry{}, false
}

// identSafeEntryName reports whether s can be written as an unquoted
// keyed entry name. It mirrors protowire-go's canonical rule (draft -01
// §3.13): s must match the identifier production (ident-start byte then
// ident-part bytes, dots allowed) and must not be a value keyword
// (null / true / false). Callers deciding how to spell a rewritten key
// quote it exactly when this is false, matching `pxf fmt` — so LSP
// output never disagrees with the formatter.
func identSafeEntryName(s string) bool {
	if s == "" || s == "true" || s == "false" || s == "null" {
		return false
	}
	if !isKeyIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isKeyIdentPart(s[i]) {
			return false
		}
	}
	return true
}

func isKeyIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isKeyIdentPart(c byte) bool {
	return isKeyIdentStart(c) || (c >= '0' && c <= '9') || c == '.'
}

// nameTokenEnd returns the byte offset just past the entry-name token
// that begins at startOff in src. For a bare identifier it consumes the
// identifier bytes; for a quoted name it consumes the whole string
// literal including its closing quote (honoring backslash escapes).
// Returns -1 when the token can't be delimited (out-of-range offset, a
// quoted flag that doesn't find an opening quote, or an empty bare
// span).
func nameTokenEnd(src []byte, startOff int, quoted bool) int {
	if startOff < 0 || startOff >= len(src) {
		return -1
	}
	if !quoted {
		i := startOff
		for i < len(src) && isKeyIdentPart(src[i]) {
			i++
		}
		if i == startOff {
			return -1
		}
		return i
	}
	if src[startOff] != '"' {
		return -1
	}
	for i := startOff + 1; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++ // skip the escaped byte
		case '"':
			return i + 1
		}
	}
	return -1
}
