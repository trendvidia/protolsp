// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// definitionFromHandle drives the definition dispatcher and returns
// the deserialized Location slice (or nil). Mirrors what a real LSP
// client would see.
func definitionFromHandle(t *testing.T, srv *Server, u string, line, char uint32) []protocol.Location {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentDefinition,
		&protocol.DefinitionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
				Position:     protocol.Position{Line: line, Character: char},
			},
		})
	require.NoError(t, err)

	var captured []protocol.Location
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := testCodec.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, testCodec.Unmarshal(b, &captured))
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return captured
}

func TestDefinition_FieldAssignmentResolvesToProtoFile(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work/protos")

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	// Line 2 is `name = "x"`. Definition should point at the field's
	// .proto file path joined to the workspace root.
	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file:///work/protos/acme/demo/v1/greeting.proto"), locs[0].URI,
		"URI must be the .proto file path resolved against workspaceRoot")
}

func TestDefinition_TypeDirectiveResolvesToMessageProto(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work/protos")

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n")))

	// Cursor anywhere on the @type line should resolve.
	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 0, 10)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file:///work/protos/acme/demo/v1/greeting.proto"), locs[0].URI)
}

func TestDefinition_TypeDirectiveUnknownFQNReturnsNil(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work/protos")

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type some.unknown.Type\n")))

	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 0, 10)
	assert.Nil(t, locs, "unknown FQN can't be resolved → no Location")
}

func TestDefinition_NoWorkspaceRootReturnsNil(t *testing.T) {
	// Without a workspace root we have no way to turn a relative
	// .proto path into a sensible URI. Decline rather than emit a
	// bogus URI that would just produce "file not found" in the
	// editor.
	srv, _, _ := newServerWithLookup(t)
	// workspaceRoot intentionally not set.

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	assert.Nil(t, locs, "missing workspace root → null definition")
}

func TestDefinition_WorkspaceRootWithTrailingSlash(t *testing.T) {
	// uri.URI strings sometimes carry a trailing slash and sometimes
	// don't. The path joiner must produce the same result either
	// way — `file:///work/` + `a.proto` and `file:///work` +
	// `a.proto` should both yield `file:///work/a.proto`.
	srv, _, _ := newServerWithLookup(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work/protos/")

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file:///work/protos/acme/demo/v1/greeting.proto"), locs[0].URI,
		"trailing slash on workspaceRoot must not produce a double slash in the joined URI")
}

func TestDefinition_NoLookupConfiguredReturnsNil(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work/protos")
	// No s.lookup assignment.

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	assert.Nil(t, locs, "no Lookup → no descriptors → null definition")
}

func TestDefinition_CursorOutsideAnyTargetReturnsNil(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work/protos")

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	// Line 1 is empty — no field, no @type directive.
	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 1, 0)
	assert.Nil(t, locs, "cursor on a blank line → no definition")
}

