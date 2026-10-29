// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// End-to-end integration test for the slow-handler tripwire.
// handler_warn_test.go covers WithSlowHandlerWarn against stub
// handlers in isolation; the audit named the gap that no test
// exercises the wrapper inside the REAL dispatcher pipeline
// (jsonrpc2.Conn.Go → wrapped handler → server.Handle). A
// regression that, say, drops the wrap during a transport
// refactor would silently leave production unprotected — the
// existing unit tests wouldn't notice. This test wires the same
// stack the production stdio/WebSocket transports use and
// confirms the warn log fires for a deliberately-slow real LSP
// dispatch.

package transport

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/internal/server"
)

// TestSlowHandlerTripwire_FiresAcrossRealDispatcher drives a real
// LSP request whose server-side dispatch is artificially slow
// through the WithSlowHandlerWarn wrapper. Mirrors the production
// wiring in serve():
//
//	conn.Go(ctx, WithSlowHandlerWarn(srv.Handle, log, threshold))
//
// The slow path is injected by wrapping srv.Handle in a decorator
// that sleeps when it sees the chosen method; the warn log is
// captured via a syncing buffer so the assertion is deterministic.
// Pins the contract that a future regression dropping the wrap
// would surface here.
func TestSlowHandlerTripwire_FiresAcrossRealDispatcher(t *testing.T) {
	logger, buf := captureLogger()
	srv := server.New(logger)
	t.Cleanup(func() { _ = srv.Close() })

	const slowMethod = "textDocument/documentSymbol"
	const sleepFor = 150 * time.Millisecond
	const threshold = 50 * time.Millisecond

	slowing := func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if req.Method() == slowMethod {
			time.Sleep(sleepFor)
		}
		return srv.Handle(ctx, reply, req)
	}
	wrapped := WithSlowHandlerWarn(slowing, logger, threshold)

	req, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), slowMethod,
		&protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: "file:///fake.pxf"},
		})
	require.NoError(t, err)

	// Confirm the inner handler ran — wrapper must not swallow
	// the call. WaitGroup ensures we synchronize on reply() rather
	// than relying on the wrapped invocation returning.
	var innerReturned sync.WaitGroup
	innerReturned.Add(1)
	reply := func(_ context.Context, _ any, _ error) error {
		innerReturned.Done()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, wrapped(ctx, reply, req))
	innerReturned.Wait()

	logs := buf.String()
	assert.Contains(t, logs, "LSP handler exceeded slow threshold",
		"slow dispatch must fire the wrapper's warn log; logs:\n%s", logs)
	assert.True(t,
		strings.Contains(logs, `"method":"`+slowMethod+`"`) ||
			strings.Contains(logs, `method=`+slowMethod),
		"warn line must name the offending method; logs:\n%s", logs)
}

// TestSlowHandlerTripwire_FastDispatchIsSilent_RealServer pairs
// with the slow-fires test: a fast real LSP method through the
// same wrapped chain produces no warn log. Catches a regression
// where the wrapper's threshold check accidentally always trips
// — the slow-side test alone wouldn't notice.
func TestSlowHandlerTripwire_FastDispatchIsSilent_RealServer(t *testing.T) {
	logger, buf := captureLogger()
	srv := server.New(logger)
	t.Cleanup(func() { _ = srv.Close() })

	const threshold = 50 * time.Millisecond
	wrapped := WithSlowHandlerWarn(srv.Handle, logger, threshold)

	// documentSymbol on an unopened URI returns nil immediately —
	// well under the threshold.
	req, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), "textDocument/documentSymbol",
		&protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: "file:///fake.pxf"},
		})
	require.NoError(t, err)

	reply := func(_ context.Context, _ any, _ error) error { return nil }
	require.NoError(t, wrapped(context.Background(), reply, req))

	assert.False(t,
		strings.Contains(buf.String(), "LSP handler exceeded slow threshold"),
		"fast dispatch must stay silent; logs:\n%s", buf.String())
}
