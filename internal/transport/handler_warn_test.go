// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package transport

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/trendvidia/protolsp/internal/lspcodec"

	"github.com/stretchr/testify/assert"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

func TestWithSlowHandlerWarn_FiresOnSlowDispatch(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	slow := jsonrpc2.Handler(func(ctx context.Context, req *jsonrpc2.Request) (any, error) {
		time.Sleep(30 * time.Millisecond)
		return nil, nil
	})
	wrapped := WithSlowHandlerWarn(slow, log, 10*time.Millisecond)

	dispatchOnce(t, wrapped, "textDocument/didOpen")

	out := buf.String()
	assert.Contains(t, out, "LSP handler exceeded slow threshold",
		"wrapper logs at Warn when handler exceeds threshold")
	assert.Contains(t, out, `method=textDocument/didOpen`,
		"warn line names the offending method")
}

func TestWithSlowHandlerWarn_QuietOnFastDispatch(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fast := jsonrpc2.Handler(func(ctx context.Context, req *jsonrpc2.Request) (any, error) {
		return nil, nil
	})
	wrapped := WithSlowHandlerWarn(fast, log, 50*time.Millisecond)

	dispatchOnce(t, wrapped, "textDocument/didOpen")

	out := buf.String()
	assert.False(t, strings.Contains(out, "exceeded slow threshold"),
		"wrapper stays silent under threshold")
}

func TestWithSlowHandlerWarn_ZeroThresholdReturnsBareHandler(t *testing.T) {
	// Disable knob: threshold <= 0 bypasses the wrap entirely. Lets
	// tests deliberately exercise slow paths without log noise.
	called := 0
	inner := jsonrpc2.Handler(func(ctx context.Context, req *jsonrpc2.Request) (any, error) {
		called++
		return nil, nil
	})
	bare := WithSlowHandlerWarn(inner, slog.Default(), 0)
	// Reference equality would require comparing function values which
	// Go doesn't allow; instead, exercise it and check the inner ran.
	dispatchOnce(t, bare, "noop")
	assert.Equal(t, 1, called, "zero threshold still invokes the inner handler")
}

// dispatchOnce runs one call through h on a real connection, since only
// a connection can build a *jsonrpc2.Request. It returns after h does.
func dispatchOnce(t *testing.T, h jsonrpc2.Handler, method string) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	server := jsonrpc2.NewConn(jsonrpc2.NewStream(serverSide), jsonrpc2.WithCodec(lspcodec.Codec{}))
	client := jsonrpc2.NewConn(jsonrpc2.NewStream(clientSide), jsonrpc2.WithCodec(lspcodec.Codec{}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server.Go(ctx, h)
	client.Go(ctx, jsonrpc2.MethodNotFoundHandler)
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	params := &protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///fake.pxf"},
	}
	if _, err := client.Call(ctx, method, params, nil); err != nil {
		t.Fatalf("call %s: %v", method, err)
	}
}
