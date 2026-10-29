// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Command protolsp-ws serves protolsp over WebSocket, the transport
// used by the hosted Monaco IDE. For desktop editors that prefer
// stdio (VS Code's vscode-languageclient/node, Neovim, Zed), use the
// protolsp binary instead.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/trendvidia/protolsp/internal/transport"
)

func main() {
	var (
		addr           = flag.String("addr", ":8080", "HTTP bind address")
		path           = flag.String("path", "/lsp", "HTTP path that accepts WebSocket upgrades")
		logLevel       = flag.String("log-level", "info", "log level: debug|info|warn|error")
		origins        = flag.String("origins", "", "comma-separated allowed Origin patterns (empty = same-origin only)")
		insecureOrigin = flag.Bool("insecure-skip-origin-verify", false,
			"disable Origin header verification; only safe behind a trusted proxy")
		maxMessageBytes = flag.Int64("max-message-bytes", 4<<20, "max inbound WS frame size in bytes")
	)
	flag.Parse()

	logger := newLogger(*logLevel)

	if *insecureOrigin {
		logger.Warn("WebSocket origin verification disabled — only safe behind a trusted proxy")
	}

	opts := &transport.AcceptOptions{
		OriginPatterns:           parseOrigins(*origins),
		InsecureSkipOriginVerify: *insecureOrigin,
		MaxMessageBytes:          *maxMessageBytes,
	}

	mux := http.NewServeMux()
	mux.Handle(*path, transport.WebsocketHandler(logger, opts))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Graceful shutdown: on SIGINT/SIGTERM, stop accepting new
	// connections and give in-flight LSP sessions a few seconds to
	// finish their final notifications.
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("protolsp-ws listening", slog.String("addr", *addr), slog.String("path", *path))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("protolsp-ws failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func parseOrigins(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	// Unlike the stdio variant, protolsp-ws is free to use stdout for
	// logs — JSON-RPC framing rides over WebSocket, not stdio.
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
