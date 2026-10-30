// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

func TestLiteralSecretDiagnostic_EmittedForScalarShorthand(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1, "exactly one publishDiagnostics call")
	diags := notes[0].params.Diagnostics

	var secretDiag *protocol.Diagnostic
	for i := range diags {
		if optValue(diags[i].Source) == "protolsp/security" {
			secretDiag = &diags[i]
			break
		}
	}
	require.NotNil(t, secretDiag, "literal-secret diagnostic must be published")
	assert.Equal(t, protocol.DiagnosticSeverityWarning, secretDiag.Severity)
	assert.Contains(t, messageText(secretDiag.Message), "password",
		"diagnostic names the field that holds the literal")
	assert.Contains(t, messageText(secretDiag.Message), "${env.PASSWORD}",
		"diagnostic suggests an env-var reference template")
}

func TestLiteralSecretDiagnostic_SuppressedForEnvReference(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)
	const src = `@type acme.demo.v1.Credentials

username = "alice"
password = "${env.MY_PASSWORD}"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	for _, d := range notes[0].params.Diagnostics {
		assert.NotEqual(t, "protolsp/security", optValue(d.Source),
			"${env.X} values must not trigger a literal-secret warning")
	}
}

func TestLiteralSecretDiagnostic_EmittedForBlockFormValue(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsBlockSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	var found bool
	for _, d := range notes[0].params.Diagnostics {
		if optValue(d.Source) == "protolsp/security" {
			found = true
			assert.Contains(t, messageText(d.Message), "password",
				"block-form literal-secret diagnostic carries the path")
		}
	}
	assert.True(t, found, "block-form literal value triggers the warning too")
}

func TestLiteralSecretDiagnostic_SuppressedForSopsEncryptedFile(t *testing.T) {
	// .sops.pxf files carry literal secret values by design —
	// sops encrypts them at rest. The "consider a vault reference"
	// suggestion doesn't apply: the file IS the vault. Identical
	// source to TestLiteralSecretDiagnostic_EmittedForScalarShorthand
	// but the URI suffix flips the diagnostic off.
	srv, fc, _ := newServerWithCredentialsType(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.sops.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	for _, d := range notes[0].params.Diagnostics {
		assert.NotEqual(t, "protolsp/security", optValue(d.Source),
			".sops.pxf files must not surface literal-secret warnings")
	}
}

func TestLiteralSecretDiagnostic_NoSecretsNoDiagnostic(t *testing.T) {
	// Greeting fixture has no Secret-typed fields. Nothing fires.
	srv, fc, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	for _, d := range notes[0].params.Diagnostics {
		assert.NotEqual(t, "protolsp/security", optValue(d.Source))
	}
}

func TestCodeAction_LiteralSecretQuickFix(t *testing.T) {
	srv, _, _ := newServerWithCredentialsType(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	// Synthesize the same diagnostic the server emitted, then ask
	// codeAction what fixes apply.
	doc := srv.Documents().Get("file:///work/c.pxf")
	require.NotNil(t, doc)
	require.NotEmpty(t, doc.SecretRanges)
	sr := doc.SecretRanges[0]

	diag := protocol.Diagnostic{
		Range:    sr.Range,
		Severity: protocol.DiagnosticSeverityWarning,
		Source:   protocol.NewOptional("protolsp/security"),
		Message:  protocol.String("literal secret value"),
		Data: testAny(LiteralSecretData{
			Kind:      DiagDataKindLiteralSecret,
			FieldPath: sr.Path,
		}),
	}

	params := protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///work/c.pxf"},
		Range:        sr.Range,
		Context:      protocol.CodeActionContext{Diagnostics: []protocol.Diagnostic{diag}},
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentCodeAction, params)
	require.NoError(t, err)

	var got []protocol.CodeAction
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, _ := json.Marshal(result)
		_ = json.Unmarshal(b, &got)
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))

	require.Len(t, got, 1, "exactly one quick fix offered")
	action := got[0]
	assert.Equal(t, protocol.CodeActionKindQuickFix, *action.Kind)
	assert.Contains(t, action.Title, "${env.PASSWORD}")
	require.NotNil(t, action.Edit)
	edits := action.Edit.Changes["file:///work/c.pxf"]
	require.Len(t, edits, 1)
	assert.Equal(t, `"${env.PASSWORD}"`, edits[0].NewText,
		"replacement text is the quoted env reference")
	assert.Equal(t, sr.Range, edits[0].Range,
		"edit range matches the secret's range (the whole string literal)")
}

func TestCodeAction_LiteralSecretViaMapPayload(t *testing.T) {
	// Real LSP clients deliver Diagnostic.Data as map[string]any
	// (JSON round-trip). The decoder must handle that shape.
	srv, _, _ := newServerWithCredentialsType(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	doc := srv.Documents().Get("file:///work/c.pxf")
	require.NotNil(t, doc)
	require.NotEmpty(t, doc.SecretRanges)
	sr := doc.SecretRanges[0]

	diag := protocol.Diagnostic{
		Range:    sr.Range,
		Severity: protocol.DiagnosticSeverityWarning,
		Source:   protocol.NewOptional("protolsp/security"),
		// Map-form payload — what LSP clients send back.
		Data: testAny(map[string]interface{}{
			"kind":       DiagDataKindLiteralSecret,
			"field_path": "password",
		}),
	}

	actions := srv.CodeActionsFor(doc, []protocol.Diagnostic{diag})
	require.Len(t, actions, 1,
		"map-form Diagnostic.Data decodes the same as the typed struct")
	assert.Contains(t, actions[0].Title, "${env.PASSWORD}")
}

func TestCodeAction_UnknownAndLiteralCoexist(t *testing.T) {
	// Multiple diagnostics of different kinds in one request → one
	// quick fix per matching diagnostic, in input order.
	srv, _, _ := newServerWithCredentialsType(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))
	doc := srv.Documents().Get("file:///work/c.pxf")

	unknownDiag := protocol.Diagnostic{
		Range:    protocol.Range{Start: protocol.Position{Line: 0, Character: 0}},
		Source:   protocol.NewOptional("protolsp/schema"),
		Severity: protocol.DiagnosticSeverityError,
		Data: testAny(UnknownFieldData{
			Kind:      DiagDataKindUnknownField,
			FieldName: "bogus",
			Line:      2,
		}),
	}
	secretDiag := protocol.Diagnostic{
		Range:    doc.SecretRanges[0].Range,
		Source:   protocol.NewOptional("protolsp/security"),
		Severity: protocol.DiagnosticSeverityWarning,
		Data: testAny(LiteralSecretData{
			Kind:      DiagDataKindLiteralSecret,
			FieldPath: "password",
		}),
	}

	actions := srv.CodeActionsFor(doc, []protocol.Diagnostic{unknownDiag, secretDiag})
	require.Len(t, actions, 2)
	assert.Contains(t, actions[0].Title, `Remove field "bogus"`)
	assert.Contains(t, actions[1].Title, "${env.PASSWORD}")
}
