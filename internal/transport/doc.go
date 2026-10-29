// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package transport hosts the LSP-over-{stdio,websocket} loops.
// Each transport wires an io.ReadWriteCloser pair into jsonrpc2 and
// runs until the underlying connection closes.
//
// Files:
//
//   - stdio.go — ServeStdio (production for cmd/protolsp). Caps the
//     incoming Content-Length header (stdio_limit.go) so a hostile
//     client can't OOM the server with a giant frame.
//   - websocket.go — ServeWebSocket (cmd/protolsp-ws, the hosted-
//     Monaco variant). Frame caps live alongside.
//   - handler_warn.go — WithSlowHandlerWarn, a wrapper that flags
//     handler regressions to the trace panel when a single
//     dispatch holds the jsonrpc2 reader goroutine past the
//     DefaultSlowHandlerThreshold (added after the #128 deadlock
//     so any future variant surfaces as a Warn instead of a
//     silent freeze).
//
// The exit-notification path inside server.Handle closes the
// connection directly (jsonrpc2's fail()-based path doesn't compose
// with WebSocket close errors), so the transports just watch for
// EOF + normal-close codes and shut down cleanly.
package transport
