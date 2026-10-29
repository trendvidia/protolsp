// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// End-to-end tests for keyed repeated fields (draft -01 §3.13,
// protowire#116, protolsp#251): a `repeated <Message>` field carrying
// the (pxf.key) option gets first-class editor semantics — outline,
// duplicate/conflict diagnostics, keyed-aware completion/hover, and
// entry rename — driven purely from the resolved schema.
package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// fieldOptionsWithKey returns a FieldOptions carrying the (pxf.key)
// extension (field 1316, a string) as unknown bytes — the same
// protoc / pure-descriptor path protowire-go's KeyFieldName reads.
func fieldOptionsWithKey(t *testing.T, keyField string) *descriptorpb.FieldOptions {
	t.Helper()
	var buf []byte
	buf = protowire.AppendTag(buf, 1316, protowire.BytesType)
	buf = protowire.AppendBytes(buf, []byte(keyField))
	opts := &descriptorpb.FieldOptions{}
	require.NoError(t, proto.Unmarshal(buf, opts),
		"FieldOptions must accept a raw (pxf.key) extension into its unknown bytes")
	return opts
}

// buildUIDocumentType constructs a recursive keyed-tree schema:
//
//	package ui.v1;
//	message Document {
//	  repeated Element children = 1 [(pxf.key) = "name"];
//	}
//	message Element {
//	  string name  = 1;
//	  string label = 2;
//	  bool disabled = 3;
//	  repeated Element children = 4 [(pxf.key) = "name"];
//	}
//
// The self-referential `children` field makes Element a keyed
// collection of Elements — the UI-document shape the issue describes.
func buildUIDocumentType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("ui/v1/document.proto"),
		Package: proto.String("ui.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Document"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("children"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						TypeName: proto.String(".ui.v1.Element"),
						JsonName: proto.String("children"),
						Options:  fieldOptionsWithKey(t, "name"),
					},
				},
			},
			{
				Name: proto.String("Element"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("name"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("name"),
					},
					{
						Name:     proto.String("label"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("label"),
					},
					{
						Name:     proto.String("disabled"),
						Number:   proto.Int32(3),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("disabled"),
					},
					{
						Name:     proto.String("children"),
						Number:   proto.Int32(4),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						TypeName: proto.String(".ui.v1.Element"),
						JsonName: proto.String("children"),
						Options:  fieldOptionsWithKey(t, "name"),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0)) // Document
}

// newServerWithUISchema stands up a server whose lookup resolves
// ui.v1.Document (and, transitively, ui.v1.Element).
func newServerWithUISchema(t *testing.T) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildUIDocumentType(t))
	srv.Lookups().Swap(lookup, nil)
	return srv, fc
}

const keyedDocValid = `@type ui.v1.Document

children {
  loginButton {
    label = "Log in"
  }
  signupLink {
    label = "Sign up"
    disabled = true
  }
}
`

// --- Outline / document symbols ---------------------------------------

func TestKeyed_DocumentSymbolsOutlineEntriesByName(t *testing.T) {
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, keyedDocValid)))

	symbols := documentSymbolsFromHandle(t, srv, "file:///work/ui.pxf")

	children := findSymbol(symbols, "children")
	require.NotNil(t, children, "keyed field block should be a symbol")
	require.Len(t, children.Children, 2, "each keyed entry is a named child symbol")

	login := findSymbol(children.Children, "loginButton")
	require.NotNil(t, login, "entry outlines by its key, not by a field name")
	assert.Equal(t, protocol.SymbolKindObject, login.Kind)
	assert.Equal(t, "ui.v1.Element", login.Detail, "entry detail is the element message type")

	// The entry nests recursively against the element message: its
	// `label` field is a child symbol.
	assert.NotNil(t, findSymbol(login.Children, "label"),
		"entry body outlines against the element message")

	assert.NotNil(t, findSymbol(children.Children, "signupLink"))
}

func findSymbol(syms []protocol.DocumentSymbol, name string) *protocol.DocumentSymbol {
	for i := range syms {
		if syms[i].Name == name {
			return &syms[i]
		}
	}
	return nil
}

// --- Diagnostics ------------------------------------------------------

func TestKeyed_ValidDocHasNoKeyedDiagnostics(t *testing.T) {
	srv, fc := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, keyedDocValid)))

	diags := lastDiagnostics(t, fc)
	for _, d := range diags {
		assert.NotEqual(t, "protolsp/keyed", d.Source, "valid keyed doc: %q", d.Message)
		// Entry names and their bodies must not be flagged as unknown
		// fields on the parent.
		assert.NotContains(t, d.Message, "unknown field", "unexpected: %q", d.Message)
	}
}

