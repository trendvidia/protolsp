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
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildEmployeeMessageType builds a message with a mix of normal and
// deprecated fields:
//
//	message Employee {
//	  string name = 1;
//	  string email = 2 [deprecated = true];
//	}
func buildEmployeeMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	deprecated := proto.Bool(true)
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/demo/v1/employee.proto"),
		Package: proto.String("acme.demo.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Employee"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("name"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("name")},
					{
						Name: proto.String("email"), Number: proto.Int32(2),
						Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("email"),
						Options: &descriptorpb.FieldOptions{Deprecated: deprecated},
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

func newServerWithEmployee(t *testing.T) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildEmployeeMessageType(t))
	srv.Lookups().Swap(lookup, nil)
	return srv, fc
}

func diagnosticsAt(t *testing.T, fc *fakeConn, atIndex int) []protocol.Diagnostic {
	t.Helper()
	calls := fc.calls()
	require.Greater(t, len(calls), atIndex, "want at least %d publish notifications", atIndex+1)
	return calls[atIndex].params.Diagnostics
}

func TestDiag_UnknownFieldEmitsPerOccurrenceWithData(t *testing.T) {
	srv, fc := newServerWithEmployee(t)
	const src = `@type acme.demo.v1.Employee

name = "alice"
unknown_a = 1
also_unknown = "x"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/e.pxf", 1, src)))

	diags := diagnosticsAt(t, fc, 0)
	require.Len(t, diags, 2, "two unknown fields → two diagnostics")
	// Diagnostics may not be in source order; sort by line for stable
	// assertions.
	byLine := map[uint32]protocol.Diagnostic{}
	for _, d := range diags {
		byLine[d.Range.Start.Line] = d
	}
	require.Contains(t, byLine, uint32(3))
	require.Contains(t, byLine, uint32(4))
	d := byLine[3]
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	assert.Equal(t, "protolsp/schema", optValue(d.Source))
	assert.Contains(t, messageText(d.Message), "unknown_a")

	// Data payload survives the JSON round-trip the fakeConn does
	// before storing the call. Decoded as map[string]any.
	data := DecodeUnknownFieldData(d.Data)
	require.NotNil(t, data, "Diagnostic.Data must carry the unknown_field payload")
	assert.Equal(t, "unknown_a", data.FieldName)
	assert.Equal(t, uint32(3), data.Line)
}

func TestDiag_DeprecatedFieldEmitsHintWithTag(t *testing.T) {
	srv, fc := newServerWithEmployee(t)
	const src = `@type acme.demo.v1.Employee

name = "alice"
email = "alice@example.com"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/e.pxf", 1, src)))

	diags := diagnosticsAt(t, fc, 0)
	require.Len(t, diags, 1, "only deprecated email should produce a diagnostic")
	d := diags[0]
	assert.Equal(t, protocol.DiagnosticSeverityHint, d.Severity,
		"deprecation surfaces as Hint so editors render strikethrough without complaining")
	assert.Contains(t, messageText(d.Message), "email")
	require.Equal(t, 1, d.Tags.Len())
	assert.Equal(t, protocol.DiagnosticTagDeprecated, d.Tags.Slice()[0])
}

