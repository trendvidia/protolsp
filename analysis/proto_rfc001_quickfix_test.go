// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
)

// rfc001QuickFixAnalyze opens src as root/rules.proto, runs the analyze
// pipeline, and returns the document and its "protolsp/proto"
// diagnostics.
func rfc001QuickFixAnalyze(t *testing.T, w *Workspace, root, src string) (*Document, []protocol.Diagnostic) {
	t.Helper()
	doc := &Document{URI: uri.File(filepath.Join(root, "rules.proto")), Source: []byte(src)}
	w.Documents().Open(doc)
	w.Analyze(doc)
	var out []protocol.Diagnostic
	for _, d := range doc.ProtoDiagnostics {
		if optText(d.Source) == "protolsp/proto" {
			out = append(out, d)
		}
	}
	return doc, out
}

// rfc001QuickFixProto wraps one field-level rule in a schema declaring
// `validate` and three functions.
//
// Line map (0-indexed): 11 is the field line.
func rfc001QuickFixProto(rule string) string {
	return `syntax = "proto3";

package t;

annotation validate(rule: expression);

function matches(value: string, pattern: string);
function matched(value: string);
function is_email(value: string);

message M {
  string email = 1 @validate(` + rule + `);
}
`
}

const rfc001QuickFixFieldLine = 11

// rfc001QuickFixRange is the UTF-16 range of needle on line of src.
func rfc001QuickFixRange(t *testing.T, src string, line int, needle string) protocol.Range {
	t.Helper()
	text := strings.Split(src, "\n")[line]
	col := strings.Index(text, needle)
	require.GreaterOrEqual(t, col, 0, "needle %q on line %d", needle, line)
	start := len(utf16.Encode([]rune(text[:col])))
	end := start + len(utf16.Encode([]rune(needle)))
	return protocol.Range{
		Start: protocol.Position{Line: uint32(line), Character: uint32(start)}, //nolint:gosec
		End:   protocol.Position{Line: uint32(line), Character: uint32(end)},   //nolint:gosec
	}
}

// The compiler rejects the misspelling (RFC-001 §5.4, protocompile#211)
// with the callee name as its primary span; protolsp adds the nearest
// declared functions. This is also the pin on the compiler's message
// prefix the decoration keys on.
func TestRFC001QuickFix_TypoGetsCandidates(t *testing.T) {
	w := New(nil)
	src := rfc001QuickFixProto(`mathces(this, "^[^@]+@[^@]+$")`)
	_, diags := rfc001QuickFixAnalyze(t, w, t.TempDir(), src)

	require.Len(t, diags, 1)
	d := diags[0]
	assert.Equal(t, rfc001QuickFixRange(t, src, rfc001QuickFixFieldLine, "mathces"), d.Range,
		"the compiler anchors the diagnostic on the callee name")
	assert.True(t, strings.HasPrefix(MessageText(d.Message), "unknown function `mathces`"), MessageText(d.Message))
	assert.True(t, strings.HasSuffix(MessageText(d.Message), `; did you mean "matches"?`), MessageText(d.Message))
	data := DecodeUnresolvedNameData(d.Data)
	require.NotNil(t, data, "Data carries the typed payload")
	assert.Equal(t, DiagDataKindUnresolvedName, data.Kind)
	assert.Equal(t, "mathces", data.Name)
	assert.Equal(t, []string{"matches"}, data.Candidates,
		"`matched` is three edits away and not suggested; `matches` is a transposition")
}

// A qualified misspelling: the compiler's span and name cover the whole
// written callee; the candidate is spelled bare, since it lives in the
// document's own package.
func TestRFC001QuickFix_QualifiedTypo(t *testing.T) {
	w := New(nil)
	src := rfc001QuickFixProto(`t.mathces(this, "^a")`)
	_, diags := rfc001QuickFixAnalyze(t, w, t.TempDir(), src)
	require.Len(t, diags, 1)
	assert.Equal(t, rfc001QuickFixRange(t, src, rfc001QuickFixFieldLine, "t.mathces"), diags[0].Range)
	data := DecodeUnresolvedNameData(diags[0].Data)
	require.NotNil(t, data)
	assert.Equal(t, "t.mathces", data.Name)
	assert.Equal(t, []string{"matches"}, data.Candidates)
}

// Diagnostics the decoration must leave alone: a declared function's
// arity (protocompile's finding, no candidates), an unknown method on
// `this` (never a declared function), and a syntax error.
func TestRFC001QuickFix_OtherDiagnosticsUntouched(t *testing.T) {
	w := New(nil)
	for _, rule := range []string{
		`matches(this)`,
		`this.is_email()`,
		`this >= && this < 150`,
	} {
		t.Run(rule, func(t *testing.T) {
			_, diags := rfc001QuickFixAnalyze(t, w, t.TempDir(), rfc001QuickFixProto(rule))
			require.NotEmpty(t, diags)
			for _, d := range diags {
				assert.Nil(t, d.Data, "%s: %s", rule, d.Message)
				assert.NotContains(t, MessageText(d.Message), "did you mean")
			}
		})
	}
}

// Nothing close enough: no hint, no payload — the compiler's message
// stands alone.
func TestRFC001QuickFix_NoCandidateNoDecoration(t *testing.T) {
	w := New(nil)
	_, diags := rfc001QuickFixAnalyze(t, w, t.TempDir(), rfc001QuickFixProto(`frobnicate(this)`))
	require.Len(t, diags, 1)
	assert.Nil(t, diags[0].Data)
	assert.NotContains(t, MessageText(diags[0].Message), "did you mean")
}

