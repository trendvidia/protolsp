// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// WebSocket close-code differentiation tests. websocket.go:113-118
// codes specifically map StatusNormalClosure + StatusGoingAway to
// a graceful nil return; anything else falls through as a real
// error. The existing TestWebsocket_InitializeRoundTrip exercises
// the graceful path implicitly; this file covers the abnormal-
// close branch so a future change to the switch (or to coder/
// websocket's CloseError shape) gets caught.

package transport

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"

	"net/http/httptest"
)

// syncBuffer is a goroutine-safe wrapper around *bytes.Buffer.
// The handler's Warn log fires from a different goroutine than the
// test's poll loop reads; bytes.Buffer isn't safe for that mix.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func captureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, buf
}

// TestWebsocket_AbnormalClose_SurfacesAsError pins the
// non-graceful branch of the CloseError switch in serveOverWS.
// When a client tears the connection down with a code outside
// {NormalClosure, GoingAway}, the server's serveOverWS must
// return a non-nil error, which the outer handler logs as
// "ws session ended with error". The same client-side close
// with StatusNormalClosure produces no warning — that's the
// existing happy-path test's contract.
func TestWebsocket_AbnormalClose_SurfacesAsError(t *testing.T) {
	logger, buf := captureLogger()
	srv := httptest.NewServer(WebsocketHandler(logger, &AcceptOptions{
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

	// Drive one initialize so we're past the WS upgrade boundary
	// and inside the jsonrpc2 dispatch loop. Without this the
	// abrupt close races the dispatcher startup and may land in
	// jsonrpc2's accept path instead of its read path — close
	// behavior may still be benign but the test is about pinning
	// the documented error-mapping logic, which only matters once
	// the dispatcher is running.
	conn := jsonrpc2.NewConn(&wsStream{conn: c})
	conn.Go(ctx, jsonrpc2.MethodNotFoundHandler)
	var result protocol.InitializeResult
	_, err = conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{Name: "ws-close-test", Version: "0"},
	}, &result)
	require.NoError(t, err)

	// Close with an abnormal code. StatusInternalError (1011)
	// is well-defined and falls outside the
	// {NormalClosure, GoingAway} graceful set.
	require.NoError(t, c.Close(websocket.StatusInternalError, "test injection"))

	// The server's serveOverWS observes the abnormal CloseError,
	// returns it, and WebsocketHandler logs "ws session ended
	// with error". Poll the log buffer rather than sleeping
	// blindly.
	waitForLog(t, buf, `"msg":"ws session ended with error"`, 2*time.Second)
}

// TestWebsocket_NormalClose_IsSilent pairs with the abnormal-
// close test: when the client closes with StatusNormalClosure,
// no warning fires. Pinning both sides of the switch keeps a
// future regression (e.g., dropping the NormalClosure case)
// loud — the abnormal test would still pass, but THIS one would
// flip to a warning log we'd notice.
func TestWebsocket_NormalClose_IsSilent(t *testing.T) {
	logger, buf := captureLogger()
	srv := httptest.NewServer(WebsocketHandler(logger, &AcceptOptions{
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

	conn := jsonrpc2.NewConn(&wsStream{conn: c})
	conn.Go(ctx, jsonrpc2.MethodNotFoundHandler)
	var result protocol.InitializeResult
	_, err = conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{Name: "ws-close-test", Version: "0"},
	}, &result)
	require.NoError(t, err)

	require.NoError(t, c.Close(websocket.StatusNormalClosure, "test done"))

	// Give the server side a moment to process the close. We DON'T
	// expect a warning; this is "absence of evidence" so we have
	// to wait long enough that a regression would have surfaced.
	time.Sleep(150 * time.Millisecond)
	assert.NotContains(t, buf.String(), `"msg":"ws session ended with error"`,
		"NormalClosure must NOT log a warning — that's the graceful path")
}

// waitForLog polls buf for a substring with a generous deadline.
// Mirrors the server-package helper so the transport-side test
// file is self-contained.
func waitForLog(t *testing.T, buf *syncBuffer, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waitForLog: timed out waiting for %q in:\n%s", want, buf.String())
}
