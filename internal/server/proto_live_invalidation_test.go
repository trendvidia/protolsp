// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/protoast"
)

func TestWatchedFiles_RegistrationIncludesProtoAndBuf(t *testing.T) {
	workspaceDir := t.TempDir()
	_, fc := initializeWithWatchSupport(t, workspaceDir)

	calls := fc.callsByMethod(protocol.MethodClientRegisterCapability)
	require.Len(t, calls, 1)
	var reg protocol.RegistrationParams
	require.NoError(t, json.Unmarshal(calls[0].params, &reg))

	var watcherReg *protocol.Registration
	for i := range reg.Registrations {
		if reg.Registrations[i].Method == protocol.MethodWorkspaceDidChangeWatchedFiles {
			watcherReg = &reg.Registrations[i]
			break
		}
	}
	require.NotNil(t, watcherReg, "didChangeWatchedFiles registration is present")

	// Re-decode RegisterOptions from the raw registration. The
	// LSP type uses interface{} so we round-trip through JSON to
	// get back to the typed shape.
	raw, err := json.Marshal(watcherReg.RegisterOptions)
	require.NoError(t, err)
	var opts protocol.DidChangeWatchedFilesRegistrationOptions
	require.NoError(t, json.Unmarshal(raw, &opts))

	globs := make([]string, 0, len(opts.Watchers))
	for _, w := range opts.Watchers {
		globs = append(globs, w.GlobPattern)
	}

	assert.Contains(t, globs, "**/*.proto",
		"phase 3 step 5: .proto sources are watched so the index can rebuild on disk-side changes")
	assert.Contains(t, globs, "**/buf.yaml",
		"buf.yaml watch triggers a full index rebuild on dep/module changes")
	assert.Contains(t, globs, "**/buf.lock",
		"buf.lock watch picks up commit pin changes that shift BSR cache paths")
}

func TestWatchedFiles_ProtoChangeRebuildsIndex(t *testing.T) {
	// Stage a buf.yaml workspace with one proto file; let
	// initialize build the first index; then write a SECOND proto
	// to disk and fire a watched-files event for it. The next
	// IndexLookup must see the new file.
	workspaceDir := t.TempDir()
	mustWrite(t, filepath.Join(workspaceDir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(workspaceDir, "proto", "first.proto"), `syntax = "proto3";
package x.v1;
message First { string a = 1; }
`)
	srv, _ := initializeWithWatchSupport(t, workspaceDir)

	require.NotNil(t, srv.SnapshotProtoIndex().Lookup("first.proto"),
		"baseline: the first proto is indexed at init time")
	assert.Nil(t, srv.SnapshotProtoIndex().Lookup("second.proto"),
		"baseline: the second proto isn't on disk yet")

	// Now write the second proto and fire the watch event.
	secondPath := filepath.Join(workspaceDir, "proto", "second.proto")
	mustWrite(t, secondPath, `syntax = "proto3";
package x.v1;
message Second { string b = 1; }
`)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(secondPath), Type: protocol.FileChangeTypeCreated},
		})))

	assert.NotNil(t, srv.SnapshotProtoIndex().Lookup("second.proto"),
		"new .proto under the module root is picked up after the watch event")
}

func TestWatchedFiles_ProtoContentChange_InvalidatesCachedAST(t *testing.T) {
	// Edge case: the file is already in the index, but its
	// content changed on disk. The lazy-AST cache means a stale
	// parse would survive across the change unless the index is
	// rebuilt. Verify the rebuild swaps in a fresh Index (which
	// reparses on demand) by checking the AST sees the new
	// message name.
	workspaceDir := t.TempDir()
	mustWrite(t, filepath.Join(workspaceDir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	protoPath := filepath.Join(workspaceDir, "proto", "foo.proto")
	mustWrite(t, protoPath, `syntax = "proto3";
package x.v1;
message OldName { string a = 1; }
`)
	srv, _ := initializeWithWatchSupport(t, workspaceDir)

	// Force the lazy parse to fire so we have a cached AST.
	before := srv.SnapshotProtoIndex().Lookup("foo.proto")
	require.NotNil(t, before)
	assert.True(t, fileNodeHasMessage(before, "OldName"),
		"baseline AST sees OldName")
	assert.False(t, fileNodeHasMessage(before, "NewName"),
		"baseline AST does not see NewName yet")

	// Rewrite the file with a renamed message and fire the watch
	// event.
	require.NoError(t, os.WriteFile(protoPath, []byte(`syntax = "proto3";
package x.v1;
message NewName { string a = 1; }
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(protoPath), Type: protocol.FileChangeTypeChanged},
		})))

	after := srv.SnapshotProtoIndex().Lookup("foo.proto")
	require.NotNil(t, after)
	assert.NotSame(t, before, after,
		"the index swapped to a fresh Index, so the cached AST changed identity")
	assert.True(t, fileNodeHasMessage(after, "NewName"),
		"after rebuild + reparse, the new message name is visible")
}

func TestWatchedFiles_BufYAMLChangeRebuildsIndex(t *testing.T) {
	// Start with a workspace that has NO buf.yaml (naive fallback:
	// workspace root + proto/). Fire a buf.yaml-created event. The
	// next index lookup should reflect the new module configuration.
	workspaceDir := t.TempDir()
	mustWrite(t, filepath.Join(workspaceDir, "proto", "a.proto"), `syntax = "proto3";
package x.v1;
`)
	srv, _ := initializeWithWatchSupport(t, workspaceDir)

	// In the naive fallback, the file appears under its proto/-
	// relative key (since the proto/ subdir is an include path).
	require.NotNil(t, srv.SnapshotProtoIndex().Lookup("a.proto"),
		"baseline: naive-fallback resolves a.proto from the proto/ subdir")

	// Now write a buf.yaml that declares a DIFFERENT module root.
	// The index must rebuild and the file should no longer appear
	// at the same key (because there's no module rooted at proto/
	// anymore in this hypothetical config — pretend the user moved
	// to a multi-module layout).
	yamlPath := filepath.Join(workspaceDir, "buf.yaml")
	require.NoError(t, os.WriteFile(yamlPath, []byte(`version: v2
modules:
  - path: api
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(yamlPath), Type: protocol.FileChangeTypeCreated},
		})))

	idx := srv.SnapshotProtoIndex()
	// `proto/` is no longer an include path; the file isn't indexed.
	assert.Nil(t, idx.Lookup("a.proto"),
		"after buf.yaml flips to a different module, the old proto/ subdir's content is no longer indexed")

	// The include path list reflects the new module declaration.
	paths := idx.IncludePaths()
	require.Len(t, paths, 1)
	assert.True(t, strings.HasSuffix(paths[0], filepath.Join("api")),
		"new include path is the declared module's directory")
}

