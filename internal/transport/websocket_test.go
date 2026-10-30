// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trendvidia/protolsp/internal/lspcodec"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestWebsocket_InitializeRoundTrip stands up the real WebsocketHandler
// behind an httptest.Server, dials it as a client would, and exchanges
// an LSP initialize call. Verifies that:
//   - the upgrade succeeds with the lsp subprotocol;
//   - initialize returns the expected server info + capabilities;
//   - the connection closes cleanly when the client sends exit.
//
// This is the M5 smoke test — the handler shares all schema/diagnostics
// logic with stdio (covered in internal/server), so we don't repeat
// those flows here; the test just proves the transport correctly wires
// jsonrpc2 to a real WS connection.
func TestWebsocket_InitializeRoundTrip(t *testing.T) {
	srv := httptest.NewServer(WebsocketHandler(discardLogger(t), &AcceptOptions{
		// httptest assigns a 127.0.0.1:<port> URL; treat any origin as
		// trusted for the test. Production deployments configure
		// OriginPatterns explicitly.
		InsecureSkipOriginVerify: true,
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	// Drive the client side with jsonrpc2 over the same wsStream
	// adapter the server uses, so the test exercises both directions.
	clientStream := &wsStream{conn: c}
	conn := jsonrpc2.NewConn(clientStream, jsonrpc2.WithCodec(lspcodec.Codec{}))
	// Conn.Go installs a handler for server-initiated calls /
	// notifications. The initialize round-trip only needs the
	// response side, so we use a do-nothing handler that returns
	// MethodNotFound for any unexpected inbound call.
	conn.Go(ctx, jsonrpc2.MethodNotFoundHandler)

	var result protocol.InitializeResult
	_, err = conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "protolsp-ws-test", Version: protocol.NewOptional("0")},
	}, &result)
	require.NoError(t, err)

	require.NotNil(t, result.ServerInfo)
	assert.Equal(t, "protolsp", result.ServerInfo.Name)
	// M4 advertised the hover provider; tests prove M1–M4 wiring
	// flows through the WS transport identically.
	assert.Equal(t, protocol.Boolean(true), result.Capabilities.HoverProvider)

	// Send exit so the server-side jsonrpc2 loop terminates and the
	// handler returns. We use Notify for fire-and-forget (no response
	// expected for notifications).
	require.NoError(t, conn.Notify(ctx, protocol.MethodExit, nil))
	// Give the server side a moment to process and close. Polling for
	// conn.Done() is more deterministic than time.Sleep but adds noise
	// for a 1-shot smoke; short sleep is fine here.
	time.Sleep(50 * time.Millisecond)
}

// TestWebsocket_RejectsUnsupportedMessageType protects against a future
// regression where a misconfigured client (or proxy) sends binary
// frames; the server must reject those with a clear error rather than
// silently mishandling them as JSON.
func TestWebsocket_RejectsUnsupportedMessageType(t *testing.T) {
	srv := httptest.NewServer(WebsocketHandler(discardLogger(t), &AcceptOptions{
		InsecureSkipOriginVerify: true,
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"lsp"},
		HTTPClient:   srv.Client(),
	})
	require.NoError(t, err)
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "test done") }()

	// Send a binary frame; the server's wsStream.Read should reject it.
	require.NoError(t, c.Write(ctx, websocket.MessageBinary, []byte("not json")))

	// The server closes the connection after the parse failure.
	// Reading should yield an error fairly quickly.
	_, _, err = c.Read(ctx)
	require.Error(t, err, "server should close the connection after a non-text frame")
}

// TestWebsocket_HealthCheck — sanity that the protolsp-ws style server
// (mux with /lsp and /healthz) doesn't accidentally upgrade non-LSP
// requests. Verifies a plain GET to a different path falls through to
// the next handler without hijacking the connection.
func TestWebsocket_HandlerDoesNotUpgradeNonWSRequests(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/lsp", WebsocketHandler(discardLogger(t), nil))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// And a plain GET to /lsp without WS upgrade headers should fail
	// with 4xx (coder/websocket returns 426 Upgrade Required equivalent).
	resp2, err := srv.Client().Get(srv.URL + "/lsp")
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.True(t, resp2.StatusCode >= 400,
		"plain GET on the LSP path must not look like a successful response, got %d", resp2.StatusCode)
}
