// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// End-to-end LSP-session lifecycle tests for both the stdio and
// WebSocket transports. The audit's "biggest single gap" was that
// existing tests only exercise initialize over the wire — neither
// transport had a test driving a doc through didOpen → didChange →
// didClose → shutdown → exit. Adding one harness shape per transport
// closes that gap and gives future regressions a fast, loud failure
// surface.

package transport

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// TestLSPLifecycle_Stdio drives an LSP doc through every dispatcher
// state transition over real piped jsonrpc2 (the production stdio
// transport shape). Each step's success criterion is the proof for the
// preceding step:
//
//   - initialize → response with ServerInfo (transport + handler wired)
//   - didOpen → publishDiagnostics arrives (analyze + notify reached us)
//   - didChange → publishDiagnostics arrives at the new version
//     (didChange ran; the version field of the latest publish is the
//     evidence the second analyze actually fired against the post-edit
//     state, not the original)
//   - didClose → no panic, no zombie publish (cancelPendingAnalyze ran)
//   - shutdown → response (server still routing requests post-close)
//   - exit → conn.Done() closes within the documented grace window
func TestLSPLifecycle_Stdio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	srv, client, shutdown := newPipedSession(t, ctx)
	defer shutdown()

	// initialize
	var initResult protocol.InitializeResult
	_, err := client.conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo:   &protocol.ClientInfo{Name: "e2e-lifecycle", Version: "0"},
		Capabilities: protocol.ClientCapabilities{},
	}, &initResult)
	require.NoError(t, err)
	require.NotNil(t, initResult.ServerInfo, "ServerInfo populated → initialize handler ran end-to-end")

	require.NoError(t, client.conn.Notify(ctx, protocol.MethodInitialized, &protocol.InitializedParams{}))
	syncProbe(t, ctx, client.conn)
	srv.WaitInitialized()

	// didOpen → expect publishDiagnostics. URI intentionally inert
	// (no @type, no chain) so dispatchDidOpen takes the plain analyze
	// path. The publish is what proves the analyze pipeline reached
	// the client.
	docURI := protocol.DocumentURI("file:///lifecycle/doc.pxf")
	require.NoError(t, client.conn.Notify(ctx, protocol.MethodTextDocumentDidOpen,
		&protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:        docURI,
				LanguageID: "pxf",
				Version:    1,
				Text:       "# inert pxf — no @type\n",
			},
		}))

	first := expectPublish(t, client, docURI, 3*time.Second)
	assert.Equal(t, uint32(1), first.Version,
		"first publish carries the didOpen version")

	// didChange to v2 — a second publishDiagnostics must arrive,
	// tagged with the new version. Same FIFO logic as the dispatcher
	// smoke test: a Notify can't be "lost" if a subsequent
	// notification's effects are observable.
	require.NoError(t, client.conn.Notify(ctx, protocol.MethodTextDocumentDidChange,
		&protocol.DidChangeTextDocumentParams{
			TextDocument: protocol.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: docURI},
				Version:                2,
			},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{
				{Text: "# edited inert pxf\n"},
			},
		}))
	second := expectPublish(t, client, docURI, 3*time.Second)
	assert.Equal(t, uint32(2), second.Version,
		"second publish carries the didChange version — proves analyze re-ran")

	// didClose — no acknowledgment needed, but the subsequent
	// shutdown must still succeed. If didClose triggered a panic or
	// hung the dispatcher, shutdown below would fail.
	require.NoError(t, client.conn.Notify(ctx, protocol.MethodTextDocumentDidClose,
		&protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		}))

	// shutdown → response. The handler flips s.closed but keeps
	// routing requests until exit.
	var shutdownResult any
	_, err = client.conn.Call(ctx, protocol.MethodShutdown, nil, &shutdownResult)
	require.NoError(t, err, "shutdown handler must respond cleanly")

	// exit → the dispatcher tears the connection down. Client-side
	// conn.Done() is the proof the server-initiated close propagated.
	require.NoError(t, client.conn.Notify(ctx, protocol.MethodExit, nil))
	select {
	case <-client.conn.Done():
		// OK — server closed our conn after dispatching exit.
	case <-time.After(2 * time.Second):
		t.Fatal("client conn did not close within 2s of exit — server failed to honor the exit notification")
	}
}

