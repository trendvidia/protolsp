// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Connect-RPC interceptor stack. Today: an slog-based logger so
// the sidecar's traffic shows up in protolsp's stderr (which
// lsp-bridge forwards to pxfed's trace panel). The Connect-RPC
// channel runs over HTTP/2 on loopback, so without this it's
// invisible to the trace UI that only sees stdio framing.
//
// History: the first cut used `connect.UnaryInterceptorFunc` and
// only saw unary calls. Subscribe streams (Secrets, Datasets,
// EffectiveConfig) bypassed it entirely, so ~⅓ of the wire
// traffic — the most diagnostically useful third — was missing
// from the trace panel. See issue #108. This implementation
// covers all three RPC categories by satisfying the full
// connect.Interceptor interface.

package server

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
)

// rpcLoggingInterceptor implements connect.Interceptor for every
// method category exposed by the Connect-RPC sidecar:
//
//   - WrapUnary  — request/response RPCs (Get*, etc.). One
//     "rpc start" + "rpc done" pair per call.
//   - WrapStreamingHandler — server-streaming Subscribe RPCs.
//     "rpc stream open" at handler entry, "rpc
//     stream close" with sent/received counts + total
//     duration at handler exit.
//   - WrapStreamingClient  — passthrough; protolsp doesn't act as
//     a Connect-RPC client today, included for
//     interface completeness so future client-side
//     use is logged through the same path.
type rpcLoggingInterceptor struct {
	log *slog.Logger
}

func (i *rpcLoggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		method := req.Spec().Procedure
		start := time.Now()
		i.log.Info("rpc start", "method", method)
		resp, err := next(ctx, req)
		fields := []any{
			"method", method,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if err != nil {
			fields = append(fields, "err", err.Error())
		}
		i.log.Info("rpc done", fields...)
		return resp, err
	}
}

func (i *rpcLoggingInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *rpcLoggingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		method := conn.Spec().Procedure
		start := time.Now()
		i.log.Info("rpc stream open", "method", method)
		counting := &countingStreamingHandlerConn{StreamingHandlerConn: conn}
		err := next(ctx, counting)
		fields := []any{
			"method", method,
			"duration_ms", time.Since(start).Milliseconds(),
			"sent", counting.sent.Load(),
			"received", counting.received.Load(),
		}
		if err != nil {
			fields = append(fields, "err", err.Error())
		}
		i.log.Info("rpc stream close", fields...)
		return err
	}
}

// countingStreamingHandlerConn wraps a connect.StreamingHandlerConn
// so the interceptor can report total Send / Receive counts at
// stream-close time without logging per-message (which would
// flood the trace panel on a chatty Subscribe).
//
// Atomic counters because Send / Receive may be invoked from
// goroutines distinct from the handler's main goroutine — the
// Subscribe handlers in particular fan messages from the doc-
// store's publish loop.
type countingStreamingHandlerConn struct {
	connect.StreamingHandlerConn
	sent     atomic.Int64
	received atomic.Int64
}

func (c *countingStreamingHandlerConn) Send(msg any) error {
	if err := c.StreamingHandlerConn.Send(msg); err != nil {
		return err
	}
	c.sent.Add(1)
	return nil
}

func (c *countingStreamingHandlerConn) Receive(msg any) error {
	if err := c.StreamingHandlerConn.Receive(msg); err != nil {
		return err
	}
	c.received.Add(1)
	return nil
}
