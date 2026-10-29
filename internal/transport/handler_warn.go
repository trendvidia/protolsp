// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Early-warning instrumentation for the LSP dispatcher.
//
// go.lsp.dev/jsonrpc2 dispatches every incoming message on a single
// reader goroutine (conn.go: `go c.run(ctx, handler)`). When a
// handler blocks — say, by calling s.conn.Call inline and waiting on
// a response only the same reader goroutine can route — every
// subsequent client message queues in the OS pipe unprocessed. The
// failure mode is silent: the client sees `→` traces fly out, the
// server's stderr goes quiet, and the only reliable symptom is "the
// feature stopped working" reported by a user some time later.
//
// PR #128 fixed the one currently-known instance of this (the
// dispatchDidChangeConfiguration inline conn.Call). PR #129 added a
// real-transport smoke test so the exact regression can't return.
// This file is the complementary live-system tripwire: any future
// handler that takes too long to return raises a Warn log naming
// the method + the duration, so the same class of bug is loud on
// any developer's machine the first time it triggers, not silent
// until it reaches a user.

package transport

import (
	"context"
	"log/slog"
	"time"

	"go.lsp.dev/jsonrpc2"
)

// DefaultSlowHandlerThreshold is the duration above which a single
// handler dispatch raises a Warn log. 5s is generous: production
// handlers (analyze + publish over a moderately-sized workspace)
// have settled in the low hundreds of milliseconds in observed
// traces, and even the initial schema-lookup dial is sub-second on
// localhost / sub-second-with-error on a misconfigured registry.
// Anything past 5s is almost certainly a bug — either a deadlock
// or an unintentionally slow path that's worth investigating.
const DefaultSlowHandlerThreshold = 5 * time.Second

// WithSlowHandlerWarn wraps h so any single dispatch taking longer
// than threshold emits a Warn log naming the method + duration.
// Returns h unchanged when threshold <= 0 (disable for tests that
// deliberately exercise slow paths).
//
// The wrap measures HANDLER RETURN TIME, not downstream goroutine
// completion. That's deliberate: the failure mode we're catching is
// "handler synchronously blocked the reader." Handlers that
// correctly spawn goroutines and return reply() immediately (the
// initialized + didChangeConfiguration pattern) don't trip this even
// when their goroutines run for minutes — those goroutines aren't
// blocking the dispatcher.
//
// Safe to call once at transport setup time; the returned handler
// is a thin wrapper that adds one time.Now() + one time.Since() per
// dispatch. Cost is negligible compared to LSP message framing
// itself.
func WithSlowHandlerWarn(h jsonrpc2.Handler, log *slog.Logger, threshold time.Duration) jsonrpc2.Handler {
	if threshold <= 0 || log == nil {
		return h
	}
	return func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		start := time.Now()
		err := h(ctx, reply, req)
		if d := time.Since(start); d > threshold {
			log.Warn("LSP handler exceeded slow threshold — handler likely blocked the jsonrpc2 reader; see internal/transport/handler_warn.go",
				slog.String("method", req.Method()),
				slog.Duration("duration", d),
				slog.Duration("threshold", threshold),
			)
		}
		return err
	}
}