func TestCodeAction_RemoveFieldQuickFix(t *testing.T) {
	srv, _ := newServerWithEmployee(t)
	const src = `@type acme.demo.v1.Employee

name = "alice"
unknown_a = 1
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/e.pxf", 1, src)))

	// Client typically sends back the diagnostics it's seen, with
	// Data intact. We mirror that by constructing the request with
	// the diagnostic the server just published.
	docURI := uri.URI("file:///work/e.pxf")
	diag := protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{Line: 3, Character: 0},
			End:   protocol.Position{Line: 3, Character: 1},
		},
		Severity: protocol.DiagnosticSeverityError,
		Source:   protocol.NewOptional("protolsp/schema"),
		Message:  protocol.String(`unknown field "unknown_a" on this message`),
		Data: testAny(UnknownFieldData{
			Kind: DiagDataKindUnknownField, FieldName: "unknown_a", Line: 3,
		}),
	}

	call, err := newTestCall(jsonrpc2.NewNumberID(7),
		protocol.MethodTextDocumentCodeAction,
		&protocol.CodeActionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
			Range:        diag.Range,
			Context:      protocol.CodeActionContext{Diagnostics: []protocol.Diagnostic{diag}},
		})
	require.NoError(t, err)

	var captured []protocol.CodeAction
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &captured))
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))

	require.Len(t, captured, 1, "exactly one quick-fix for the unknown field")
	act := captured[0]
	assert.Equal(t, "Remove field \"unknown_a\"", act.Title)
	assert.Equal(t, protocol.CodeActionKindQuickFix, *act.Kind)
	require.NotNil(t, act.Edit)
	edits := act.Edit.Changes[docURI]
	require.Len(t, edits, 1)
	e := edits[0]
	assert.Equal(t, "", e.NewText, "the edit deletes the entire line")
	// Range covers line 3 through start of line 4 — i.e. swallows the trailing newline.
	assert.Equal(t, uint32(3), e.Range.Start.Line)
	assert.Equal(t, uint32(0), e.Range.Start.Character)
	assert.Equal(t, uint32(4), e.Range.End.Line)
	assert.Equal(t, uint32(0), e.Range.End.Character)
}

// TestCodeAction_RecomputeAfterAppliedFixReanchors is the regression
// guard for the audit's "stacked diagnostics where one quick-fix
// invalidates another's anchor" gap. Open a doc with two unknown
// fields on adjacent lines; simulate applying the quick-fix for
// the first by sending a didChange that removes its line. The
// server must republish the remaining diagnostic on its NEW line
// number — not on the stale pre-edit line where the second field
// USED to live. A code-action request for the new diagnostic must
// then anchor its edit on the new line. Without this contract, a
// client that batches code-action invocations could delete the
// wrong row.
func TestCodeAction_RecomputeAfterAppliedFixReanchors(t *testing.T) {
	srv, fc := newServerWithEmployee(t)
	// Two unknown fields, lines 3 and 4 (0-indexed).
	const src = `@type acme.demo.v1.Employee

name = "alice"
unknown_a = 1
unknown_b = 2
`
	docURI := uri.URI("file:///work/e.pxf")
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, src)))

	// didOpen produced one publishDiagnostics. Pull the
	// pre-edit diagnostic for unknown_b out of it so we can
	// compare to its post-edit replacement.
	preEditDiags := func() []protocol.Diagnostic {
		notes := fc.calls()
		if len(notes) == 0 {
			return nil
		}
		return notes[len(notes)-1].params.Diagnostics
	}()
	require.Len(t, preEditDiags, 2, "two unknown fields → two diagnostics")
	var preEditB *protocol.Diagnostic
	for i, d := range preEditDiags {
		if data := DecodeUnknownFieldData(d.Data); data != nil && data.FieldName == "unknown_b" {
			preEditB = &preEditDiags[i]
		}
	}
	require.NotNil(t, preEditB, "must find the unknown_b diagnostic before the edit")
	preEditBLine := DecodeUnknownFieldData(preEditB.Data).Line
	require.Equal(t, uint32(4), preEditBLine,
		"sanity: unknown_b sits on line 4 before the edit")

	// Apply the unknown_a quick-fix by sending the equivalent
	// didChange. A real client would forward the action's
	// WorkspaceEdit through workspace/applyEdit; in-process tests
	// can mutate directly via didChange to the new source.
	const editedSrc = `@type acme.demo.v1.Employee

name = "alice"
unknown_b = 2
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, string(docURI), 2, editedSrc)))

	// Post-edit: one diagnostic remains, on line 3 (was line 4).
	postEditDiags := func() []protocol.Diagnostic {
		notes := fc.calls()
		return notes[len(notes)-1].params.Diagnostics
	}()
	require.Len(t, postEditDiags, 1,
		"after removing unknown_a, only the unknown_b diagnostic remains")
	postEditB := postEditDiags[0]
	postEditBData := DecodeUnknownFieldData(postEditB.Data)
	require.NotNil(t, postEditBData, "remaining diagnostic must carry UnknownFieldData")
	assert.Equal(t, "unknown_b", postEditBData.FieldName)
	assert.Equal(t, uint32(3), postEditBData.Line,
		"server must re-anchor the post-edit diagnostic to the new line — was 4, now 3")

	// Code-action for the re-anchored diagnostic must target the
	// NEW line. If the server returned a stale action (line 4),
	// applying it would delete the wrong content.
	doc := srv.Documents().Get("file:///work/e.pxf")
	require.NotNil(t, doc)
	actions := srv.CodeActionsFor(doc, []protocol.Diagnostic{postEditB})
	require.Len(t, actions, 1)
	require.NotNil(t, actions[0].Edit)
	edits := actions[0].Edit.Changes[docURI]
	require.Len(t, edits, 1)
	assert.Equal(t, uint32(3), edits[0].Range.Start.Line,
		"recomputed action must target the new (post-edit) line for unknown_b")
	assert.Equal(t, uint32(4), edits[0].Range.End.Line,
		"line range still spans line→next-line for full-line removal")
}

