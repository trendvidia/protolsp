// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// End-to-end integration test for the buf.lock-deps-add mid-
// session flow. The watched-files-side trigger
// (TestWatchedFiles_BufLockChangeRebuildsIndex) confirms the
// index instance gets swapped; the audit named the broader gap:
// no test asserts the FOLLOW-UP behavior — a fresh .proto
// reference that resolves through a newly-added BSR dep right
// after the lock changes, without restarting protolsp. A
// regression in the include-path rebuild would leave hover /
// definition broken for the just-added dep until the next
// session.

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

// TestBufLock_MidSessionDepsAdd_ResolvesAfterRebuild stages a
// proto workspace whose consumer.proto references a type from a
// BSR-cached dep that's NOT YET in buf.lock. The fake cache root
// is injected via Server.protoIndexCacheRoot so the test doesn't
// touch the developer's real ~/.cache/buf. The flow:
//
//  1. Open consumer.proto: hover on the dep type returns nothing
//     (lock has no deps → the cache path isn't in include paths
//     → the dep file isn't indexed).
//  2. Edit buf.lock to add the dep entry on disk.
//  3. Fire a watched-files event for buf.lock.
//  4. Hover on the SAME position resolves to the dep file.
//
// This pins the watched-files → index rebuild → resolver
// refresh chain end-to-end.
func TestBufLock_MidSessionDepsAdd_ResolvesAfterRebuild(t *testing.T) {
	root := t.TempDir()

	// Workspace: a single module, buf.lock initially has NO deps.
	mustWrite(t, filepath.Join(root, "buf.yaml"), `version: v2
modules:
  - path: proto
`)
	lockPath := filepath.Join(root, "buf.lock")
	mustWrite(t, lockPath, `version: v2
deps: []
`)

	// Consumer file: imports a BSR-pinned dep + references the
	// dep type. The reference will only resolve once the index
	// has the dep's cache path in its include list.
	consumerPath := filepath.Join(root, "proto", "consumer.proto")
	mustWrite(t, consumerPath, `syntax = "proto3";

package consumer.v1;

import "remote/types.proto";

message Consumer {
  remote.v1.Remote ref = 1;
}
`)

	// Fake BSR cache root: <cache>/<name>/<commit>/files/<path>.proto.
	// Layout mirrors what buf CLI v3 maintains under the user cache.
	cacheRoot := t.TempDir()
	const depName = "buf.build/example/remote"
	const depCommit = "deadbeefdeadbeefdeadbeefdeadbeef"
	remoteProtoPath := filepath.Join(cacheRoot, depName, depCommit, "files", "remote", "types.proto")
	mustWrite(t, remoteProtoPath, `syntax = "proto3";

package remote.v1;

// Remote is the type the in-workspace consumer refers to.
message Remote {
  string field = 1;
}
`)

	// Initialize WITH the cache-root override pre-installed. The
	// override must be set BEFORE initialize runs so the first
	// LoadProtoIndex call uses it.
	srv, _ := newServerWithFakeConn(t)
	srv.SetWatchedFilesWindow(0)
	srv.SetProtoIndexCacheRoot(cacheRoot)
	params := &protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{Name: "buf-lock-mid-session-test"},
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{URI: string(uri.File(root)), Name: "root"},
		},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				DidChangeWatchedFiles: &protocol.DidChangeWatchedFilesWorkspaceClientCapabilities{
					DynamicRegistration: true,
				},
			},
		},
	}
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))
	initCall, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	// Open the consumer doc. The hover handler reads from the
	// index that was just built — which doesn't include the dep
	// yet because the lock had no deps.
	consumerURI := "file://" + filepath.ToSlash(consumerPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, consumerURI, 1, mustRead(t, consumerPath))))

	// Sanity: the index instance is in place and the dep file
	// is NOT yet reachable.
	indexBefore := srv.SnapshotProtoIndex()
	require.NotNil(t, indexBefore, "precondition: initial index built")
	assert.Nil(t, indexBefore.Lookup("remote/types.proto"),
		"precondition: empty buf.lock → dep file is not in the index")

	// Phase 1: hover on the dep type before the lock update.
	// Line 7 (0-indexed) is `  remote.v1.Remote ref = 1;`.
	// Character 14 lands inside "Remote". With the dep absent
	// from the index, the cross-file resolver can't reach the
	// definition.
	defsBefore := definitionFromHandle(t, srv, consumerURI, 7, 14)
	assert.Empty(t, defsBefore,
		"precondition: dep type can't resolve before buf.lock adds the dep")

	// Phase 2: write the new buf.lock and fire the watcher event.
	// classifyWatchedEvents routes the lock change through
	// reloadProtoIndex → LoadProtoIndex.
	require.NoError(t, os.WriteFile(lockPath, []byte(`version: v2
deps:
  - name: `+depName+`
    commit: `+depCommit+`
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(lockPath), Type: protocol.FileChangeTypeChanged},
		})))

	// Phase 3: after the rebuild, the dep file IS in the index.
	indexAfter := srv.SnapshotProtoIndex()
	require.NotNil(t, indexAfter)
	assert.NotSame(t, indexBefore, indexAfter,
		"buf.lock change must swap in a fresh index instance")
	require.NotNil(t, indexAfter.Lookup("remote/types.proto"),
		"after the lock update, the dep file resolves through the cache root")

	// And — the load-bearing assertion — the in-doc reference
	// resolves end-to-end. Same position as the pre-update query;
	// the only thing that changed is the index's include-path list.
	defsAfter := definitionFromHandle(t, srv, consumerURI, 7, 14)
	require.Len(t, defsAfter, 1,
		"hover/def must resolve through the freshly-added dep without an LSP restart")
	expectedURI := uri.URI("file://" + filepath.ToSlash(remoteProtoPath))
	assert.Equal(t, expectedURI, defsAfter[0].URI,
		"definition lands on the BSR-cached dep file")
}
