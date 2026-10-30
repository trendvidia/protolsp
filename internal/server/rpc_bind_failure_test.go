// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Sidecar bind-failure contract test. rpc_server.go documents:
//
//	"Any error (port exhaustion, token generation failure) is
//	 fatal for the sidecar but non-fatal for protolsp overall —
//	 the caller logs and continues with LSP-only mode."
//
// The dispatchInitialize callsite mirrors that with a Warn log.
// Without a test, a future refactor that lifts the bind error up
// the call chain (or that takes the whole server down on a port
// conflict) would silently break the contract for every user
// whose chosen loopback port is already occupied — a real
// scenario when two protolsp instances share a developer's
// machine.

package server

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// TestRPC_BindFailure_LSPKeepsServing pre-seeds s.rpc with the
// listenAddr of a port we've already bound, then drives
// dispatchInitialize through the real handler. The sidecar must
// fail to bind, the server must log + continue, and a follow-up
// LSP method must still succeed. Pins the documented contract that
// a broken sidecar leaves LSP-only mode operational.
func TestRPC_BindFailure_LSPKeepsServing(t *testing.T) {
	// Occupy a loopback port — startRPC's bind into the same
	// address will fail with EADDRINUSE.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })
	occupiedAddr := occupied.Addr().String()

	srv, _ := newServerWithFakeConn(t)
	// Pre-seed the rpc struct with the occupied address so the
	// listenAddr override takes effect when dispatchInitialize
	// calls startRPC.
	srv.rpc = &rpcServer{listenAddr: occupiedAddr}

	// initialize must succeed even though startRPC will fail.
	params := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "bind-fail-test"},
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)

	var initResult any
	gotResult := false
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr,
			"initialize must NOT surface the bind failure as an LSP error")
		initResult = result
		gotResult = result != nil
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	require.True(t, gotResult, "initialize must reply with a result even when the sidecar can't bind")

	// The RPC sidecar must NOT be running — startRPC returned
	// an error so started stayed false.
	srv.rpc.mu.Lock()
	started := srv.rpc.started
	srv.rpc.mu.Unlock()
	assert.False(t, started, "bind failure must leave the sidecar in the stopped state")

	// Sanity: the result still carries server capabilities; we
	// didn't truncate the response just because the rpc field
	// went missing.
	require.NotNil(t, initResult,
		"initialize must produce a non-nil result for LSP-only clients")

	// Follow-up LSP method must work — symbol on an unopened doc
	// is the cheapest no-side-effect probe.
	probeCall, err := newTestCall(jsonrpc2.NewNumberID(2),
		protocol.MethodTextDocumentDocumentSymbol,
		&protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: "file:///nope.pxf"},
		})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, probeCall),
		"LSP requests must still route after the sidecar failed to bind")
}
