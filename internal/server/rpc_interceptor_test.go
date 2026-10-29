// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// syncBuffer is a goroutine-safe wrapper around *bytes.Buffer.
// The interceptor's stream-close log lines fire from the
// Connect-RPC handler goroutine while the test's poll loop reads
// concurrently — `bytes.Buffer` itself isn't safe for that mix,
// which the race detector caught.
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

// captureLogger returns an slog.Logger that writes JSON-encoded
// lines into the returned syncBuffer. Used to assert what the
// interceptor emitted without coupling to slog's text format
// (which uses key=value but escapes differently across builds).
func captureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, buf
}

// TestRPCInterceptor_LogsUnaryStartAndDone exercises the
// WrapUnary path: a single Get* call should produce one
// "rpc start" + one "rpc done" log line, each tagged with the
// procedure name.
func TestRPCInterceptor_LogsUnaryStartAndDone(t *testing.T) {
	logger, buf := captureLogger()
	srv := New(logger)
	t.Cleanup(func() { _ = srv.Close() })

	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	client := rpcpbconnect.NewRegistryServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.GetStatus(ctx, connect.NewRequest(&rpcpb.RegistryServiceGetStatusRequest{}))
	require.NoError(t, err)

	logs := buf.String()
	assert.Contains(t, logs, `"msg":"rpc start"`)
	assert.Contains(t, logs, `"msg":"rpc done"`)
	assert.Contains(t, logs, `"method":"/protolsp.v1.rpc.RegistryService/GetStatus"`)
	// Streaming-specific keys must NOT appear on a unary call.
	assert.NotContains(t, logs, `"msg":"rpc stream open"`)
	assert.NotContains(t, logs, `"msg":"rpc stream close"`)
}

// TestRPCInterceptor_LogsStreamingHandler covers streaming
// handlers: Subscribe RPCs must produce stream-open / stream-
// close log lines AND report a send count covering the snapshot
// message + any subsequent change events. The first Subscribe to
// a doc with one secret + a follow-up edit that removes the
// secret yields sent=2 (snapshot + change).
func TestRPCInterceptor_LogsStreamingHandler(t *testing.T) {
	logger, buf := captureLogger()
	srv := New(logger)
	srv.AttachConn(&fakeConn{})

	// Wire the credentials type so the secret-ranges machinery
	// has a schema to bind against.
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	t.Cleanup(func() { _ = srv.Close() })

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	client := rpcpbconnect.NewSecretsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.SecretsServiceSubscribeRequest{
		TextDocument: &rpcpb.TextDocumentIdentifier{Uri: "file:///work/c.pxf"},
	}))
	require.NoError(t, err)

	// Snapshot.
	require.True(t, stream.Receive())
	require.Len(t, stream.Msg().Ranges, 1)

	// Trigger one change.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/c.pxf", 2,
			"@type acme.demo.v1.Credentials\n\nusername = \"alice\"\n")))
	require.True(t, stream.Receive())

	// Cancelling the context drains the handler so the
	// "rpc stream close" line lands in the buffer before we
	// inspect it.
	cancel()
	require.NoError(t, stream.Close())

	// Allow the close log line to flush — the handler runs on a
	// goroutine separate from our caller; the slog write itself
	// is synchronous but we're racing the handler return.
	waitForLog(t, buf, `"msg":"rpc stream close"`, 2*time.Second)

	logs := buf.String()
	assert.Contains(t, logs, `"msg":"rpc stream open"`)
	assert.Contains(t, logs, `"msg":"rpc stream close"`)
	assert.Contains(t, logs, `"method":"/protolsp.v1.rpc.SecretsService/Subscribe"`)
	// At least the snapshot + the one change event must show up
	// in the sent count. JSON-encoded number renders as bare digits.
	assert.True(t, hasSentCountAtLeast(logs, 2), "sent count must include snapshot + change events; got logs: %s", logs)
	// Unary keys must NOT appear on a stream-only run.
	assert.NotContains(t, logs, `"msg":"rpc start"`)
	assert.NotContains(t, logs, `"msg":"rpc done"`)
}

// waitForLog polls buf for the given substring, returning when it
// appears or t.Fatal-ing past the deadline. The Subscribe close
// log is emitted from the handler goroutine after the client's
// stream.Close() returns, so a poll is more reliable than a
// fixed sleep.
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

// hasSentCountAtLeast looks for a "sent":N field where N >= want.
// Coarse string-scan rather than JSON-parsing each line — keeps
// the test independent of the line ordering and the rest of the
// emitted fields.
func hasSentCountAtLeast(logs string, want int) bool {
	for n := want; n <= want+10; n++ {
		needle := `"sent":` + itoa10(n)
		if strings.Contains(logs, needle) {
			return true
		}
	}
	return false
}

func itoa10(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