// A function declared in an imported file of another package is
// suggested fully qualified — the spelling that resolves from here.
// The bare-name near-miss (`is_email` written where only
// `lib.is_email` resolves) is the distance-zero case.
func TestRFC001QuickFix_ImportedCandidateIsQualified(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "lib.proto"), []byte(`syntax = "proto3";

package lib;

function is_email(value: string);
`), 0o600))
	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(root))
	w.LoadProtoIndex()
	src := `syntax = "proto3";

package t;

import "lib.proto";

annotation validate(rule: expression);

message M {
  string email = 1 @validate(is_email(this));
}
`
	_, diags := rfc001QuickFixAnalyze(t, w, root, src)
	require.Len(t, diags, 1, "the import resolves through the index; only the call is unresolved")
	assert.Equal(t, rfc001QuickFixRange(t, src, 9, "is_email"), diags[0].Range)
	assert.Equal(t, []string{"lib.is_email"}, DecodeUnresolvedNameData(diags[0].Data).Candidates)
}

func TestCodeActions_UnresolvedNameReplace(t *testing.T) {
	w := New(nil)
	src := rfc001QuickFixProto(`mathces(this, "^a")`)
	doc, diags := rfc001QuickFixAnalyze(t, w, t.TempDir(), src)
	require.Len(t, diags, 1)

	// Data is raw JSON, which is how the LSP client returns it.
	diag := diags[0]

	doc.mu.RLock()
	actions := w.CodeActionsFor(doc, []protocol.Diagnostic{diag})
	doc.mu.RUnlock()
	require.Len(t, actions, 1)
	a := actions[0]
	assert.Equal(t, `Replace "mathces" with "matches"`, a.Title)
	assert.Equal(t, protocol.CodeActionKindQuickFix, *a.Kind)
	assert.True(t, isTrue(a.IsPreferred))
	edits := a.Edit.Changes[doc.URI]
	require.Len(t, edits, 1)
	assert.Equal(t, diags[0].Range, edits[0].Range)

	rng := edits[0].Range
	fixed, errs := ApplyContentChanges(doc.Source, []IncrementalChange{{
		Range: &ProtocolRangeShape{
			Start: PositionShape{Line: rng.Start.Line, Character: rng.Start.Character},
			End:   PositionShape{Line: rng.End.Line, Character: rng.End.Character},
		},
		Text: edits[0].NewText,
	}}, positions.UTF16)
	require.Empty(t, errs)
	assert.Contains(t, string(fixed), `@validate(matches(this, "^a"))`)
}

func TestDecodeUnresolvedNameData_RejectsOtherKinds(t *testing.T) {
	assert.Nil(t, DecodeUnresolvedNameData(nil))
	assert.Nil(t, DecodeUnresolvedNameData(map[string]interface{}{"kind": DiagDataKindUnknownField}))
	assert.Nil(t, DecodeUnresolvedNameData(UnknownFieldData{Kind: DiagDataKindUnknownField}))
	got := DecodeUnresolvedNameData(&UnresolvedNameData{Kind: DiagDataKindUnresolvedName, Name: "x"})
	require.NotNil(t, got)
	assert.Equal(t, "x", got.Name)
}

func TestRFC001UnresolvedFunctionName(t *testing.T) {
	name, ok := rfc001UnresolvedFunctionName("unknown function `t.mathces` in argument \"rule\" for `t.validate`: neither a visible `function` declaration nor a builtin")
	require.True(t, ok)
	assert.Equal(t, "t.mathces", name)
	_, ok = rfc001UnresolvedFunctionName("unknown method `foo` on `this` in argument \"rule\" for `t.validate`")
	assert.False(t, ok)
	_, ok = rfc001UnresolvedFunctionName("unknown function ``")
	assert.False(t, ok)
}

func TestRFC001NearestFunctions(t *testing.T) {
	funcs := []rfc001DeclaredFunction{
		{bare: "matches", written: "matches"},
		{bare: "matched", written: "matched"},
		{bare: "is_email", written: "lib.is_email"},
		{bare: "is_e164", written: "lib.is_e164"},
		{bare: "contains", written: "contains"},
		{bare: "now", written: "now"},
	}
	assert.Equal(t, []string{"matches"}, rfc001NearestFunctions("mathces", funcs),
		"a transposition is two edits; `matched` is three")
	assert.Equal(t, []string{"matches", "matched"}, rfc001NearestFunctions("t.Matches", funcs),
		"closest first; the last segment is compared")
	assert.Equal(t, []string{"matched", "matches"}, rfc001NearestFunctions("matchez", funcs),
		"ties by name")
	assert.Equal(t, []string{"lib.is_email"}, rfc001NearestFunctions("is_email", funcs),
		"an exact bare match that resolves nowhere suggests the qualified spelling")
	assert.Empty(t, rfc001NearestFunctions("frobnicate", funcs))
	assert.Equal(t, []string{"now"}, rfc001NearestFunctions("nw", funcs))
	assert.Empty(t, rfc001NearestFunctions("nwo", funcs), "short names get a one-edit limit")
}

func TestEditDistance(t *testing.T) {
	assert.Equal(t, 0, editDistance("", ""))
	assert.Equal(t, 3, editDistance("", "abc"))
	assert.Equal(t, 2, editDistance("mathces", "matches"))
	assert.Equal(t, 2, editDistance("thsi", "this"), "a transposition costs two edits")
	assert.Equal(t, 1, editDistance("héllo", "hello"), "counted in code points")
}
