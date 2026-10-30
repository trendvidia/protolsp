// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Real-transport smoke test for the LSP dispatcher.
//
// Unlike server_test.go's fakeConn (which short-circuits jsonrpc2's
// stream reader), this test wires the actual Server through a real
// jsonrpc2.NewStream over io.Pipe pairs. That fidelity matters
// because the bug class this file exists to guard against — handlers
// blocking on s.conn.Call — is invisible to a fakeConn (calls return
// inline) but freezes a real dispatcher (the single reader goroutine
// is busy waiting for a response it would itself have to route).
//
// Cost: ~150 LOC. Value: catches every future "I added a feature
// that calls s.conn.Call inline" mistake the day it lands.

package transport

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/trendvidia/protolsp/internal/lspcodec"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/internal/server"
)

// TestDispatcher_NoDeadlockOnDidChangeConfiguration is the regression
// guard for the locked-bw → paste-session → reopen-prod.sops.pxf
// deadlock (see internal/server/configuration.go's
// dispatchDidChangeConfiguration comment block).
//
// Sequence:
//  1. Real jsonrpc2 over io.Pipe with the actual srv.Handle on the
//     server side and a small fake client that responds to
//     workspace/configuration + client/registerCapability.
//  2. initialize → initialized handshake; the client services the
//     initialized follow-up's workspace/configuration pull (this
//     alone proves the dispatcher routes responses correctly while
//     a handler-spawned goroutine is in flight).
//  3. workspace/didChangeConfiguration — the historically-blocking
//     notification. The client services its workspace/configuration
//     pull.
//  4. textDocument/didOpen with a minimal pxf source. publishDiagnostics
//     for the URI is expected back within a generous timeout.
//
// The deadlock would manifest as step 4's publishDiagnostics never
// arriving — the dispatcher freezes inside dispatchDidChangeConfiguration
// waiting on a response it would itself have to route. The 3s
// timeout fails the test long before the test binary itself times
// out, so failures are loud and easy to triage.
func TestDispatcher_NoDeadlockOnDidChangeConfiguration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv, client, shutdown := newPipedSession(t, ctx)
	defer shutdown()

	// Initialize with workspace.configuration support. Without this
	// capability flag the server skips the conn.Call paths under
	// test entirely, so we'd never exercise the deadlock window.
	var initResult protocol.InitializeResult
	_, err := client.conn.Call(ctx, protocol.MethodInitialize, &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "dispatcher-smoke", Version: protocol.NewOptional("0")},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				Configuration: &trueValue,
			},
		},
	}, &initResult)
	require.NoError(t, err)
	require.NotNil(t, initResult.ServerInfo)

	// initialized fires the followup goroutine (registerCapability +
	// workspace/configuration pull). Our client handler responds to
	// both; routing those responses through the same reader the
	// followup goroutine waits on is itself a smoke check on
	// AttachConn correctness.
	require.NoError(t, client.conn.Notify(ctx, protocol.MethodInitialized, &protocol.InitializedParams{}))
	// jsonrpc2's Notify returns the moment the bytes hit the pipe,
	// not when the server has dispatched the handler. The followup-
	// goroutine WaitGroup is incremented INSIDE the handler, so a
	// naive WaitInitialized right after Notify races: the server may
	// not have read the Notify yet and the WaitGroup is still 0.
	// Drain the dispatcher first with a synchronous probe — any
	// no-side-effect Call works; documentSymbol on a not-open URI
	// returns nil and is FIFO-ordered after the Notify, so when it
	// returns we know dispatchInitialized has run.
	syncProbe(t, ctx, client.conn)
	srv.WaitInitialized()

	// The actual regression scenario: didChangeConfiguration triggers
	// a second workspace/configuration round-trip. This used to
	// hang the dispatcher.
	require.NoError(t, client.conn.Notify(ctx, protocol.MethodWorkspaceDidChangeConfiguration,
		&protocol.DidChangeConfigurationParams{}))
	syncProbe(t, ctx, client.conn)
	srv.WaitConfigChange()

	// The dispatcher MUST still be processing messages after the
	// change. didOpen kicks off analyze + publishDiagnostics; receipt
	// of the publish is proof that didOpen was dispatched.
	// URI and content deliberately inert: a file:// path that doesn't
	// match `.protolsp.pxf` and content with no @type, so dispatchDidOpen
	// doesn't take the isWorkspaceConfigDoc / isChainSpecDoc branches
	// (those run a live-reload INSIDE the handler — under -race that
	// interleaves with the still-in-flight publishDiagnostics write and
	// produces a different deadlock that has nothing to do with the
	// regression under test). The bare analyze + publishDiagnostics path
	// is what we want to exercise.
	docURI := uri.URI("file:///nonexistent/smoke.pxf")
	require.NoError(t, client.conn.Notify(ctx, protocol.MethodTextDocumentDidOpen,
		&protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:        docURI,
				LanguageID: "pxf",
				Version:    1,
				Text:       "# inert pxf — no @type, not a config / chain doc\n",
			},
		}))

	select {
	case got := <-client.publishes:
		assert.Equal(t, docURI, got.URI,
			"publishDiagnostics for the just-opened URI is the proof the dispatcher kept routing after didChangeConfiguration")
	case <-time.After(3 * time.Second):
		t.Fatal("textDocument/publishDiagnostics for didOpen did not arrive within 3s — " +
			"dispatcher deadlock regression; some handler likely calls s.conn.Call inline")
	}
}