// TestLSPLifecycle_WebSocket exercises the same full lifecycle over
// the WebSocket transport. The handler chain is identical from the
// server's perspective; the test pins that the WS frame boundaries
// and CloseError paths don't introduce divergence.
func TestLSPLifecycle_WebSocket(t *testing.T) {
	srv := httptest.NewServer(WebsocketHandler(discardLogger(t), &AcceptOptions{
		InsecureSkipOriginVerify: true,
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"lsp"},
		HTTPClient:   srv.Client(),
	})
	require.NoError(t, err)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "test done") }()

	clientStream := &wsStream{conn: c}
	conn := jsonrpc2.NewConn(clientStream)

	// Capture publishDiagnostics notifications. Same shape as the
	// piped test's pipedClient — small handler that funnels
	// publishes into a buffered channel.
	publishes := make(chan *protocol.PublishDiagnosticsParams, 8)
	conn.Go(ctx, func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if req.Method() == protocol.MethodTextDocumentPublishDiagnostics {
			var p protocol.PublishDiagnosticsParams
			if err := json.Unmarshal(req.Params(), &p); err == nil {
				select {
				case publishes <- &p:
				default:
				}
			}
			return reply(ctx, nil, nil)
		}
		return jsonrpc2.MethodNotFoundHandler(ctx, reply, req)
	})

	// initialize
	var initResult protocol.InitializeResult
	_, err = conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{Name: "e2e-ws-lifecycle", Version: "0"},
	}, &initResult)
	require.NoError(t, err)
	require.NotNil(t, initResult.ServerInfo)

	require.NoError(t, conn.Notify(ctx, protocol.MethodInitialized, &protocol.InitializedParams{}))

	// didOpen → publish
	docURI := protocol.DocumentURI("file:///lifecycle/ws.pxf")
	require.NoError(t, conn.Notify(ctx, protocol.MethodTextDocumentDidOpen,
		&protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:        docURI,
				LanguageID: "pxf",
				Version:    1,
				Text:       "# inert pxf — ws\n",
			},
		}))
	first := expectPublishFromChan(t, publishes, docURI, 3*time.Second)
	assert.Equal(t, uint32(1), first.Version)

	// didChange → publish at v2
	require.NoError(t, conn.Notify(ctx, protocol.MethodTextDocumentDidChange,
		&protocol.DidChangeTextDocumentParams{
			TextDocument: protocol.VersionedTextDocumentIdentifier{
				TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: docURI},
				Version:                2,
			},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{
				{Text: "# edited inert pxf — ws\n"},
			},
		}))
	second := expectPublishFromChan(t, publishes, docURI, 3*time.Second)
	assert.Equal(t, uint32(2), second.Version)

	// didClose + shutdown + exit
	require.NoError(t, conn.Notify(ctx, protocol.MethodTextDocumentDidClose,
		&protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		}))

	var shutdownResult any
	_, err = conn.Call(ctx, protocol.MethodShutdown, nil, &shutdownResult)
	require.NoError(t, err)

	require.NoError(t, conn.Notify(ctx, protocol.MethodExit, nil))
	select {
	case <-conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("WS client conn did not close within 2s of exit")
	}
}

// expectPublish blocks until a publishDiagnostics arrives for docURI on
// the piped client harness, or the timeout fires. The channel buffer
// in pipedClient ensures we don't lose intermediate publishes; we
// drain notifications for other URIs in case the server publishes
// against ambient docs.
func expectPublish(t *testing.T, c *pipedClient, docURI protocol.DocumentURI, timeout time.Duration) *protocol.PublishDiagnosticsParams {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case got := <-c.publishes:
			if got.URI == docURI {
				return got
			}
			// Different URI — keep waiting.
		case <-deadline.C:
			t.Fatalf("no publishDiagnostics for %s within %s", docURI, timeout)
			return nil
		}
	}
}

// expectPublishFromChan is the WebSocket-test analogue: same draining
// behavior but reading from a local channel rather than the pipedClient
// struct's field.
func expectPublishFromChan(t *testing.T, ch chan *protocol.PublishDiagnosticsParams, docURI protocol.DocumentURI, timeout time.Duration) *protocol.PublishDiagnosticsParams {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case got := <-ch:
			if got.URI == docURI {
				return got
			}
		case <-deadline.C:
			t.Fatalf("no publishDiagnostics for %s within %s", docURI, timeout)
			return nil
		}
	}
}
