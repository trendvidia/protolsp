// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"
)

// TestSubWorkspace_EvictsAfterLastDocClosed is R4's positive
// case: a sub-workspace loads on first didOpen, persists while
// any of its docs is open, and evicts the moment the last one
// closes. Re-opening triggers a fresh load.
func TestSubWorkspace_EvictsAfterLastDocClosed(t *testing.T) {
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	_, loaded := srv.SubWorkspaceByDir(svcA)
	require.True(t, loaded, "didOpen on a sub-workspace doc must load its state")

	// Close — should trigger eviction.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didCloseCall(t, string(devURI))))

	_, stillLoaded := srv.SubWorkspaceByDir(svcA)
	assert.False(t, stillLoaded,
		"closing the last doc in a sub-workspace must evict its state — otherwise long-lived sessions leak entries per the audit's R4")
}

// TestSubWorkspace_NotEvictedWhileOtherDocOpen pins the other
// side: closing one of two docs in the same sub-workspace must
// keep the workspace loaded.
func TestSubWorkspace_NotEvictedWhileOtherDocOpen(t *testing.T) {
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	prodURI := uri.File(filepath.Join(svcA, "env", "prod.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	// Close one. The other still references svcA.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didCloseCall(t, string(devURI))))

	_, stillLoaded := srv.SubWorkspaceByDir(svcA)
	assert.True(t, stillLoaded,
		"sub-workspace must persist while any of its docs is open")
}

// TestSubWorkspace_ReDiscoversAfterEviction pins that eviction
// is reversible: after the last doc closes (evict) and a new
// one opens, ensureSubWorkspace re-discovers and reloads.
func TestSubWorkspace_ReDiscoversAfterEviction(t *testing.T) {
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didCloseCall(t, string(devURI))))

	_, presentAfterClose := srv.SubWorkspaceByDir(svcA)
	require.False(t, presentAfterClose,
		"precondition: eviction ran on close")

	// Re-open: ensureSubWorkspace runs again.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 2, "@type c4test.v1.AppConfig\n")))

	ws, reloaded := srv.SubWorkspaceByDir(svcA)
	assert.True(t, reloaded, "re-opening a doc in the evicted dir must trigger a fresh discover-and-load")
	assert.NotNil(t, ws.FileConfig,
		"reloaded sub-workspace must have its FileConfig populated, same as the initial load")
}

// TestSubWorkspace_RootDocCloseDoesNotEvict pins that root-
// workspace state is never touched by the eviction path —
// root state lives on Server directly, not in the sub-workspace map,
// so the eviction is moot but the helper must still no-op
// cleanly when WorkspaceDir equals the root.
func TestSubWorkspace_RootDocCloseDoesNotEvict(t *testing.T) {
	root, _, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	// Open a doc directly at the root (not under any sub-
	// workspace). Its WorkspaceDir equals the root.
	rootDoc := uri.File(filepath.Join(root, "root.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(rootDoc), 1, "@type c4test.v1.AppConfig\n")))

	// Close. MaybeEvictSubWorkspace must short-circuit for the
	// root dir; nothing in the sub-workspace map touched.
	before := srv.SubWorkspaceCount()
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didCloseCall(t, string(rootDoc))))
	after := srv.SubWorkspaceCount()
	assert.Equal(t, before, after,
		"closing a root-workspace doc must not mutate the sub-workspace map")
}
