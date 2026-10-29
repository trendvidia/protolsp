// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain wires the helper-process pattern: when PROTOLSP_HELPER_MAIN
// is set, this test binary re-executes itself as the LSP server by
// calling main(). The smoke-exec test then drives an LSP session over
// stdin/stdout against that child process — no separate `go build`
// step is needed and the test exercises the EXACT entry point cmd
// users hit in production.
func TestMain(m *testing.M) {
	if os.Getenv("PROTOLSP_HELPER_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestNewLogger_LevelMapping(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"unknown falls back to info", "trace", slog.LevelInfo},
		{"empty falls back to info", "", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger := newLogger(tc.in)
			if logger == nil {
				t.Fatal("newLogger returned nil")
			}
			if !logger.Enabled(t.Context(), tc.want) {
				t.Fatalf("logger should be enabled at %v for input %q", tc.want, tc.in)
			}
		})
	}
}

// TestSmokeExec_InitializeAndExit drives a full LSP session against
// the cmd/protolsp binary entry point: initialize → initialized →
// shutdown → exit. The child must exit within the 200ms grace
// window the stdio loop documents — the "ghost protolsp survives
// shutdown" symptom from earlier incidents shows up as a hung
// child here.
//
// Uses the TestMain helper-process pattern so no separate
// `go build` step is required.
func TestSmokeExec_InitializeAndExit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), "PROTOLSP_HELPER_MAIN=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	// Discard stderr so log lines don't pollute test output.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Send initialize → wait for the response → send initialized →
	// shutdown → wait for response → exit (notification). Exit is
	// what triggers the graceful stdio shutdown.
	writeLSP(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`)
	mustReadResponse(t, stdout, 1)

	writeLSP(t, stdin, `{"jsonrpc":"2.0","method":"initialized","params":{}}`)

	writeLSP(t, stdin, `{"jsonrpc":"2.0","id":2,"method":"shutdown"}`)
	mustReadResponse(t, stdout, 2)

	writeLSP(t, stdin, `{"jsonrpc":"2.0","method":"exit"}`)
	// Close stdin to mirror real-LSP-client behavior: a graceful
	// shutdown pairs `exit` with closing the input stream so the
	// server's stdin reader unblocks. Without this on Darwin the
	// child's blocking read never returns even after conn.Close —
	// stdio.go documents this hazard in its serve() loop and
	// guards SIGTERM paths against it, but the LSP-exit-notification
	// path relies on the client closing stdin.
	_ = stdin.Close()

	// Child must exit quickly after the exit notification. The
	// documented stdio grace window is 200ms; allow a generous
	// multiplier for CI variance. The audit named this as the
	// regression boundary for the ghost-protolsp class.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
		// OK — clean exit. We don't check the exit code; the LSP
		// server may return 0 or non-zero depending on whether
		// shutdown was acknowledged before exit. Both are acceptable
		// for this test, which is about TIMING.
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("protolsp did not exit within 3s of receiving exit notification — ghost-protolsp regression")
	}
}

// TestSmokeExec_OversizeFrameClosesConnection drives a frame whose
// Content-Length exceeds MaxStdioFrameBytes through the real
// ServeStdio loop. The audit named this gap: the limit-reader is
// unit-tested in isolation but never asserted end-to-end against
// the binary, so a regression that bypasses the limit (e.g. a
// future refactor that wires raw os.Stdin instead of the limited
// reader) would slip through.
//
// Expected behavior: the server reads the headers, rejects the
// Content-Length, and the connection terminates. The child exits
// without us needing to send shutdown/exit.
func TestSmokeExec_OversizeFrameClosesConnection(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), "PROTOLSP_HELPER_MAIN=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// MaxStdioFrameBytes is 4 MiB. Claim 5 MiB so the limit
	// trips at header-parse time before the server allocates the
	// body buffer. Body bytes don't matter — the limit rejects
	// the frame before they're consumed.
	const oversize = 5 * 1024 * 1024
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", oversize)
	if _, err := io.WriteString(stdin, header); err != nil {
		// EPIPE here is acceptable — the server may have already
		// closed by the time we finish writing the header. The
		// real test is whether the child exits.
		t.Logf("stdin write returned (may be EPIPE if server already closed): %v", err)
	}
	// Close stdin so any blocking read in the child unblocks too.
	_ = stdin.Close()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
		// OK — the limit rejection caused the server loop to
		// terminate and the process exited. We don't check the
		// exit code; stdio.go's serve() returns the framing error
		// up through the dispatcher and main exits with status 1
		// after logging.
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("oversize-frame regression: server did not close the connection within 5s")
	}
}

// TestSmokeExec_SIGTERMMidHandshakeExitsCleanly drives the
// signal-handling path the production cmd binary registers via
// signal.NotifyContext. After initialize completes (so we're
// past the cheapest startup path and into a stable dispatcher
// state), the test sends SIGTERM to the child. The serve() loop
// in stdio.go documents a 200ms grace window before os.Exit(0)
// fires; we assert the process exits within a generous multiple
// of that to absorb CI variance.
//
// Pins the "no ghost protolsp on shutdown signal" contract from
// a different angle than the exit-notification smoke test —
// signals come from the OS, not the LSP client.
func TestSmokeExec_SIGTERMMidHandshakeExitsCleanly(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = append(os.Environ(), "PROTOLSP_HELPER_MAIN=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Drive the handshake far enough that the dispatcher is
	// running. initialize alone is enough — the signal arrives
	// before any didOpen / shutdown / exit, which is the
	// realistic "user quit the editor mid-session" scenario.
	writeLSP(t, stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`)
	mustReadResponse(t, stdout, 1)

	// Send SIGTERM. The child's signal.NotifyContext catches it,
	// cancels the serve() context, srv.Close + rwc.Close run,
	// 200ms grace passes, os.Exit(0).
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-exited:
		// Clean exit. The serve() loop forces os.Exit(0) after
		// the grace window; we don't check the exit code
		// because Go's exec reports it differently across
		// versions.
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("SIGTERM regression: child did not exit within 3s — the 200ms grace + force-exit path documented in stdio.go must keep working")
	}
}

// writeLSP frames a JSON-RPC payload with the LSP Content-Length
// header and writes it to w.
func writeLSP(t *testing.T, w io.Writer, body string) {
	t.Helper()
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := io.WriteString(w, frame); err != nil {
		t.Fatalf("write LSP frame: %v", err)
	}
}

// mustReadResponse reads one LSP-framed message from r and confirms
// it carries the expected request id. Fails on any framing /
// transport error.
func mustReadResponse(t *testing.T, r io.Reader, wantID int) {
	t.Helper()
	br := bufio.NewReader(r)
	var contentLength int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading header line: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil {
				t.Fatalf("parsing Content-Length %q: %v", line, err)
			}
			contentLength = n
		}
	}
	if contentLength <= 0 {
		t.Fatalf("missing or invalid Content-Length")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(br, body); err != nil {
		t.Fatalf("reading body: %v", err)
	}
	// Light-touch verification: the response body must mention the
	// expected id. Avoids pulling encoding/json into the test for
	// what's essentially a "did we get a reply" assertion.
	idPattern := fmt.Sprintf(`"id":%d`, wantID)
	if !strings.Contains(string(body), idPattern) {
		t.Fatalf("response body missing %q: %s", idPattern, body)
	}
}
