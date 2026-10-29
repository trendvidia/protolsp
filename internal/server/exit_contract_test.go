// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Contract tests for the MethodExit handler. The existing
// TestHandle_ExitClosesConnectionAndReturnsNil covers the happy
// path without an attached conn; this file pins the with-conn
// behaviors the server.go:591 comment block calls out:
//
//   - Handle returns nil from MethodExit (not ErrServerClosed)
//     so jsonrpc2's atomic.Value.Store fail-path doesn't race a
//     subsequent *websocket.CloseError with mismatched type.
//   - conn.Close runs asynchronously (Handle must NOT block on
//     it) so the dispatcher's single reader is freed immediately.
//   - The Close DOES eventually fire — without it the conn's
//     reader never sees EOF and the process can hang on Darwin.

package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// closeRecordingConn wraps fakeConn with an atomic Close counter
// + a slow Close path the test can use to confirm the handler
// doesn't block on Close completion. Inherits the rest of the
// jsonrpc2.Conn surface from fakeConn.
type closeRecordingConn struct {
	fakeConn
	closeCount atomic.Int32
	closeDelay time.Duration
}

func (c *closeRecordingConn) Close() error {
	if c.closeDelay > 0 {
		time.Sleep(c.closeDelay)
	}
	c.closeCount.Add(1)
	return nil
}

// TestExit_HandleReturnsImmediatelyAndCloseRunsAsync confirms the
// async-Close design. We attach a conn whose Close sleeps for a
// substantial duration; Handle must still return promptly.
// Without the goroutine, Handle would block for the full delay.
func TestExit_HandleReturnsImmediatelyAndCloseRunsAsync(t *testing.T) {
	srv := New(discardLogger())
	rec := &closeRecordingConn{closeDelay: 250 * time.Millisecond}
	srv.AttachConn(rec)

	notif, err := jsonrpc2.NewNotification(protocol.MethodExit, nil)
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, srv.Handle(context.Background(), noopReply, notif))
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 50*time.Millisecond,
		"exit handler must NOT block on conn.Close — Close runs in a goroutine")

	// Close eventually fires. Poll rather than sleep blindly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.closeCount.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, int32(1), rec.closeCount.Load(),
		"conn.Close must run exactly once for a single MethodExit dispatch")
}

// TestExit_NilReplyForNotification pins that exit is treated
// strictly as a notification — no reply path runs, the supplied
// replier never gets invoked. A regression that routed exit
// through the call-with-reply branch would surface as a stray
// reply on the wire (or worse, a duplicate response that breaks
// the next request's id matching).
func TestExit_NilReplyForNotification(t *testing.T) {
	srv := New(discardLogger())
	srv.AttachConn(&fakeConn{})
	rep := &captureReplier{}

	notif, err := jsonrpc2.NewNotification(protocol.MethodExit, nil)
	require.NoError(t, err)

	require.NoError(t, srv.Handle(context.Background(), rep.reply, notif))
	assert.False(t, rep.called,
		"exit is a notification — reply must NOT be invoked")
}

// TestExit_TwoBackToBackExitsClosesTwice is the regression guard
// for a future change that tries to deduplicate exit handling
// (e.g., a flag that skips Close after the first call). The
// jsonrpc2 dispatch loop should never deliver two exits in
// practice, but if it does the handler must remain panic-free.
// closeRecordingConn.Close is safe to call repeatedly; we just
// confirm we don't panic and Handle keeps returning nil.
func TestExit_TwoBackToBackExitsClosesTwice(t *testing.T) {
	srv := New(discardLogger())
	rec := &closeRecordingConn{}
	srv.AttachConn(rec)

	notif, err := jsonrpc2.NewNotification(protocol.MethodExit, nil)
	require.NoError(t, err)

	require.NoError(t, srv.Handle(context.Background(), noopReply, notif))
	require.NoError(t, srv.Handle(context.Background(), noopReply, notif))

	// Both Close calls eventually run. The contract isn't "exactly
	// once for the lifetime of the server" — it's "no panic + nil
	// return for every exit". A real client only sends exit once,
	// so this test pins worst-case resilience.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.closeCount.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, rec.closeCount.Load(), int32(2),
		"two MethodExit dispatches → two Close calls (no dedup, no panic)")
}
