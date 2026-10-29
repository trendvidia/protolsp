// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"go.lsp.dev/jsonrpc2"

	"github.com/trendvidia/protolsp/internal/server"
)

// WebsocketHandler returns an http.Handler that accepts LSP-over-WebSocket
// connections. Each accepted connection spawns its own server.Server and
// runs the jsonrpc2 dispatch loop until the client disconnects or the
// server signals exit. Use this for the hosted Monaco IDE — desktop
// editors prefer stdio (see ServeStdio).
func WebsocketHandler(log *slog.Logger, opts *AcceptOptions) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	if opts == nil {
		opts = &AcceptOptions{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptOpts := &websocket.AcceptOptions{
			InsecureSkipVerify: opts.InsecureSkipOriginVerify,
			OriginPatterns:     opts.OriginPatterns,
			Subprotocols:       []string{"lsp"},
		}
		c, err := websocket.Accept(w, r, acceptOpts)
		if err != nil {
			log.Warn("ws accept failed", slog.String("err", err.Error()))
			return
		}
		// Generous read limit: LSP didOpen of a large document can be
		// hundreds of KB. 4MiB is comfortable for a single message;
		// adjust via AcceptOptions if needed.
		c.SetReadLimit(opts.maxMessageBytes())

		ctx := r.Context()
		if err := serveOverWS(ctx, log, c); err != nil {
			log.Warn("ws session ended with error", slog.String("err", err.Error()))
		}
	})
}

// AcceptOptions tunes the WebSocket handler. The zero value is safe
// for development; production deployments should set
// OriginPatterns to the IDE's expected origin to defeat
// cross-origin upgrade attacks.
type AcceptOptions struct {
	// OriginPatterns are the allowed Origin header values for incoming
	// WS upgrade requests. Wildcards are supported per coder/websocket
	// rules. Empty means same-origin only.
	OriginPatterns []string
	// InsecureSkipOriginVerify disables origin checking entirely.
	// Only safe behind a trusted reverse proxy that enforces auth /
	// network isolation. Logs a startup warning when set.
	InsecureSkipOriginVerify bool
	// MaxMessageBytes is the maximum size of a single inbound LSP
	// frame (one didOpen payload). Defaults to 4 MiB.
	MaxMessageBytes int64
}

func (o *AcceptOptions) maxMessageBytes() int64 {
	if o.MaxMessageBytes > 0 {
		return o.MaxMessageBytes
	}
	return 4 << 20
}

// serveOverWS wires a single WebSocket connection into jsonrpc2 and
// runs the LSP dispatch loop. Each WS message is one JSON-RPC frame
// (LSP-over-WebSocket convention; no Content-Length framing). Returns
// nil for a clean close, an error for unexpected disconnects.
func serveOverWS(ctx context.Context, log *slog.Logger, c *websocket.Conn) error {
	srv := server.New(log)
	defer func() {
		if err := srv.Close(); err != nil {
			log.Warn("protolsp server close (ws)", "err", err)
		}
	}()

	stream := &wsStream{conn: c}
	jc := jsonrpc2.NewConn(stream)
	srv.AttachConn(jc)
	// Same slow-handler tripwire as stdio (see handler_warn.go). The
	// hosted IDE has the same single-goroutine-dispatcher contract.
	jc.Go(ctx, WithSlowHandlerWarn(srv.Handle, log, DefaultSlowHandlerThreshold))

	<-jc.Done()
	jcErr := jc.Err()

	// Best-effort close handshake; ignore errors after the server has
	// already terminated the connection.
	_ = c.Close(websocket.StatusNormalClosure, "session ended")

	if jcErr == nil || errors.Is(jcErr, io.EOF) {
		return nil
	}
	// coder/websocket returns CloseError for normal close codes; treat
	// any "going away" or "normal closure" as a graceful end too.
	var ce websocket.CloseError
	if errors.As(jcErr, &ce) {
		switch ce.Code {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return nil
		}
	}
	return jcErr
}

// wsStream adapts a coder/websocket.Conn to jsonrpc2.Stream — one WS
// message per JSON-RPC frame, no Content-Length framing. This matches
// the LSP-over-WebSocket convention used by Monaco's
// vscode-languageclient/browser transport.
type wsStream struct {
	conn *websocket.Conn
}

func (s *wsStream) Read(ctx context.Context) (jsonrpc2.Message, int64, error) {
	typ, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, 0, err
	}
	if typ != websocket.MessageText {
		return nil, int64(len(data)), fmt.Errorf("ws: expected text message, got %v", typ)
	}
	msg, err := jsonrpc2.DecodeMessage(data)
	if err != nil {
		return nil, int64(len(data)), fmt.Errorf("ws: decoding jsonrpc2 message: %w", err)
	}
	return msg, int64(len(data)), nil
}

func (s *wsStream) Write(ctx context.Context, msg jsonrpc2.Message) (int64, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return 0, fmt.Errorf("ws: encoding jsonrpc2 message: %w", err)
	}
	if err := s.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (s *wsStream) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}
