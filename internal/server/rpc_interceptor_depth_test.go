// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Deeper coverage for the RPC interceptor: error preservation
// across the interceptor boundary AND context-cancellation
// propagation through streaming handlers. The existing
// rpc_interceptor_test.go covers the happy paths (unary
// start/done, streaming open/close + send counts); these tests
// pin the failure paths.

package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// TestRPCInterceptor_PreservesConnectCodeOnError exercises the
// path where a handler returns connect.NewError(code, err). The
// interceptor wraps the call but must:
//
//  1. Log the "rpc done" line with the error message.
//  2. Return the original error unchanged so connect.CodeOf on
//     the client side still surfaces the typed code (not a
//     generic Unknown).
//
// A regression where the interceptor swallowed the error or
// reduced it to a string would silently break every editor-side
// code path that branches on connect.CodeOf.
func TestRPCInterceptor_PreservesConnectCodeOnError(t *testing.T) {
	logger, buf := captureLogger()
	srv := New(logger)
	srv.AttachConn(&fakeConn{})
	t.Cleanup(func() { _ = srv.Close() })

	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	client := rpcpbconnect.NewConfigServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	// GetAttribute with a name not in the registry returns
	// CodeInvalidArgument. The interceptor must preserve that code
	// AND log the error message.
	_, err = client.GetAttribute(context.Background(),
		connect.NewRequest(&rpcpb.GetAttributeRequest{Name: "no.such.attr"}))
	require.Error(t, err, "unknown attribute must surface as an error")
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err),
		"interceptor must preserve the typed connect.Code through to the client")

	// The interceptor's "rpc done" line must carry the error text
	// (proves it routed through the err-branch of WrapUnary instead
	// of silently logging success).
	waitForLog(t, buf, `"msg":"rpc done"`, 2*time.Second)
	logs := buf.String()
	assert.Contains(t, logs, `"err":"`,
		"interceptor must log the err field on a failed RPC; logs were:\n"+logs)
	assert.Contains(t, logs, "no.such.attr",
		"the error message must contain the offending attribute name")
}

// TestRPCInterceptor_PropagatesContextCancellation drives the
// streaming-handler path under a context that the client cancels
// mid-stream. The interceptor must:
//
//  1. NOT swallow the cancellation; the inner handler returns
//     normally (context.Canceled is the documented exit signal).
//  2. Log the "rpc stream close" line with sent/received counts
//     accumulated up to the cancel point — the counters are atomic
//     and must reflect what actually flowed before the cancel.
//
// A regression here would manifest as a Subscribe stream that
// silently leaks goroutines on client disconnect.
func TestRPCInterceptor_PropagatesContextCancellation(t *testing.T) {
	logger, buf := captureLogger()
	srv := New(logger)
	srv.AttachConn(&fakeConn{})

	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	t.Cleanup(func() { _ = srv.Close() })

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	client := rpcpbconnect.NewSecretsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Subscribe(ctx,
		connect.NewRequest(&rpcpb.SecretsServiceSubscribeRequest{
			TextDocument: &rpcpb.TextDocumentIdentifier{Uri: "file:///work/c.pxf"},
		}))
	require.NoError(t, err)

	// Receive the snapshot — proves the stream is established and
	// the interceptor's "rpc stream open" line has fired.
	require.True(t, stream.Receive(), "snapshot must arrive")
	require.Len(t, stream.Msg().Ranges, 1)

	// Cancel mid-stream. The interceptor's WrapStreamingHandler
	// wraps the handler in a `next(ctx, ...)` that exits when
	// ctx.Done() fires — the interceptor must let that propagate
	// AND log the close with counters intact.
	cancel()
	_ = stream.Close()

	waitForLog(t, buf, `"msg":"rpc stream close"`, 2*time.Second)
	logs := buf.String()
	assert.Contains(t, logs, `"msg":"rpc stream open"`,
		"open line must fire even when the stream is short-lived")
	assert.Contains(t, logs, `"msg":"rpc stream close"`,
		"close line must fire on context cancellation, not just on graceful close")
	assert.True(t, hasSentCountAtLeast(logs, 1),
		"sent count must include the snapshot delivered before cancel; logs:\n"+logs)
}
