// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.lsp.dev/jsonrpc2"
)

func TestWithSlowHandlerWarn_FiresOnSlowDispatch(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	slow := jsonrpc2.Handler(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	})
	wrapped := WithSlowHandlerWarn(slow, log, 10*time.Millisecond)

	req, err := jsonrpc2.NewNotification("textDocument/didOpen", nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	noopReply := func(ctx context.Context, result any, err error) error { return nil }
	_ = wrapped(context.Background(), noopReply, req)

	out := buf.String()
	assert.Contains(t, out, "LSP handler exceeded slow threshold",
		"wrapper logs at Warn when handler exceeds threshold")
	assert.Contains(t, out, `method=textDocument/didOpen`,
		"warn line names the offending method")
}

func TestWithSlowHandlerWarn_QuietOnFastDispatch(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fast := jsonrpc2.Handler(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		return nil
	})
	wrapped := WithSlowHandlerWarn(fast, log, 50*time.Millisecond)

	req, err := jsonrpc2.NewNotification("textDocument/didOpen", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	noopReply := func(ctx context.Context, result any, err error) error { return nil }
	_ = wrapped(context.Background(), noopReply, req)

	out := buf.String()
	assert.False(t, strings.Contains(out, "exceeded slow threshold"),
		"wrapper stays silent under threshold")
}

func TestWithSlowHandlerWarn_ZeroThresholdReturnsBareHandler(t *testing.T) {
	// Disable knob: threshold <= 0 bypasses the wrap entirely. Lets
	// tests deliberately exercise slow paths without log noise.
	called := 0
	inner := jsonrpc2.Handler(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		called++
		return nil
	})
	bare := WithSlowHandlerWarn(inner, slog.Default(), 0)
	// Reference equality would require comparing function values which
	// Go doesn't allow; instead, exercise it and check the inner ran.
	req, _ := jsonrpc2.NewNotification("noop", nil)
	noopReply := func(ctx context.Context, result any, err error) error { return nil }
	_ = bare(context.Background(), noopReply, req)
	assert.Equal(t, 1, called, "zero threshold still invokes the inner handler")
}
