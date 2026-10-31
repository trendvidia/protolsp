// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"io"
	"log/slog"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// captureReplier records the result + error a handler passes to its
// testReplier. Used as a stand-in for a live connection in tests.
type captureReplier struct {
	called bool
	result interface{}
	err    error
}

func (c *captureReplier) reply(_ context.Context, result interface{}, err error) error {
	c.called = true
	c.result = result
	c.err = err
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestHandle_InitializeAdvertisesTextDocumentSync(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test", Version: protocol.NewOptional("0")},
	})
	require.NoError(t, err)

	require.NoError(t, testHandle(srv, context.Background(), rep.reply, call))
	require.True(t, rep.called, "Handle must reply to an initialize call")
	require.NoError(t, rep.err)

	result, ok := rep.result.(*initializeResult)
	require.True(t, ok, "initialize result should be *initializeResult, got %T", rep.result)

	syncOpts, ok := result.Capabilities.TextDocumentSync.(*protocol.TextDocumentSyncOptions)
	require.True(t, ok, "TextDocumentSync should be *TextDocumentSyncOptions")
	assert.True(t, *syncOpts.OpenClose, "openClose must be true for M1+")
	assert.Equal(t, protocol.TextDocumentSyncKindIncremental, *syncOpts.Change,
		"I1: server advertises incremental sync")

	require.NotNil(t, result.ServerInfo)
	assert.Equal(t, "protolsp", result.ServerInfo.Name)
	// A test binary has no module version, so any literal here differs.
	assert.Equal(t, develVersion, result.ServerInfo.Version)

	// General block folding is advertised as a standard
	// pull-based provider so editors enable textDocument/foldingRange.
	assert.Equal(t, protocol.Boolean(true), result.Capabilities.FoldingRangeProvider,
		"FoldingRangeProvider must be advertised")
}

func TestModuleVersion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"release tag", "v0.24.0", "0.24.0"},
		{"pseudo-version", "v0.23.2-0.20261031000737-cfb40a2b6522", "0.23.2-0.20261031000737-cfb40a2b6522"},
		{"dirty tree", "v0.23.2-0.20261031000737-cfb40a2b6522+dirty", "0.23.2-0.20261031000737-cfb40a2b6522+dirty"},
		{"devel", "(devel)", "(devel)"},
		{"empty", "", "(devel)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := &debug.BuildInfo{Main: debug.Module{Version: tc.in}}
			assert.Equal(t, tc.want, moduleVersion(info))
		})
	}
}

func TestHandle_InitializedIsNoOp(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	notif, err := newTestNotification(protocol.MethodInitialized, &protocol.InitializedParams{})
	require.NoError(t, err)

	// Notifications go through reply with nil/nil per the jsonrpc2
	// contract; the framework treats it as "no response to send."
	require.NoError(t, testHandle(srv, context.Background(), rep.reply, notif))
}

func TestHandle_ShutdownMarksClosed(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	call, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodShutdown, nil)
	require.NoError(t, err)

	require.NoError(t, testHandle(srv, context.Background(), rep.reply, call))
	assert.True(t, rep.called)
	assert.True(t, srv.closed.Load(), "shutdown must flip the closed flag")
}

func TestHandle_ExitClosesConnectionAndReturnsNil(t *testing.T) {
	// Exit closes the conn asynchronously (so jsonrpc2's read loop
	// surfaces a single EOF rather than racing two error types through
	// fail's atomic.Value.Store). Without an attached conn (as in this
	// minimal test), Handle just returns nil cleanly.
	srv := New(discardLogger())
	rep := &captureReplier{}

	notif, err := newTestNotification(protocol.MethodExit, nil)
	require.NoError(t, err)

	err = testHandle(srv, context.Background(), rep.reply, notif)
	require.NoError(t, err)
	assert.False(t, rep.called, "exit is a notification — no reply expected")
}

func TestHandle_UnknownCallReturnsMethodNotFound(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	// Pick any method the dispatcher doesn't recognize. Update if/when
	// the next feature lands and starts handling whatever's here.
	call, err := newTestCall(jsonrpc2.NewNumberID(3), "textDocument/colorPresentation", nil)
	require.NoError(t, err)

	require.NoError(t, testHandle(srv, context.Background(), rep.reply, call))
	require.True(t, rep.called)
	require.Error(t, rep.err)

	var jsErr *jsonrpc2.Error
	require.ErrorAs(t, rep.err, &jsErr)
	assert.Equal(t, jsonrpc2.MethodNotFound, jsErr.Code)
}

func TestHandle_UnknownNotificationIsSilentlyDropped(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	notif, err := newTestNotification("$/setTrace", nil)
	require.NoError(t, err)

	require.NoError(t, testHandle(srv, context.Background(), rep.reply, notif))
	assert.False(t, rep.called, "unknown notifications must not produce a reply")
}