func TestKeyed_DuplicateKeyDiagnostic(t *testing.T) {
	const src = `@type ui.v1.Document

children {
  row { label = "a" }
  row { label = "b" }
}
`
	srv, fc := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, src)))

	d := findDiagnostic(lastDiagnostics(t, fc), "protolsp/keyed")
	require.NotNil(t, d, "duplicate entry name should produce a keyed diagnostic")
	assert.Contains(t, d.Message, "duplicate key")
	assert.Contains(t, d.Message, "row")
	// The squiggle lands on the second `row`, at line 4 (0-indexed).
	assert.Equal(t, uint32(4), d.Range.Start.Line)
}

func TestKeyed_KeyConflictDiagnostic(t *testing.T) {
	const src = `@type ui.v1.Document

children {
  row { name = "other" }
}
`
	srv, fc := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, src)))

	d := findDiagnostic(lastDiagnostics(t, fc), "protolsp/keyed")
	require.NotNil(t, d, "explicit key disagreeing with the entry name should conflict")
	assert.Contains(t, d.Message, "conflicts with entry name")
}

func TestKeyed_RedundantKeyAssignmentIsNotAConflict(t *testing.T) {
	const src = `@type ui.v1.Document

children {
  row { name = "row" }
}
`
	srv, fc := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, src)))

	assert.Nil(t, findDiagnostic(lastDiagnostics(t, fc), "protolsp/keyed"),
		"an explicit key that agrees with the entry name is redundant, not a conflict")
}

// --- Completion -------------------------------------------------------

func TestKeyed_CompletionInsideEntryOffersElementFields(t *testing.T) {
	// Cursor on the blank line inside loginButton's body (line 4).
	const src = `@type ui.v1.Document

children {
  loginButton {

  }
}
`
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, src)))

	labels := labelsFromList(completionFromHandle(t, srv, "file:///work/ui.pxf", 4, 4))
	assert.Contains(t, labels, "label", "entry body completes as the element message")
	assert.Contains(t, labels, "disabled")
	assert.Contains(t, labels, "name")
}

func TestKeyed_CompletionAtEntryNamePositionOffersNoFields(t *testing.T) {
	// Cursor directly inside the children block (line 3), at
	// entry-name position — the next token is a free-form key.
	const src = `@type ui.v1.Document

children {

}
`
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, src)))

	list := completionFromHandle(t, srv, "file:///work/ui.pxf", 3, 2)
	labels := labelsFromList(list)
	assert.NotContains(t, labels, "children", "must not offer parent fields at entry-name position")
	assert.NotContains(t, labels, "label", "entry name is a free-form key, not an element field")
}

// --- Hover ------------------------------------------------------------

func TestKeyed_HoverOnEntryNameShowsElementType(t *testing.T) {
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, keyedDocValid)))

	// `loginButton` is on line 4 (0-indexed 3).
	h := hoverFromHandle(t, srv, "file:///work/ui.pxf", 3, 2)
	require.NotNil(t, h, "hovering a keyed entry name yields hover content")
	md := hoverText(h)
	assert.Contains(t, md, "ui.v1.Element", "entry hover shows the element message type")
	assert.Contains(t, md, "keyed field")
}

func TestKeyed_HoverInsideEntryResolvesElementField(t *testing.T) {
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, keyedDocValid)))

	// `label = "Log in"` is on line 5 (0-indexed 4).
	h := hoverFromHandle(t, srv, "file:///work/ui.pxf", 4, 4)
	require.NotNil(t, h)
	md := hoverText(h)
	assert.Contains(t, md, "**label**", "field inside a keyed entry resolves against the element")
	assert.Contains(t, md, "string")
}

// --- Rename -----------------------------------------------------------

func TestKeyed_RenameEntryRewritesNameToken(t *testing.T) {
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, keyedDocValid)))

	edit, rpcErr := renameFromHandle(t, srv, "file:///work/ui.pxf", 3, 4, "logoutButton")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	edits := edit.Changes["file:///work/ui.pxf"]
	require.Len(t, edits, 1, "renaming loginButton edits just its name token")
	assert.Equal(t, "logoutButton", edits[0].NewText)
	assert.Equal(t, uint32(3), edits[0].Range.Start.Line)
}