// syncProbe issues a synchronous Call whose only purpose is to drain
// the server's dispatcher up to the current message position. jsonrpc2
// processes messages FIFO on a single goroutine, so a Call sent after
// a Notify can't return until the Notify's handler has finished.
//
// Used by the test to bridge between fire-and-forget LSP notifications
// (Initialized, didChangeConfiguration) and the WaitGroups their
// handlers populate. Without this, WaitInitialized / WaitConfigChange
// race: WaitGroup.Add(1) happens inside the handler, but the test
// reaches Wait() before the handler has been dispatched at all.
//
// documentSymbol against a not-open URI is a safe choice: protolsp
// returns nil without side effects (document_symbol.go:dispatchDocumentSymbol).
func syncProbe(t *testing.T, ctx context.Context, conn jsonrpc2.Conn) {
	t.Helper()
	var dummy any
	_, err := conn.Call(ctx, protocol.MethodTextDocumentDocumentSymbol,
		&protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{
				URI: uri.URI("file:///dispatcher-smoke/sync-probe"),
			},
		}, &dummy)
	require.NoError(t, err, "sync probe Call should always succeed; failure here means the dispatcher itself is broken")
}

// pipedClient is the test-side surface: a jsonrpc2 connection driven
// over io.Pipe, plus the channels we use to assert on server-initiated
// traffic (workspace/configuration pulls, publishDiagnostics notifies).
type pipedClient struct {
	conn      jsonrpc2.Conn
	publishes chan *protocol.PublishDiagnosticsParams
}

