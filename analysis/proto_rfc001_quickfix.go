// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Quick-fix for unresolved calls in PSE expression arguments (#276).
//
// Since RFC-001 §5.4 (protowire#282) the `@validate` expression
// language is normative and protocompile parses every expression
// argument against it (protocompile#211): a call that resolves to
// neither a visible `function` declaration nor a §5.4 builtin is a
// compile error whose primary span is the callee name as written. The
// compile pass already surfaces that as a "protolsp/proto" diagnostic;
// this file adds what only the editor can: the nearest declared
// functions as replacement candidates, carried in Diagnostic.Data and
// rendered by codeaction.go as `Replace "mathces" with "matches"`.
//
// The coupling to the compiler is its message prefix, "unknown
// function `<name>`"; TestRFC001QuickFix_TypoGetsCandidates compiles a
// misspelling through the real compiler and fails the day that prefix
// moves.

package analysis

import (
	"fmt"
	"sort"
	"strings"

	"github.com/trendvidia/protocompile/ir"
	"github.com/trendvidia/protocompile/seq"
	"go.lsp.dev/protocol"
)

// rfc001UnknownFunctionPrefix opens protocompile's diagnostic for a
// call that is neither declared nor a builtin; the callee as written
// follows, in backticks.
const rfc001UnknownFunctionPrefix = "unknown function `"

// DiagDataKindUnresolvedName tags Diagnostic.Data on an unresolved
// engine-expression name so the code-action handler can offer the
// nearest declared functions as replacements.
const DiagDataKindUnresolvedName = "unresolved_expression_name"

// UnresolvedNameData is the Diagnostic.Data payload for an
// unresolved name inside an engine expression.
type UnresolvedNameData struct {
	Kind string `json:"kind"`
	// Name is the reference as written — the qualified callee of a
	// call (`myco.is_emial`) or the bare identifier.
	Name string `json:"name"`
	// Candidates are the declared functions nearest to Name, spelled
	// the way they resolve from the document (bare for same-package
	// functions, fully qualified otherwise), closest first. Empty
	// when nothing is close enough to suggest.
	Candidates []string `json:"candidates,omitempty"`
}

// rfc001UnresolvedFunctionName extracts the callee from protocompile's
// unknown-function diagnostic message, or reports false for any other
// message.
func rfc001UnresolvedFunctionName(msg string) (string, bool) {
	rest, ok := strings.CutPrefix(msg, rfc001UnknownFunctionPrefix)
	if !ok {
		return "", false
	}
	name, _, ok := strings.Cut(rest, "`")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// rfc001AttachQuickFix decorates a compile diagnostic for an unresolved
// call with the nearest declared functions: a "did you mean" hint on
// the message and an UnresolvedNameData payload for the code action.
// funcs is evaluated lazily, once per analyze pass, and only when a
// diagnostic needs it. No-op for every other diagnostic and when
// nothing is close enough to suggest.
func rfc001AttachQuickFix(d *protocol.Diagnostic, funcs func() []rfc001DeclaredFunction) {
	name, ok := rfc001UnresolvedFunctionName(d.Message)
	if !ok {
		return
	}
	near := rfc001NearestFunctions(name, funcs())
	if len(near) == 0 {
		return
	}
	d.Message += fmt.Sprintf("; did you mean %q?", near[0])
	d.Data = UnresolvedNameData{Kind: DiagDataKindUnresolvedName, Name: name, Candidates: near}
}

// rfc001DeclaredFunction is a function declaration visible from the
// document, with the spelling that resolves from it.
type rfc001DeclaredFunction struct {
	bare    string
	written string
}

// rfc001VisibleFunctions lists the function declarations the
// document's expressions can name: its own and those of visible
// imports (direct, or public through a direct one). Same-package
// functions resolve bare; anything else needs its full name.
func rfc001VisibleFunctions(irFile *ir.File) []rfc001DeclaredFunction {
	pkg := irFile.Package()
	var out []rfc001DeclaredFunction
	add := func(f *ir.File) {
		for fn := range seq.Values(f.Functions()) {
			written := fn.Name()
			if f.Package() != pkg {
				written = string(fn.FullName())
			}
			out = append(out, rfc001DeclaredFunction{bare: fn.Name(), written: written})
		}
	}
	add(irFile)
	for imp := range seq.Values(irFile.TransitiveImports()) {
		if imp.Visible && imp.File != nil {
			add(imp.File)
		}
	}
	return out
}

// rfc001NearestFunctions ranks the declared functions whose bare
// name is within a small edit distance of the written callee's last
// segment, closest first, at most three. Distance zero is a hit too:
// `matches(this)` when only `myco.commons.matches` is declared
// resolves nowhere, and the qualified spelling is the fix.
func rfc001NearestFunctions(name string, funcs []rfc001DeclaredFunction) []string {
	last := name[strings.LastIndexByte(name, '.')+1:]
	limit := rfc001SuggestLimit(last)
	type scored struct {
		written string
		d       int
	}
	var hits []scored
	seen := map[string]bool{}
	for _, f := range funcs {
		if f.written == name || seen[f.written] {
			continue
		}
		if d := editDistance(last, f.bare); d <= limit {
			seen[f.written] = true
			hits = append(hits, scored{f.written, d})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].d != hits[j].d {
			return hits[i].d < hits[j].d
		}
		return hits[i].written < hits[j].written
	})
	out := make([]string, 0, min(3, len(hits)))
	for _, h := range hits[:min(3, len(hits))] {
		out = append(out, h.written)
	}
	return out
}

// rfc001SuggestLimit is how far a written name may be from a
// declared one to be suggested: two edits (a transposition —
// `mathces` — costs two), one for names too short for two edits to
// mean anything.
func rfc001SuggestLimit(name string) int {
	if len(name) < 4 {
		return 1
	}
	return 2
}

// editDistance is the Levenshtein distance between two strings,
// counted in code points.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