func TestDefinition_NestedBlockFieldResolves(t *testing.T) {
	// Cursor on a field assignment inside a nested block should
	// resolve against the nested message's descriptor, not the root.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildOrderMessageType(t))
	srv.Lookups().Swap(lookup, nil)
	srv.Workspaces().SetWorkspaceRoot("file:///work/protos")
	_ = fc

	const src = `@type acme.demo.v1.Order

shipping {
  street = "x"
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/o.pxf", 1, src)))

	// `street = "x"` is on line 3 (0-indexed). It's inside a nested
	// Address block (acme/demo/v1/order.proto declares both Order and
	// Address in the same file).
	locs := definitionFromHandle(t, srv, "file:///work/o.pxf", 3, 2)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file:///work/protos/acme/demo/v1/order.proto"), locs[0].URI)
}

func TestDefinition_RegistryFallback_WhenFileNotOnDisk(t *testing.T) {
	// File isn't on disk under workspaceRoot but the lookup attributes
	// it to a tracked namespace → return a registry:// URI rather than
	// a dead-end file:// path. The editor's content provider fetches
	// bytes via protolsp/sourceContent.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildGreetingMessageType(t), "acme-demo")
	srv.Lookups().Swap(lookup, nil)
	srv.Workspaces().SetWorkspaceRoot("file:///nonexistent/work")
	_ = fc

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("registry://acme-demo/acme/demo/v1/greeting.proto"), locs[0].URI,
		"file not on disk + origin tracked → registry:// URI carrying namespace + path")
}

func TestDefinition_PrefersLocalCopy_WhenBothExist(t *testing.T) {
	// File exists on disk under workspaceRoot AND the lookup tracks a
	// namespace origin. Working-copy semantics win — return file:// so
	// the user edits their local source rather than the registry's
	// read-only view.
	workDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "acme/demo/v1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "acme/demo/v1/greeting.proto"),
		[]byte(`syntax = "proto3"; package acme.demo.v1; message Greeting { string name = 1; }`), 0o644))

	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildGreetingMessageType(t), "acme-demo")
	srv.Lookups().Swap(lookup, nil)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + workDir))
	_ = fc

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\nname = \"x\"\n")))

	locs := definitionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file://"+workDir+"/acme/demo/v1/greeting.proto"), locs[0].URI,
		"local working copy on disk wins over registry:// fallback")
}

func TestDefinition_ProtoFilesSourceResolvesToLocalFile(t *testing.T) {
	// Regression test: a message backed by a local
	// proto_files source must resolve go-to-definition to the
	// file:// path the compile read from disk — NOT a registry://
	// preview of a file the user could be editing directly.
	// Descriptor paths are search-path-relative
	// ("myapp/v1/greeting.proto"), the file lives under the
	// configured search path ("proto/myapp/v1/greeting.proto"), so
	// the workspace-root existence probe misses, and the lookup's
	// origin attribution used to win.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigProtoFilesOnly), 0o600))
	protoPath := filepath.Join(workspaceDir, "proto/myapp/v1/greeting.proto")
	require.NoError(t, os.MkdirAll(filepath.Dir(protoPath), 0o700))
	require.NoError(t, os.WriteFile(protoPath, []byte(greetingProtoSrc), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	docURI := uri.File(filepath.Join(workspaceDir, "greeting.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "@type myapp.v1.Greeting\n\nname = \"alice\"\n")))

	// Line 2 is `name = "alice"`.
	locs := definitionFromHandle(t, srv, string(docURI), 2, 0)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.File(protoPath), locs[0].URI,
		"proto_files-backed descriptor must resolve to its on-disk working copy, not registry://")

	// The @type directive line must land on the same file.
	locs = definitionFromHandle(t, srv, string(docURI), 0, 10)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.File(protoPath), locs[0].URI)
}

func TestDefinition_SubWorkspaceProtoFilesResolvesToLocalFile(t *testing.T) {
	// Sub-workspace variant of the test above: the sub's proto_files paths are
	// relative to ITS .protolsp.pxf dir, so resolution must route
	// through the sub-workspace's lookup — probing the root gets it
	// wrong even after the exact-attribution fix.
	workspaceDir := t.TempDir()
	subDir := filepath.Join(workspaceDir, "svc-a")
	subProtoPath := filepath.Join(subDir, "proto/greeting.proto")
	require.NoError(t, os.MkdirAll(filepath.Dir(subProtoPath), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, ".protolsp.pxf"),
		[]byte(wsConfigProtoFilesOnly), 0o600))
	require.NoError(t, os.WriteFile(subProtoPath,
		[]byte(`syntax="proto3"; package svca; message Hi { string who = 1; }`),
		0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	docURI := uri.File(filepath.Join(subDir, "hi.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "@type svca.Hi\n\nwho = \"world\"\n")))

	// Line 2 is `who = "world"`.
	locs := definitionFromHandle(t, srv, string(docURI), 2, 0)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.File(subProtoPath), locs[0].URI,
		"sub-workspace proto_files descriptor must resolve to the sub's on-disk copy")
}

func TestInitialize_AdvertisesDefinitionProvider(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize,
		&protocol.InitializeParams{ClientInfo: protocol.ClientInfo{Name: "test"}})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), rep.reply, call))

	result := rep.result.(*initializeResult)
	assert.True(t, result.Capabilities.DefinitionProvider == protocol.Boolean(true),
		"server must advertise DefinitionProvider in initialize result")
}

func TestInitialize_CapturesWorkspaceFolders(t *testing.T) {
	// Multi-root: WorkspaceFolders takes priority over RootURI.
	srv := New(discardLogger())
	rep := &captureReplier{}

	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize,
		&protocol.InitializeParams{
			ClientInfo: protocol.ClientInfo{Name: "test"},
			WorkspaceFoldersInitializeParams: workspaceFolders(
				protocol.WorkspaceFolder{URI: "file:///work/main", Name: "main"},
			),
		})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), rep.reply, call))

	assert.Equal(t, uri.URI("file:///work/main"), srv.Workspaces().WorkspaceRoot())
}