// newPipedSession wires Server <-> client over two pipes and starts
// both jsonrpc2 dispatchers. Returns the Server, the client harness,
// and a shutdown function the caller defers. The shutdown function
// closes both pipes and waits for the run goroutines to exit.
//
// Uses os.Pipe() rather than io.Pipe() deliberately: jsonrpc2's
// dispatchers are single-goroutine on both sides, so a bidirectional
// request/response exchange where each side has nontrivial work
// between read and reply will deadlock on a zero-buffer io.Pipe (one
// side's write blocks because the other side's reader is in a handler;
// the other side's reply blocks because the first side's reader is
// still in its handler). Production stdio uses OS pipes whose kernel
// buffer (~64 KiB on Linux/macOS) absorbs the interleaving — the
// test must use the same to avoid a deadlock that has nothing to do
// with the regression under test. The race detector almost always
// catches that bogus deadlock without this; without -race, it surfaces
// intermittently as flakes.
func newPipedSession(t *testing.T, ctx context.Context) (*server.Server, *pipedClient, func()) {
	t.Helper()

	// Two OS pipes form a duplex channel:
	//   clientToServer: client writes, server reads
	//   serverToClient: server writes, client reads
	c2sR, c2sW, err := os.Pipe()
	require.NoError(t, err)
	s2cR, s2cW, err := os.Pipe()
	require.NoError(t, err)

	srv := server.New(discardLogger(t))
	serverRWC := &pipeRWC{r: c2sR, w: s2cW, name: "server"}
	srvStream := jsonrpc2.NewStream(serverRWC)
	srvConn := jsonrpc2.NewConn(srvStream, jsonrpc2.WithCodec(lspcodec.Codec{}))
	srv.AttachConn(srvConn)
	srvConn.Go(ctx, srv.Handle)

	client := &pipedClient{
		publishes: make(chan *protocol.PublishDiagnosticsParams, 8),
	}
	clientRWC := &pipeRWC{r: s2cR, w: c2sW, name: "client"}
	clientStream := jsonrpc2.NewStream(clientRWC)
	client.conn = jsonrpc2.NewConn(clientStream, jsonrpc2.WithCodec(lspcodec.Codec{}))
	client.conn.Go(ctx, client.handle)

	shutdown := func() {
		// Close both pipe writers; the readers return EOF and the
		// jsonrpc2 run goroutines exit. Done channels close shortly
		// after; we wait on them to keep the test deterministic.
		_ = s2cW.Close()
		_ = c2sW.Close()
		select {
		case <-srvConn.Done():
		case <-time.After(2 * time.Second):
			t.Log("server jsonrpc2 conn.Done() did not close within 2s after pipe close")
		}
		select {
		case <-client.conn.Done():
		case <-time.After(2 * time.Second):
			t.Log("client jsonrpc2 conn.Done() did not close within 2s after pipe close")
		}
	}
	return srv, client, shutdown
}

// handle answers the small set of server-to-client traffic protolsp
// actually issues during the scenario under test:
//   - workspace/configuration: return one null per requested section.
//     The server treats null as "client has no opinion" and keeps
//     its current value, which is exactly what we want here (we're
//     not testing config application, just dispatcher liveness).
//   - client/registerCapability: nil response. The server doesn't
//     act on the result.
//   - window/showMessage: notification, no reply expected.
//   - textDocument/publishDiagnostics: notification we surface to
//     the test via a channel.
//
// Any other inbound method gets MethodNotFound back — useful as a
// fail-fast if a new server-initiated call shows up unexpectedly.
func (c *pipedClient) handle(ctx context.Context, req *jsonrpc2.Request) (any, error) {
	switch req.Method() {
	case protocol.MethodWorkspaceConfiguration:
		var params protocol.ConfigurationParams
		if err := protocol.Unmarshal(req.Params(), &params); err != nil {
			return nil, err
		}
		nulls := make([]any, len(params.Items))
		return nulls, nil
	case protocol.MethodClientRegisterCapability:
		return nil, nil
	case protocol.MethodWindowShowMessage:
		// Notification — no reply payload; reply with nil per jsonrpc2.
		return nil, nil
	case protocol.MethodTextDocumentPublishDiagnostics:
		var params protocol.PublishDiagnosticsParams
		if err := protocol.Unmarshal(req.Params(), &params); err != nil {
			return nil, err
		}
		select {
		case c.publishes <- &params:
		default:
			// Channel full — drop. Test failure surface is the
			// receive side timing out, not this drop, so we don't
			// fail loudly here.
		}
		return nil, nil
	default:
		return jsonrpc2.MethodNotFoundHandler(ctx, req)
	}
}

// pipeRWC adapts an io.Reader + io.Writer into the io.ReadWriteCloser
// jsonrpc2.NewStream expects. Close shuts down both halves so the
// server-side stdio loop exits cleanly. The name field exists to make
// any future debugging-by-print easier.
type pipeRWC struct {
	r    io.Reader
	w    io.Writer
	name string

	closeOnce sync.Once
	closeErr  error
}

func (p *pipeRWC) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeRWC) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeRWC) Close() error {
	p.closeOnce.Do(func() {
		// io.PipeReader and io.PipeWriter expose Close; the input
		// here can be either side. Best-effort close both.
		if rc, ok := p.r.(io.Closer); ok {
			if err := rc.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				p.closeErr = err
			}
		}
		if wc, ok := p.w.(io.Closer); ok {
			if err := wc.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) && p.closeErr == nil {
				p.closeErr = err
			}
		}
	})
	return p.closeErr
}

var trueValue = true
