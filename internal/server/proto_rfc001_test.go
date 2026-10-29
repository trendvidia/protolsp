// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// rfc001Proto exercises the protowire RFC-001 schema extensions:
// parameterless + one-arg annotation declarations, a leading
// annotation use on a message, and a trailing use on a field.
// Mirrors trendvidia/protocompile's internal annotations/basic.proto
// fixture. protoc rejects this file at the `annotation` keyword;
// trendvidia/protocompile parses and lowers it — the whole point of
// the fork swap is that editors stop seeing spurious errors here.
const rfc001Proto = `syntax = "proto3";

package test.annotations.basic;

annotation required;
annotation description(text: string);

@description("primary email")
message Email {
  string value = 1 @description("RFC-5322 address");
}
`

func TestDidOpen_RFC001GrammarPublishesEmptyDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/rfc001.proto", 1, rfc001Proto)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Equal(t, protocol.MethodTextDocumentPublishDiagnostics, notes[0].method)
	assert.Empty(t, notes[0].params.Diagnostics,
		"RFC-001 grammar (annotation decls + @uses) must not produce spurious editor errors")

	doc := srv.Documents().Get("file:///work/rfc001.proto")
	require.NotNil(t, doc)
	assert.Empty(t, doc.ProtoDiagnostics)
}

// The remaining RFC-001 keywords — `type` aliases and `function`
// declarations — must parse just as cleanly as the annotation forms
// above.
func TestDidOpen_RFC001TypeAndFunctionDeclsPublishEmptyDiagnostics(t *testing.T) {
	const src = `syntax = "proto3";

package test.rfc001.decls;

type UserID = string;
function is_email(s: string);
`
	srv, fc := newServerWithFakeConn(t)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/decls.proto", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, notes[0].params.Diagnostics,
		"type/function declarations must not produce spurious editor errors")
}

// Pins the diagnostics the compile pipeline (queries.IR) emits today
// for malformed RFC-001 constructs: each case asserts
// the message wording and — because the report-native pipeline
// carries full start..end spans — that the squiggle covers exactly
// the offending token. If a protocompile bump rewords or repositions
// these, this test is the tripwire to re-review the UX rather than
// silently shipping the new text.
//
// The pipeline may emit the same finding more than once (one per
// query that re-parses the file), so cases assert "at least one
// diagnostic matches" instead of pinning counts.
func TestDidOpen_RFC001MalformedConstructDiagnostics(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantMsg string         // substring of the diagnostic message
		wantRng protocol.Range // exact range of the offending token
	}{
		{
			name: "type decl missing aliased value",
			src: `syntax = "proto3";
package t;
type Foo;
`,
			wantMsg: "unexpected `;` in `type`",
			wantRng: rng(2, 8, 2, 9),
		},
		{
			name: "function param missing colon",
			src: `syntax = "proto3";
package t;
function f(x string);
`,
			wantMsg: "unexpected `string` in `function`",
			wantRng: rng(2, 13, 2, 19),
		},
		{
			name: "use of undeclared annotation",
			src: `syntax = "proto3";
package t;
@nope
message M {}
`,
			wantMsg: "cannot find `nope` in this scope",
			wantRng: rng(2, 1, 2, 5),
		},
		{
			name: "use argument type mismatch",
			src: `syntax = "proto3";
package t;
annotation d(text: string);
@d(42)
message M {}
`,
			wantMsg: "argument \"text\" for `t.d` expects string, got number literal",
			wantRng: rng(3, 3, 3, 5),
		},
		{
			// protocompile v0.17.0: a parameter declared
			// without a default is required, and an unbound one now
			// diagnoses at the whole use site. Bare `@d`, `@d()`, and
			// partial argument lists all fire; this pins the bare form.
			name: "use missing required argument",
			src: `syntax = "proto3";
package t;
annotation d(text: string);
@d
message M {}
`,
			wantMsg: "missing required argument \"text\" for `t.d`",
			wantRng: rng(3, 0, 3, 2),
		},
		{
			// Empty parens are the form protocompile#93 reported as
			// passing silently pre-v0.17.0 — pinned separately from
			// the bare form so a regression on either shape trips.
			name: "use missing required argument with empty parens",
			src: `syntax = "proto3";
package t;
annotation d(text: string);
@d()
message M {}
`,
			wantMsg: "missing required argument \"text\" for `t.d`",
			wantRng: rng(3, 0, 3, 4),
		},
		{
			// Arg-count mismatch carries both counts (#220 acceptance
			// criterion). The snippet covers the whole use site.
			name: "use with too many arguments",
			src: `syntax = "proto3";
package t;
annotation d(text: string);
@d("a", "b")
message M {}
`,
			wantMsg: "too many arguments for `t.d`: got 2, want at most 1",
			wantRng: rng(3, 0, 3, 12),
		},
		{
			name: "use with unclosed argument list",
			src: `syntax = "proto3";
package t;
annotation d(text: string);
@d("x"
message M {}
`,
			wantMsg: "encountered unmatched `(` delimiter",
			wantRng: rng(3, 2, 3, 3),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, fc := newServerWithFakeConn(t)
			require.NoError(t, srv.Handle(context.Background(), noopReply,
				didOpenCall(t, "file:///work/bad.proto", 1, c.src)))

			notes := fc.calls()
			require.Len(t, notes, 1)
			diags := notes[0].params.Diagnostics
			require.NotEmpty(t, diags, "malformed construct must surface a diagnostic")

			var match *protocol.Diagnostic
			for i := range diags {
				if strings.Contains(diags[i].Message, c.wantMsg) {
					match = &diags[i]
					break
				}
			}
			require.NotNil(t, match, "no diagnostic contains %q; got %+v", c.wantMsg, diags)
			assert.Equal(t, c.wantRng, match.Range, "squiggle must cover the offending token")
			assert.Equal(t, "protolsp/proto", match.Source)
			assert.Equal(t, protocol.DiagnosticSeverityError, match.Severity)
		})
	}
}

// rng builds a 0-indexed LSP range on the given (line, char) pairs.
func rng(startLine, startChar, endLine, endChar uint32) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: startLine, Character: startChar},
		End:   protocol.Position{Line: endLine, Character: endChar},
	}
}