func TestCodeAction_IgnoresUnrelatedDiagnostics(t *testing.T) {
	// A diagnostic without our Data payload (e.g. a parse error from
	// "protolsp/parse") must not produce any code action.
	srv, _ := newServerWithEmployee(t)
	const src = `@type acme.demo.v1.Employee
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/e.pxf", 1, src)))

	diag := protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 0, Character: 1},
		},
		Severity: protocol.DiagnosticSeverityError,
		Source:   protocol.NewOptional("protolsp/parse"),
		Message:  protocol.String("syntax error"),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(8),
		protocol.MethodTextDocumentCodeAction,
		&protocol.CodeActionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI("file:///work/e.pxf")},
			Context:      protocol.CodeActionContext{Diagnostics: []protocol.Diagnostic{diag}},
		})
	require.NoError(t, err)

	gotResult := true
	reply := func(_ context.Context, result interface{}, _ error) error {
		gotResult = result != nil
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	assert.False(t, gotResult, "non-protolsp diagnostics should yield no actions (null result)")
}

// TestDiagnosticData_RoundTripsViaJSON pins down the wire-shape
// contract the code-action handlers rely on. Both Diagnostic.Data
// payloads (UnknownFieldData, LiteralSecretData) are emitted as
// typed Go structs server-side, but they come back from the LSP
// client as JSON-deserialized values whose Go type is
// map[string]interface{} (because protocol.Diagnostic.Data is
// declared as interface{}). The decoders are coded against that
// shape; this test guards the contract so a future refactor of the
// data types can't silently break it.
//
// Maps to the "Diagnostic Data round-trip smoke test" roadmap item.
func TestDiagnosticData_RoundTripsViaJSON(t *testing.T) {
	t.Run("UnknownFieldData", func(t *testing.T) {
		original := UnknownFieldData{
			Kind:      DiagDataKindUnknownField,
			FieldName: "missing_field",
			Line:      7,
		}
		// Marshal a Diagnostic with the typed value, then
		// Unmarshal back into a Diagnostic — same path
		// dispatchCodeAction takes when a real client returns the
		// diagnostic with the CodeActionRequest.
		buf, err := protocol.Marshal(protocol.Diagnostic{Data: testAny(original)})
		require.NoError(t, err)
		var roundtripped protocol.Diagnostic
		require.NoError(t, protocol.Unmarshal(buf, &roundtripped))

		decoded := DecodeUnknownFieldData(roundtripped.Data)
		require.NotNil(t, decoded)
		assert.Equal(t, original.Kind, decoded.Kind)
		assert.Equal(t, original.FieldName, decoded.FieldName)
		assert.Equal(t, original.Line, decoded.Line)
	})

	t.Run("LiteralSecretData", func(t *testing.T) {
		original := LiteralSecretData{
			Kind:      DiagDataKindLiteralSecret,
			FieldPath: "credentials.password",
		}
		buf, err := protocol.Marshal(protocol.Diagnostic{Data: testAny(original)})
		require.NoError(t, err)
		var roundtripped protocol.Diagnostic
		require.NoError(t, protocol.Unmarshal(buf, &roundtripped))

		decoded := DecodeLiteralSecretData(roundtripped.Data)
		require.NotNil(t, decoded)
		assert.Equal(t, original.Kind, decoded.Kind)
		assert.Equal(t, original.FieldPath, decoded.FieldPath)
	})
}

func TestInitialize_AdvertisesCodeActionProvider(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize,
		&protocol.InitializeParams{ClientInfo: protocol.ClientInfo{Name: "test"}})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), rep.reply, call))

	result := rep.result.(*initializeResult)
	opts, ok := result.Capabilities.CodeActionProvider.(*protocol.CodeActionOptions)
	require.True(t, ok, "CodeActionProvider should be a *CodeActionOptions, got %T", result.Capabilities.CodeActionProvider)
	require.Contains(t, opts.CodeActionKinds, protocol.CodeActionKindQuickFix)
}
