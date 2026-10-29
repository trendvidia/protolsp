// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestIsProtoURI(t *testing.T) {
	cases := []struct {
		u    string
		want bool
	}{
		{"file:///work/foo.proto", true},
		{"file:///work/foo.sops.pxf", false},
		{"file:///work/foo.pxf", false},
		// registry:// .proto previews are virtual read-only views
		// the schema resolver serves — they must NOT route through
		// analyzeProto. URI.Filename() panics on non-file schemes,
		// so a true return here would crash the server on first
		// hover that opens a registry:// .proto in the editor.
		{"registry:///acme/foo.proto", false},
		{"untitled:///scratch.proto", false},
		{"file:///work/protocompile/internal.proto.txt", false}, // suffix must be exact
	}
	for _, c := range cases {
		t.Run(c.u, func(t *testing.T) {
			if got := IsProtoURI(uri.URI(c.u)); got != c.want {
				t.Errorf("IsProtoURI(%q) = %v; want %v", c.u, got, c.want)
			}
		})
	}
}

// Regression: opening a registry:// .proto document must not
// panic the server. monaco-language-client sends didOpen for
// every model whose language matches documentSelector — including
// the registry:// previews the schema resolver creates for proto
// peek/hover views. Before the IsFileURI guard, this routed
// through analyzeProto → URI.Filename() → panic. Several other
// code paths on dispatchDidOpen also call Filename() (workspace
// config detection, chain spec detection), each of which would
// panic for the same reason — so the guard sits at the top of
// dispatchDidOpen rather than inside any single downstream
// caller.
func TestDidOpen_RegistryProtoURIDoesNotCrash(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "registry:///acme/foo.proto", 1, validProto)))

	// No publishDiagnostics fires — protolsp ignores non-file
	// didOpen entirely. The registry:// content was served by
	// protolsp itself via the schema RPC and isn't subject to any
	// downstream analysis pipeline.
	assert.Empty(t, fc.calls(),
		"non-file URIs produce zero LSP notifications back to the client")

	assert.Nil(t, srv.Documents().Get("registry:///acme/foo.proto"),
		"non-file URIs are not tracked in the document store")
}

// Regression: didChange for a non-file URI must not crash. The
// editor can fire didChange even when didOpen was previously
// skipped — dispatchDidChange CREATES the document on the fly via
// docs.Open if Update doesn't find one, so the open-time guard
// alone doesn't protect this path.
func TestDidChange_RegistryProtoURIDoesNotCrash(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	// Send didChange straight off — no preceding didOpen. The
	// fall-through Open() path inside dispatchDidChange would
	// have created the doc and tripped the same Filename panic.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "registry:///acme/foo.proto", 2, validProto)))

	assert.Empty(t, fc.calls())
	assert.Nil(t, srv.Documents().Get("registry:///acme/foo.proto"))
}

// validProto compiles cleanly against protocompile's standard imports
// (no external imports referenced; just builtin types). The .proto
// syntax here is the proto3 minimum that exercises message + field
// parsing without depending on any workspace-resolved imports.
const validProto = `syntax = "proto3";

package acme.demo.v1;

message Greeting {
  string name = 1;
}
`

// brokenProto: missing semicolon after the syntax directive and a
// stray closing brace. protocompile emits at least one positioned
// error for each.
const brokenProto = `syntax = "proto3"

message Broken {
  string name = 1
}}
`

func TestDidOpen_ValidProtoPublishesEmptyDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/greet.proto", 1, validProto)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Equal(t, protocol.MethodTextDocumentPublishDiagnostics, notes[0].method)
	assert.Equal(t, uri.URI("file:///work/greet.proto"), notes[0].params.URI)
	assert.Empty(t, notes[0].params.Diagnostics, "valid .proto → no diagnostics")

	doc := srv.Documents().Get("file:///work/greet.proto")
	require.NotNil(t, doc, "didOpen must store the .proto document like any other")
	assert.Empty(t, doc.ProtoDiagnostics, "successful compile leaves ProtoDiagnostics empty")
	// pxf-specific state must stay zero for proto docs — analyze's
	// proto branch returns before touching any of it.
	assert.Nil(t, doc.Parsed, "pxf parse state is irrelevant for .proto")
	assert.NoError(t, doc.ParseErr)
}

func TestDidOpen_BrokenProtoPublishesDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/broken.proto", 1, brokenProto)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := notes[0].params.Diagnostics
	require.NotEmpty(t, diags, "broken .proto must produce at least one diagnostic")

	for _, d := range diags {
		assert.Equal(t, "protolsp/proto", d.Source,
			"every proto diagnostic carries the protolsp/proto source string")
		assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity,
			"protocompile parse failures map to Error severity")
	}

	doc := srv.Documents().Get("file:///work/broken.proto")
	require.NotNil(t, doc)
	assert.NotEmpty(t, doc.ProtoDiagnostics, "diagnostics persist on the document for re-publish")
}

// Belt-and-suspenders: a .proto with an import that doesn't resolve.
// No workspace root is set here, so the proto index is empty and the
// import can't be supplied (index-backed resolution is covered in
// proto_cross_file_test.go); protocompile reports it as a compile
// error. Confirms the diagnostic reaches the editor rather than
// silently dropping.
func TestDidOpen_UnresolvedImportSurfacesAsDiagnostic(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	const src = `syntax = "proto3";

import "no/such/file.proto";

package acme.demo.v1;
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/imports.proto", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := notes[0].params.Diagnostics
	require.NotEmpty(t, diags, "unresolved import should surface as a diagnostic")
	for _, d := range diags {
		assert.Equal(t, "protolsp/proto", d.Source)
	}
}
