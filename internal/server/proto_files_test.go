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
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/schema"
)

// wsConfigProtoFilesOnly declares a single proto_files source
// pointing at a local "proto/" subdirectory. Used by the W2 tests
// to verify the workspace ends up with a proto_files-backed
// lookup, no registry needed.
const wsConfigProtoFilesOnly = `@type protolsp.v1.WorkspaceConfig

proto_files {
  paths = ["proto/"]
}
`

const greetingProtoSrc = `syntax = "proto3";
package myapp.v1;

message Greeting {
  string name = 1;
  string locale = 2;
}
`

func TestProtoFiles_RootLookupBuiltFromProtosOnDisk(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigProtoFilesOnly), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "proto/myapp/v1"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "proto/myapp/v1/greeting.proto"),
		[]byte(greetingProtoSrc), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	lk := srv.Lookups().SnapshotProtoFiles()
	require.NotNil(t, lk, "proto_files lookup must be installed when .protolsp.pxf declares one")
	mt, err := lk.FindMessageByName(protoreflect.FullName("myapp.v1.Greeting"))
	require.NoError(t, err)
	require.NotNil(t, mt)
	assert.Equal(t, protoreflect.FullName("myapp.v1.Greeting"), mt.Descriptor().FullName())

	// Server-wide Snapshot falls through from registry to proto_files
	// when only the latter is configured (W2 fork).
	root := srv.Lookups().Snapshot()
	require.NotNil(t, root)
	_, err = root.FindMessageByName(protoreflect.FullName("myapp.v1.Greeting"))
	assert.NoError(t, err, "lookupMgr.Snapshot routes to proto_files when no registry is set")
}

func TestProtoFiles_CompileErrorAppearsAsDiagnosticOnConfig(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigProtoFilesOnly), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "proto"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "proto/bad.proto"),
		// Intentional syntax error so protocompile rejects.
		[]byte(`syntax = "proto3";
package broken;
message  // missing body
`), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	errs := srv.RootProtoFilesErrs().Snapshot()
	require.NotEmpty(t, errs, "broken .proto must surface compile errors on root")
	assert.Contains(t, errs[0].File, "bad.proto")

	// Open the .protolsp.pxf in the doc store so configDiagnostics
	// has something to compute against.
	cfgURI := uri.File(filepath.Join(workspaceDir, ".protolsp.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(cfgURI), 1, wsConfigProtoFilesOnly)))
	doc := srv.Documents().Get(cfgURI)
	require.NotNil(t, doc)
	diags := srv.ConfigDiagnostics(doc)
	require.NotEmpty(t, diags, "compile errors must surface as diagnostics on .protolsp.pxf")
	var found bool
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/proto_files" {
			found = true
			assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
			assert.Contains(t, messageText(d.Message), "bad.proto")
		}
	}
	assert.True(t, found, "expected at least one proto_files-source diagnostic")
}

func TestProtoFiles_SubWorkspaceGetsItsOwnLookup(t *testing.T) {
	workspaceDir := t.TempDir()
	subDir := filepath.Join(workspaceDir, "svc-a")
	require.NoError(t, os.MkdirAll(filepath.Join(subDir, "proto"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, ".protolsp.pxf"),
		[]byte(wsConfigProtoFilesOnly), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, "proto/greeting.proto"),
		[]byte(`syntax="proto3"; package svca; message Hi { string who = 1; }`),
		0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	// Open a doc inside the sub-workspace so it gets lazy-loaded.
	docURI := uri.File(filepath.Join(subDir, "hi.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "@type svca.Hi\nwho = \"world\"\n")))

	doc := srv.Documents().Get(docURI)
	require.NotNil(t, doc)
	srv.EnsureWorkspaceForDoc(doc)

	sw, ok := srv.SubWorkspaceByDir(subDir)
	require.True(t, ok, "sub-workspace must be registered")
	require.NotNil(t, sw)
	require.NotNil(t, sw.ProtoFiles, "sub-workspace gets its own proto_files lookup")

	lk := srv.LookupForDoc(doc)
	require.NotNil(t, lk, "lookupForDoc routes to the sub's proto_files lookup")
	_, err := lk.FindMessageByName(protoreflect.FullName("svca.Hi"))
	assert.NoError(t, err)
}

// wsConfigBothSources declares a registry block AND a proto_files
// block in the same workspace — the W3 composition shape. The
// registry address is intentionally unreachable so the dial fails
// fast in the background; we only assert that proto_files is
// installed and reachable via lookupMgr.SnapshotProtoFiles.
const wsConfigBothSources = `@type protolsp.v1.WorkspaceConfig

registry {
  address    = "unreachable.example.invalid:443"
  namespace  = "test"
}

proto_files {
  paths = ["proto/"]
}
`

func TestProtoFiles_W3WorkspaceWithRegistryAndProtoFilesKeepsBoth(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigBothSources), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "proto/myapp/v1"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "proto/myapp/v1/greeting.proto"),
		[]byte(greetingProtoSrc), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	// Proto_files lookup is installed synchronously before the
	// registry dial fires, so it's observable immediately.
	pf := srv.Lookups().SnapshotProtoFiles()
	require.NotNil(t, pf)
	mt, err := pf.FindMessageByName(protoreflect.FullName("myapp.v1.Greeting"))
	require.NoError(t, err)
	require.NotNil(t, mt)

	// The W3 wire-up extracts files+types from the proto_files
	// lookup for WithFallback. Verify the data is there even when
	// the dial goroutine hasn't completed (it never will against
	// an unreachable.example.invalid address).
	files, types := schema.ProtoFilesParents(pf)
	assert.NotNil(t, files)
	assert.NotNil(t, types)
}

func TestProtoFiles_NoBlockMeansNoLookup(t *testing.T) {
	// Registry-only config — proto_files slot must stay nil.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigRegistryOnly), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	assert.Nil(t, srv.Lookups().SnapshotProtoFiles())
	assert.Empty(t, srv.RootProtoFilesErrs().Snapshot())
}