func TestWatchedFiles_BufLockChangeRebuildsIndex(t *testing.T) {
	// buf.lock changes can shift BSR cache paths; the index needs
	// a rebuild even when buf.yaml hasn't changed. We test by
	// firing a buf.lock-created event and asserting the index
	// instance changed (a new Index was installed).
	workspaceDir := t.TempDir()
	mustWrite(t, filepath.Join(workspaceDir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(workspaceDir, "proto", "x.proto"), `syntax = "proto3";
`)
	srv, _ := initializeWithWatchSupport(t, workspaceDir)

	before := srv.SnapshotProtoIndex()
	require.NotNil(t, before)

	lockPath := filepath.Join(workspaceDir, "buf.lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(`version: v2
deps:
  - name: buf.build/some/dep
    commit: abc
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(lockPath), Type: protocol.FileChangeTypeCreated},
		})))

	after := srv.SnapshotProtoIndex()
	assert.NotSame(t, before, after,
		"buf.lock change swaps in a fresh index instance")
}

func TestWatchedFiles_ProtoOutsideIndexIsIgnored(t *testing.T) {
	// A .proto event for a path OUTSIDE the index's include paths
	// (and outside proto_files paths) shouldn't trigger any rebuild.
	// Guards against over-aggressive rebuilds — every .proto edit
	// in the entire watched tree would otherwise force a full
	// index rebuild even when irrelevant.
	workspaceDir := t.TempDir()
	mustWrite(t, filepath.Join(workspaceDir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(workspaceDir, "proto", "in-scope.proto"), `syntax = "proto3";`)
	srv, _ := initializeWithWatchSupport(t, workspaceDir)

	before := srv.SnapshotProtoIndex()
	require.NotNil(t, before)

	// Create a .proto OUTSIDE the proto/ module dir.
	outsidePath := filepath.Join(workspaceDir, "scratch", "playground.proto")
	mustWrite(t, outsidePath, `syntax = "proto3";`)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(outsidePath), Type: protocol.FileChangeTypeCreated},
		})))

	after := srv.SnapshotProtoIndex()
	assert.Same(t, before, after,
		"out-of-scope .proto events don't trigger an index rebuild")
}

// fileNodeHasMessage walks a parsed proto file looking for a
// top-level (or nested) message with the given name. Used by the
// content-change test to verify that a rebuilt index reparsed the
// file's bytes rather than serving a stale AST. Local pre-order
// walk: the analysis package's walkMessages is engine-internal.
func fileNodeHasMessage(file *protoast.File, name string) bool {
	if file == nil || file.AST == nil {
		return false
	}
	var hasMessage func(body ast.DeclBody) bool
	hasMessage = func(body ast.DeclBody) bool {
		for decl := range seq.Values(body.Decls()) {
			def := decl.AsDef()
			if def.Classify() != ast.DefKindMessage {
				continue
			}
			if def.Name().AsIdent().Text() == name {
				return true
			}
			if b := def.Body(); !b.IsZero() && hasMessage(b) {
				return true
			}
		}
		return false
	}
	for decl := range seq.Values(file.AST.Decls()) {
		def := decl.AsDef()
		if def.Classify() != ast.DefKindMessage {
			continue
		}
		if def.Name().AsIdent().Text() == name {
			return true
		}
		if b := def.Body(); !b.IsZero() && hasMessage(b) {
			return true
		}
	}
	return false
}
