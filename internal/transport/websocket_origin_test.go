// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Origin verification + max-message-bytes tests for the WebSocket
// transport. The existing websocket_test.go suite uses
// InsecureSkipOriginVerify: true for every dial, so the production
// origin-checking path had no regression coverage — a future
// upstream change to coder/websocket's enforcement could silently
// break the hosted Monaco IDE's cross-origin guard.

package transport

import (
	"context"
	"encoding/json"
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

// TestWebsocket_OriginPattern_AllowsMatch confirms a dial whose
// Origin header matches a configured pattern completes the upgrade
// and exchanges an LSP message. The pattern set is the
// production-shaped configuration — explicit allow-list, no
// InsecureSkipOriginVerify flag.
func TestWebsocket_OriginPattern_AllowsMatch(t *testing.T) {
	srv := httptest.NewServer(WebsocketHandler(discardLogger(t), &AcceptOptions{
		OriginPatterns: []string{"allowed.example"},
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Origin", "https://allowed.example")

	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"lsp"},
		HTTPClient:   srv.Client(),
		HTTPHeader:   hdr,
	})
	require.NoError(t, err, "matching Origin must succeed under the configured pattern")
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "test done") }()

	// Exchange one initialize call to prove the connection is
	// actually live — a 101 Switching Protocols alone wouldn't
	// catch a regression where the upgrade succeeds but the
	// dispatcher fails to attach.
	conn := jsonrpc2.NewConn(&wsStream{conn: c}, jsonrpc2.WithCodec(lspcodec.Codec{}))
	conn.Go(ctx, jsonrpc2.MethodNotFoundHandler)
	var result protocol.InitializeResult
	_, err = conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "origin-test", Version: protocol.NewOptional("0")},
	}, &result)
	require.NoError(t, err)
	assert.NotNil(t, result.ServerInfo)
}

// TestWebsocket_OriginPattern_RejectsMismatch is the negative half:
// a dial whose Origin doesn't appear in OriginPatterns must fail at
// the upgrade boundary, NEVER reaching wsStream / dispatcher code.
// Catches an upstream-rule shift that would silently widen the
// allowed origin set.
func TestWebsocket_OriginPattern_RejectsMismatch(t *testing.T) {
	srv := httptest.NewServer(WebsocketHandler(discardLogger(t), &AcceptOptions{
		OriginPatterns: []string{"allowed.example"},
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Origin", "https://evil.example")

	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"lsp"},
		HTTPClient:   srv.Client(),
		HTTPHeader:   hdr,
	})
	require.Error(t, err, "mismatched Origin must be rejected at upgrade")
	if c != nil {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// TestWebsocket_OriginPattern_RejectsMissingOriginUnderSameOriginRule
// pins the "empty OriginPatterns + InsecureSkipOriginVerify=false"
// branch: the zero-value AcceptOptions is documented as
// "same-origin only". A dial whose Origin is foreign to the host
// must be refused. This is the default safe posture for the
// hosted-Monaco deployment.
func TestWebsocket_OriginPattern_RejectsMissingOriginUnderSameOriginRule(t *testing.T) {
	srv := httptest.NewServer(WebsocketHandler(discardLogger(t), &AcceptOptions{}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Origin", "https://elsewhere.example")

	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"lsp"},
		HTTPClient:   srv.Client(),
		HTTPHeader:   hdr,
	})
	require.Error(t, err, "foreign Origin under same-origin rule must be rejected")
	if c != nil {
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// TestWebsocket_MaxMessageBytes_EnforcedOnOversizeFrame confirms the
// SetReadLimit guard: a single inbound frame larger than the
// configured MaxMessageBytes closes the connection. Without this
// test a future regression that fails to call SetReadLimit (or
// passes the wrong number) would silently lift the cap and let a
// hostile client OOM the server.
func TestWebsocket_MaxMessageBytes_EnforcedOnOversizeFrame(t *testing.T) {
	// Tight limit so we can construct a single oversize frame
	// cheaply. Still well above the LSP initialize payload so the
	// upgrade + a small follow-up message stay legal.
	const limit = 4096
	srv := httptest.NewServer(WebsocketHandler(discardLogger(t), &AcceptOptions{
		InsecureSkipOriginVerify: true,
		MaxMessageBytes:          limit,
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

	// Build a JSON-RPC message whose serialized form is far above
	// the limit. The "garbage" param ensures it serializes to a
	// long enough payload regardless of jsonrpc2 envelope size.
	type bigParams struct {
		Garbage string `json:"garbage"`
	}
	pad := strings.Repeat("x", limit*2)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  bigParams{Garbage: pad},
	})
	require.NoError(t, err)
	require.Greater(t, len(body), limit, "test fixture must exceed the configured limit")

	// Send the oversize frame; the server's read-limit guard
	// triggers, the connection terminates. The Write itself may
	// succeed (the bytes hit the wire before the server's read
	// fails), but the next Read MUST return an error indicating
	// the closure.
	_ = c.Write(ctx, websocket.MessageText, body)

	_, _, err = c.Read(ctx)
	require.Error(t, err,
		"server must close the connection after an oversize frame; got nil error which means the limit was lifted")
}
