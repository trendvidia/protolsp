// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Top-level Server.Close lifecycle tests. The sub-workspace
// analogue is covered by TestSubWorkspace_Close_IsIdempotent +
// TestSubWorkspace_Close_CancelsInFlightRebuild in
// workspace_lookup_test.go; this file mirrors them for the
// top-level Server.Close path.

package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestServerClose_IsIdempotent confirms calling Close twice doesn't
// panic, double-free, or leak goroutines. Transports today call
// Close on both the shutdown notification path AND the connection
// EOF path; without idempotency the second call could try to close
// an already-closed RPC server or re-iterate a torn workspace map.
func TestServerClose_IsIdempotent(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	// Stand up the RPC sidecar so Close has more state to tear
	// down — without this we'd skip the stopRPC branch entirely.
	_, err := srv.startRPC()
	require.NoError(t, err)

	require.NoError(t, srv.Close())
	require.NoError(t, srv.Close(),
		"second Close must be a no-op — no panic, no double-free, no second teardown side effect")
	require.NoError(t, srv.Close(),
		"third Close to pin the idempotency contract")
}

// TestServerClose_StopsWatchedFilesTimer pairs with the
// per-document analyze-timer test in cancellation_test.go: the
// watched-files debounce timer is a SEPARATE timer with its own
// state, and Server.Close must stop it too. Otherwise a fired
// timer reaches into a half-torn workspace state and triggers
// LoadWorkspaceFileConfig / reanalyzeOpenDocs on closed
// resources.
func TestServerClose_StopsWatchedFilesTimer(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	srv.SetWatchedFilesWindow(50 * time.Millisecond)

	// Schedule a watched-files notification. The handler stores
	// the event and starts the timer; Close should stop it.
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodWorkspaceDidChangeWatchedFiles,
		&protocol.DidChangeWatchedFilesParams{
			Changes: []protocol.FileEvent{
				{URI: uri.URI("file:///work/.protolsp.pxf"), Type: protocol.FileChangeTypeChanged},
			},
		})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	// Timer must be set right after the notification — confirms the
	// preconditions of the test before we exercise Close.
	timerWasSet, _ := srv.WatchedFilesDebounce()
	require.True(t, timerWasSet,
		"precondition: watched-files notification must arm the debounce timer")

	require.NoError(t, srv.Close())

	// After Close, the timer must be nil. If it were still pending,
	// it could fire after Close and run the watched-files pipeline
	// against a torn workspace.
	timerAfterClose, pendingAfterClose := srv.WatchedFilesDebounce()
	assert.False(t, timerAfterClose,
		"Close must stop the watched-files debounce timer")
	assert.Zero(t, pendingAfterClose,
		"Close must drop the pending watched-files events buffer")

	// Wait well past the window — if the timer somehow snuck a
	// fire in, it would have happened by now. The fact that Close
	// already nil'd the fields above is the primary assertion;
	// this just guards against a future regression that races the
	// timer's own callback against Close.
	time.Sleep(100 * time.Millisecond)
}
