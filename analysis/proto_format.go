// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/ast/printer"
	"github.com/trendvidia/protocompile/parser"
	"github.com/trendvidia/protocompile/report"
	"github.com/trendvidia/protocompile/source"
	"github.com/trendvidia/protocompile/token"
)

// formatProto renders .proto source in canonical form with
// protocompile's printer. The Legacy preset is the one protocompile
// holds byte-compatible with `buf format`, so a file buf could
// already format keeps its layout; unlike buf, the parser reads
// v1.2 (RFC-001) syntax: annotation uses, `type`, `function`.
//
// Errors when the source has a parse error (the user is mid-edit;
// rewriting from a partial AST would lose their text) and when the
// output does not carry every token and comment word of the input.
// The second check is a guard, not a formatting rule: the printer
// has dropped text before (annotation uses, protocompile#258;
// comments, protocompile#273), and refusing to format is always
// cheaper than deleting part of the user's file on save.
func formatProto(path string, src []byte) ([]byte, error) {
	in, err := parseProtoStrict(path, string(src))
	if err != nil {
		return nil, err
	}
	out, err := printProto(in)
	if err != nil {
		return nil, fmt.Errorf("printing: %w", err)
	}
	printed, err := parseProtoStrict(path, out)
	if err != nil {
		return nil, fmt.Errorf("formatted output does not parse: %w", err)
	}
	if lost, added := diffContent(contentOf(in.Stream()), contentOf(printed.Stream())); len(lost)+len(added) > 0 {
		return nil, fmt.Errorf("formatter would change content (lost %q, added %q); not formatting", lost, added)
	}
	return []byte(out), nil
}

// printProto is the printer call, a variable so a test can swap in
// one that loses text and check the guard catches it.
var printProto = func(f *ast.File) (string, error) {
	return printer.PrintFile(printer.Options{Format: true, Formatting: printer.Legacy()}, f)
}

// parseProtoStrict parses text and fails on the first error-level
// diagnostic.
func parseProtoStrict(path, text string) (*ast.File, error) {
	var rep report.Report
	f, _ := parser.Parse(path, source.NewFile(path, text), &rep)
	for _, d := range rep.Diagnostics {
		if d.Level() == report.Error || d.Level() == report.ICE {
			return nil, fmt.Errorf("parse error: %s", d.Message())
		}
	}
	if f == nil {
		return nil, fmt.Errorf("parse produced no file")
	}
	return f, nil
}

// contentOf counts what formatting must preserve in a token stream:
// the text of every token that has a letter or digit in it, and
// every word of every comment. Punctuation is left out because the
// Legacy preset rewrites it the way buf does (`<...>` literals
// become `{...}`, `foo {` becomes `foo: {`, separators come and go,
// a byte-order mark is dropped). Comment markers are left out
// because it also rewrites `//` comments to `/* */`, escaping a `*/`
// inside one as `* /`.
func contentOf(stream *token.Stream) map[string]int {
	counts := make(map[string]int)
	for tok := range stream.All() {
		if tok.IsSynthetic() {
			continue
		}
		text := tok.Text()
		switch tok.Kind() {
		case token.Space:
		case token.Comment:
			for _, word := range strings.Fields(text) {
				if strings.Trim(word, "/*") != "" {
					counts["comment:"+strings.Trim(word, "/*")]++
				}
			}
		default:
			if strings.IndexFunc(text, isWordRune) >= 0 {
				counts[text]++
			}
		}
	}
	return counts
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// diffContent returns the keys whose count dropped from before to
// after, and those whose count grew, each sorted.
func diffContent(before, after map[string]int) (lost, added []string) {
	for _, k := range slices.Sorted(maps.Keys(before)) {
		if before[k] > after[k] {
			lost = append(lost, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(after)) {
		if after[k] > before[k] {
			added = append(added, k)
		}
	}
	return lost, added
}
