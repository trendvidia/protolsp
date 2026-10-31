// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package cli holds the entry points of the protolsp commands, so that
// the commands in this module and the ones in the nested govm module
// are the same program.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/trendvidia/protolsp/internal/transport"
)

// installGoroutineDumper wires SIGUSR1 to dump all goroutine stacks
// to stderr. Diagnostic for the intermittent "schema features stop
// responding" symptom — when the user notices the hang, they
// `kill -USR1 <pid>` and the stacks flow through lsp-bridge's
// stderr forwarder into pxfed's trace panel, so we can see exactly
// which goroutine is stuck on which lock / channel.
//
// SIGUSR1 specifically (not SIGQUIT / SIGABRT) because Go's runtime
// already handles those by exiting after the dump — we want the
// server to keep running so the user doesn't lose any in-flight
// work.
func installGoroutineDumper(logger *slog.Logger) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	go func() {
		for range sig {
			buf := make([]byte, 1<<20) // 1 MiB ceiling; truncates pathological cases
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr,
				"=== goroutine dump (SIGUSR1, %d goroutines, %d bytes) ===\n%s=== end goroutine dump ===\n",
				runtime.NumGoroutine(), n, buf[:n])
			logger.Info("dumped goroutine stacks on SIGUSR1",
				slog.Int("goroutines", runtime.NumGoroutine()))
		}
	}()
}

// Stdio runs the LSP server over stdio against a single client. It
// parses the command line and exits the process on failure.
func Stdio() {
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	flag.Parse()

	logger := newStderrLogger(*logLevel)
	installGoroutineDumper(logger)

	// LSP servers write all log output to stderr — stdout is reserved
	// for the JSON-RPC framing protocol; any stray byte to stdout
	// corrupts the client's parser.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := transport.ServeStdio(ctx, logger); err != nil {
		logger.Error("protolsp exited with error", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func newStderrLogger(level string) *slog.Logger {
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
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