func TestKeyed_RenameEntryToNonIdentifierKeyQuotes(t *testing.T) {
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, keyedDocValid)))

	// "log out" is not identifier-safe — the rewrite must quote it, and
	// dispatch must accept the non-identifier new name for a keyed entry.
	edit, rpcErr := renameFromHandle(t, srv, "file:///work/ui.pxf", 3, 4, "log out")
	require.Nil(t, rpcErr, "keyed entry keys may be non-identifiers")
	require.NotNil(t, edit)
	edits := edit.Changes["file:///work/ui.pxf"]
	require.Len(t, edits, 1)
	assert.Equal(t, `"log out"`, edits[0].NewText, "non-identifier key is quoted to match pxf fmt")
}

func TestKeyed_RenameEntryUpdatesRedundantKeyAssignment(t *testing.T) {
	const src = `@type ui.v1.Document

children {
  row {
    name = "row"
    label = "hi"
  }
}
`
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, src)))

	edit, rpcErr := renameFromHandle(t, srv, "file:///work/ui.pxf", 3, 2, "cell")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	edits := edit.Changes["file:///work/ui.pxf"]
	require.Len(t, edits, 2, "name token + redundant key assignment update atomically")
	texts := []string{edits[0].NewText, edits[1].NewText}
	assert.Contains(t, texts, "cell", "the entry name token")
	assert.Contains(t, texts, `"cell"`, "the redundant `name =` assignment is retargeted")
}

func TestKeyed_RenamePropagatesAcrossOpenDocs(t *testing.T) {
	// Two documents define the same keyed entry (the chain-layer
	// override shape). Renaming it in one must rewrite it in both —
	// the entry's identity is its collection path + key, shared across
	// files, exactly like the path-based field rename.
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/base.pxf", 1, keyedDocValid)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/override.pxf", 1, keyedDocValid)))

	edit, rpcErr := renameFromHandle(t, srv, "file:///work/base.pxf", 3, 4, "logoutButton")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	require.Len(t, edit.Changes, 2, "keyed rename propagates to every doc defining the entry")
	for u, edits := range edit.Changes {
		require.Lenf(t, edits, 1, "one name-token edit per doc (%s)", u)
		assert.Equal(t, "logoutButton", edits[0].NewText)
	}
}

func TestKeyed_RenamePropagatesToUnopenedFileOnDisk(t *testing.T) {
	// Scope parity with references / field rename: a keyed rename must
	// reach on-disk workspace files, not only open buffers.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "base.pxf"), []byte(keyedDocValid), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "override.pxf"), []byte(keyedDocValid), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildUIDocumentType(t))
	srv.Lookups().Swap(lookup, nil)

	baseURI := uri.File(filepath.Join(workspaceDir, "base.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(baseURI), 1, keyedDocValid)))

	edit, rpcErr := renameFromHandle(t, srv, string(baseURI), 3, 4, "logoutButton")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	require.Len(t, edit.Changes, 2, "keyed rename must reach the on-disk override.pxf")

	overrideURI := uri.File(filepath.Join(workspaceDir, "override.pxf"))
	for _, target := range []uri.URI{baseURI, overrideURI} {
		edits, ok := edit.Changes[protocol.DocumentURI(target)]
		require.Truef(t, ok, "expected an edit entry for %s", target)
		require.Len(t, edits, 1)
		assert.Equal(t, "logoutButton", edits[0].NewText)
	}
}

func TestKeyed_RenameOnKeyedFieldNameFallsBackToFieldRename(t *testing.T) {
	srv, _ := newServerWithUISchema(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/ui.pxf", 1, keyedDocValid)))

	// Cursor on `children` (the keyed field itself, line 3): a
	// non-identifier is rejected because this is an ordinary field
	// rename, not a keyed entry.
	_, rpcErr := renameFromHandle(t, srv, "file:///work/ui.pxf", 2, 0, "not valid")
	require.NotNil(t, rpcErr, "field rename still requires a valid PXF identifier")
}

// hoverText extracts the markdown body of a hover result.
func hoverText(h *protocol.Hover) string {
	return h.Contents.Value
}

// findDiagnostic returns the first diagnostic with the given source, or
// nil when none matches.
func findDiagnostic(diags []protocol.Diagnostic, source string) *protocol.Diagnostic {
	for i := range diags {
		if diags[i].Source == source {
			return &diags[i]
		}
	}
	return nil
}
