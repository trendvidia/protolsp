// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package interp matches PXF's `${path}` interpolation syntax
// in a single place so the workspaceconfig parser, the LSP
// analyze pass, and any future consumer don't drift on what
// counts as a reference.
//
// The scanner is policy-free: it returns every `${path}` match
// it finds, leaving each caller to decide which paths it
// resolves. workspaceconfig.interpolate filters to single-
// segment env refs (`env.NAME`) and substitutes via os.Getenv;
// the server's field-reference analyze pass keeps every match
// and classifies them downstream (env / ctx / field).
//
// Path syntax: one or more dot-separated identifier segments.
// Each segment matches `[A-Za-z_][A-Za-z0-9_]*` — the same
// shape protowire's identifier lexer accepts. No quoting, no
// brackets, no defaults inside `${...}` for now; if the spec
// adds `${path:default}` shorthand we extend here.

package interp

import (
	"regexp"
	"strings"
)

// refPattern matches one `${path}` interpolation reference.
// Capture group 1 holds the dotted path (delimiters stripped).
var refPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\}`)

// Ref is one `${path}` occurrence found in a source string.
// Start / End are byte offsets into the scanned string covering
// the full reference (`${` through `}`).
type Ref struct {
	Path  string
	Start int
	End   int
}

// FindAll returns every `${path}` reference in s, in source
// order. Returns nil (not an empty slice) when there are no
// matches — lets callers cheaply skip downstream work.
func FindAll(s string) []Ref {
	// Cheap reject before invoking the regex.
	if !strings.Contains(s, "${") {
		return nil
	}
	matches := refPattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Ref, 0, len(matches))
	for _, m := range matches {
		// m[0..1] = full-match offsets; m[2..3] = group-1 offsets.
		out = append(out, Ref{
			Path:  s[m[2]:m[3]],
			Start: m[0],
			End:   m[1],
		})
	}
	return out
}

// ReplaceAll substitutes each `${path}` reference using fn. When
// fn returns `(_, false)` the original `${...}` text is kept
// verbatim — useful for "resolve some kinds, leave others
// literal" semantics (e.g. resolve env refs while leaving field
// refs alone for downstream interpretation).
func ReplaceAll(s string, fn func(path string) (string, bool)) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return refPattern.ReplaceAllStringFunc(s, func(match string) string {
		path := refPattern.FindStringSubmatch(match)[1]
		if v, ok := fn(path); ok {
			return v
		}
		return match
	})
}
