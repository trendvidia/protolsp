// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package transport

import (
	"context"
	"errors"
	"github.com/trendvidia/protolsp/internal/lspcodec"
	"io"
	"log/slog"
	"os"
	"time"

	"go.lsp.dev/jsonrpc2"

	"github.com/trendvidia/protolsp/internal/server"
)

// ServeStdio runs the LSP loop over stdin/stdout. Blocks until the
// client closes stdin or the server is shut down via the LSP exit
// notification. Returns nil for a clean shutdown.
func ServeStdio(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	return serve(ctx, log, newStdioRWC())
}

// stdioReadWriteCloser glues os.Stdin and os.Stdout into a single
// io.ReadWriteCloser. Reads route through a frame-bounded
// validator (MaxStdioFrameBytes) so a malicious client can't
// drive an OOM via a giant Content-Length header. Close closes
// os.Stdin to unblock any in-flight read — without this, a
// SIGTERM from the parent process leaves the reader stuck in a
// blocking syscall and the process never exits (the LSP-bridge
// "Stopping the server timed out" symptom).
type stdioReadWriteCloser struct {
	r io.Reader
}

func newStdioRWC() stdioReadWriteCloser {
	return stdioReadWriteCloser{
		r: newLimitedFrameReader(os.Stdin, MaxStdioFrameBytes),
	}
}

func (s stdioReadWriteCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (stdioReadWriteCloser) Write(p []byte) (int, error)  { return os.Stdout.Write(p) }
func (stdioReadWriteCloser) Close() error                 { return os.Stdin.Close() }

func serve(ctx context.Context, log *slog.Logger, rwc io.ReadWriteCloser) error {
	srv := server.New(log)
	// Server.Close stops the schema-resolver refresh goroutines (if any
	// were opened in dispatchInitialize). Safe even when no resolver
	// was configured.
	defer func() {
		if err := srv.Close(); err != nil {
			log.Warn("protolsp server close", "err", err)
		}
	}()

	stream := jsonrpc2.NewStream(rwc)
	conn := jsonrpc2.NewConn(stream, jsonrpc2.WithCodec(lspcodec.Codec{}))

	// The Server emits publishDiagnostics notifications back to the
	// client; AttachConn gives it the channel for those. Must happen
	// before conn.Go starts the dispatch loop or an incoming didOpen
	// could race ahead of the attach.
	srv.AttachConn(conn)

	// Wrap the handler with a tripwire so any future regression to
	// the dispatcher-deadlock bug class (handler blocking the single-goroutine
	// jsonrpc2 reader) shows up as a Warn log in the trace panel
	// the first time it triggers, instead of going silent until a
	// user reports broken behavior. Threshold default in
	// handler_warn.go.
	conn.Go(ctx, WithSlowHandlerWarn(srv.Handle, log, DefaultSlowHandlerThreshold))

	// Bridge ctx cancellation to process exit: when main's SIGTERM
	// handler cancels ctx, give srv.Close + rwc.Close a brief chance
	// to run, then force-exit. Without this, serve() blocks on
	// `<-conn.Done()` forever while the reader sits in
	// syscall.Read(os.Stdin) — closing os.Stdin from another
	// goroutine isn't guaranteed to unblock a blocking read on
	// Darwin, so the process would hang detached. That's exactly the
	// "ghost protolsp survives shutdown" symptom that produces a
	// second protolsp on every workspace open (welcome-screen LSP
	// gets SIGTERM, doesn't die, stays alive forever).
	ctxDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// Best-effort graceful cleanup first.
			_ = srv.Close()
			_ = rwc.Close()
			// Brief grace window for in-flight handlers (publishing
			// final diagnostics, etc.). 200ms is enough for anything
			// reasonable; longer hurts pxfed's workspace-open latency
			// since it waits on the old server to die before spawning
			// the new one.
			time.Sleep(200 * time.Millisecond)
			os.Exit(0)
		case <-ctxDone:
		}
	}()
	defer close(ctxDone)

	<-conn.Done()
	err := conn.Err()
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
