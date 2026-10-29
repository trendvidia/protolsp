// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// fakeConn is a stand-in for jsonrpc2.Conn that records every Notify
// invocation. Used to assert which diagnostics the Server publishes.
// Call/Go/Done/Err are stubbed to satisfy the interface — none are
// invoked by the M2 code paths under test.
type fakeConn struct {
	mu             sync.Mutex
	notify         []notifyCall
	showMessages   []protocol.ShowMessageParams
	configChanged  []EffectiveConfigChangedParams
	secretChanges  []SecretRangesChangedParams
	datasetChanges []DatasetRegionsChangedParams

	// calls records every server→client Call invocation. Tests
	// assert against the recorded method names + params.
	callRecords []callRecord
	// callResponses maps method → canned response bytes (JSON-encoded).
	// When pullEditorConfig or registerDynamicCapabilities sends a
	// Call to the client, the matching response is written into the
	// caller's result pointer. Unset methods get a null response.
	callResponses map[string]json.RawMessage
}

type callRecord struct {
	method string
	params []byte // JSON-encoded params for assertions
}

type notifyCall struct {
	method string
	params *protocol.PublishDiagnosticsParams
}

func (f *fakeConn) Notify(_ context.Context, method string, params interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Round-trip through JSON so the captured params reflect what a real
	// peer would see (no shared-pointer aliasing with the caller).
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	switch method {
	case protocol.MethodTextDocumentPublishDiagnostics:
		var p protocol.PublishDiagnosticsParams
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		f.notify = append(f.notify, notifyCall{method: method, params: &p})
	case protocol.MethodWindowShowMessage:
		var p protocol.ShowMessageParams
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		f.showMessages = append(f.showMessages, p)
	case MethodEffectiveConfigChanged:
		var p EffectiveConfigChangedParams
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		f.configChanged = append(f.configChanged, p)
	case MethodSecretRangesChanged:
		var p SecretRangesChangedParams
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		f.secretChanges = append(f.secretChanges, p)
	case MethodDatasetRegionsChanged:
		var p DatasetRegionsChangedParams
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		f.datasetChanges = append(f.datasetChanges, p)
	default:
		// Other notifications aren't asserted in tests today; record
		// nothing rather than fail unrecognized methods.
	}
	return nil
}

// Call records the invocation and, when callResponses has a canned
// reply for the method, writes it into result via JSON round-trip.
// Result types are typed by the caller (json.RawMessage for
// workspace/configuration); we marshal back to bytes and unmarshal
// into result to honor that.
func (f *fakeConn) Call(_ context.Context, method string, params, result interface{}) (jsonrpc2.ID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pb, err := json.Marshal(params)
	if err != nil {
		return jsonrpc2.ID{}, err
	}
	f.callRecords = append(f.callRecords, callRecord{method: method, params: pb})
	if result == nil {
		return jsonrpc2.ID{}, nil
	}
	resp, ok := f.callResponses[method]
	if !ok {
		return jsonrpc2.ID{}, nil // no canned response → leave result zero-valued
	}
	if err := json.Unmarshal(resp, result); err != nil {
		return jsonrpc2.ID{}, err
	}
	return jsonrpc2.ID{}, nil
}

// callsByMethod returns all Call invocations targeting the given
// method, in the order they happened. Convenience for assertions.
func (f *fakeConn) callsByMethod(method string) []callRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []callRecord
	for _, c := range f.callRecords {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// setCallResponse stages a canned response for the next Call to
// `method`. The bytes are returned verbatim (and unmarshaled into
// the caller's result pointer). Tests call this before triggering
// the path that does the Call.
func (f *fakeConn) setCallResponse(method string, payload interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callResponses == nil {
		f.callResponses = map[string]json.RawMessage{}
	}
	b, _ := json.Marshal(payload)
	f.callResponses[method] = b
}
func (f *fakeConn) Go(context.Context, jsonrpc2.Handler) {}
func (f *fakeConn) Close() error                         { return nil }
func (f *fakeConn) Done() <-chan struct{}                { return nil }
func (f *fakeConn) Err() error                           { return nil }

func (f *fakeConn) calls() []notifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notifyCall, len(f.notify))
	copy(out, f.notify)
	return out
}

func (f *fakeConn) shownMessages() []protocol.ShowMessageParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]protocol.ShowMessageParams, len(f.showMessages))
	copy(out, f.showMessages)
	return out
}

func (f *fakeConn) configChangedNotifications() []EffectiveConfigChangedParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]EffectiveConfigChangedParams, len(f.configChanged))
	copy(out, f.configChanged)
	return out
}

func (f *fakeConn) secretRangesChangedNotifications() []SecretRangesChangedParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SecretRangesChangedParams, len(f.secretChanges))
	copy(out, f.secretChanges)
	return out
}

func (f *fakeConn) datasetRegionsChangedNotifications() []DatasetRegionsChangedParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]DatasetRegionsChangedParams, len(f.datasetChanges))
	copy(out, f.datasetChanges)
	return out
}

func newServerWithFakeConn(t *testing.T) (*Server, *fakeConn) {
	t.Helper()
	srv := New(discardLogger())
	// Default tests off the buf-lint pass — existing proto
	// fixtures weren't designed to pass buf's default ruleset, and
	// invoking a subprocess against whatever binary the dev
	// machine happens to have installed would slow the suite +
	// produce surprise diagnostics. Phase-5b lint tests flip this
	// back on explicitly.
	srv.SetBufLint(false)
	fc := &fakeConn{}
	srv.AttachConn(fc)
	return srv, fc
}

// validPxf parses cleanly. The `@type` directive binds the document to
// a fully-qualified message — M3 will use it to fetch the schema, M2
// just needs it to round-trip through pxf.Parse without error.
//
// pfx uses `=` for top-level field assignment (the `:` form is only
// allowed inside `{ … }` blocks as map-entry syntax).
const validPxf = `@type acme.demo.v1.Greeting

name = "world"
`

// invalidPxf trips the parser somewhere — exact position and error
// text are intentionally not asserted in the integration tests below
// (position conversion is covered separately by TestPxfPosToLSP).
// `name:` at top level uses map-entry syntax outside a block, which
// the parser rejects.
const invalidPxf = `name: "world"
`

func TestDidOpen_ValidPxfPublishesEmptyDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	call := didOpenCall(t, "file:///work/greet.pxf", 1, validPxf)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Equal(t, protocol.MethodTextDocumentPublishDiagnostics, notes[0].method)
	assert.Equal(t, uri.URI("file:///work/greet.pxf"), notes[0].params.URI)
	assert.Empty(t, notes[0].params.Diagnostics, "valid input → no diagnostics")

	doc := srv.Documents().Get("file:///work/greet.pxf")
	require.NotNil(t, doc, "didOpen must store the document")
	assert.NotNil(t, doc.Parsed, "valid input → parsed AST cached")
	assert.NoError(t, doc.ParseErr)
}

func TestDidOpen_InvalidPxfPublishesParseDiagnostic(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	call := didOpenCall(t, "file:///work/bad.pxf", 1, invalidPxf)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := notes[0].params.Diagnostics
	require.Len(t, diags, 1, "exactly one parse-error diagnostic")
	d := diags[0]
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	assert.Equal(t, "protolsp/parse", d.Source)
	assert.NotEmpty(t, d.Message, "diagnostic must carry the parser's error message")
	// Range is non-empty (start before end) so editors render a squiggle
	// rather than collapsing the diagnostic. Specific line/column for a
	// given input is covered by TestPxfPosToLSP.
	assert.True(t, d.Range.End.Character > d.Range.Start.Character ||
		d.Range.End.Line > d.Range.Start.Line,
		"diagnostic range must be non-empty")

	doc := srv.Documents().Get("file:///work/bad.pxf")
	require.NotNil(t, doc)
	assert.Nil(t, doc.Parsed, "parse failed → no AST")
	require.Error(t, doc.ParseErr)
}

func TestDidChange_RepublishesDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, validPxf)))
	// First open published an empty-diagnostics notification.
	require.Len(t, fc.calls(), 1)
	assert.Empty(t, fc.calls()[0].params.Diagnostics)

	// Modify to something invalid; expect a new publish with one diag.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 2, invalidPxf)))
	notes := fc.calls()
	require.Len(t, notes, 2)
	assert.Len(t, notes[1].params.Diagnostics, 1)

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	assert.Equal(t, int32(2), doc.Version, "didChange must bump version")
	assert.Nil(t, doc.Parsed)
}

func TestDidClose_RemovesDocumentAndClearsDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, invalidPxf)))
	require.Len(t, fc.calls(), 1)
	require.Len(t, fc.calls()[0].params.Diagnostics, 1, "invalid open → one diag")

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didCloseCall(t, "file:///work/g.pxf")))

	notes := fc.calls()
	require.Len(t, notes, 2, "didClose must publish an empty list to clear squiggles")
	assert.Empty(t, notes[1].params.Diagnostics)
	assert.Nil(t, srv.Documents().Get("file:///work/g.pxf"), "didClose must drop the document")
}

func TestPxfPosToLSP_OneIndexedToZeroIndexed(t *testing.T) {
	// pxf reports line/column 1-indexed; LSP wants them 0-indexed.
	// A defensive clamp keeps any synthetic zero-positions at the
	// document origin instead of underflowing.
	require.Equal(t, protocol.Position{Line: 0, Character: 0},
		PxfPosToLSP(pxfPos(1, 1)))
	require.Equal(t, protocol.Position{Line: 4, Character: 9},
		PxfPosToLSP(pxfPos(5, 10)))
	require.Equal(t, protocol.Position{Line: 0, Character: 0},
		PxfPosToLSP(pxfPos(0, 0)),
		"zero or negative positions must clamp to origin")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func noopReply(context.Context, interface{}, error) error { return nil }

func didOpenCall(t *testing.T, u string, version int32, text string) jsonrpc2.Request {
	t.Helper()
	notif, err := jsonrpc2.NewNotification(protocol.MethodTextDocumentDidOpen,
		&protocol.DidOpenTextDocumentParams{
			TextDocument: protocol.TextDocumentItem{
				URI:        uri.URI(u),
				LanguageID: "pxf",
				Version:    version,
				Text:       text,
			},
		})
	require.NoError(t, err)
	return notif
}

// didChangeCall builds a textDocument/didChange notification with a
// single full-document content-change event. Sent as raw JSON to
// honor the LSP spec's "absent range = full document" convention —
// the upstream protocol.TextDocumentContentChangeEvent's value-Range
// field would otherwise marshal as a zero-valued range object
// (defeating the incremental-vs-full discrimination on the server
// side).
func didChangeCall(t *testing.T, u string, version int32, text string) jsonrpc2.Request {
	t.Helper()
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":     u,
			"version": version,
		},
		"contentChanges": []map[string]any{
			{"text": text}, // no "range" key → full-document fallback
		},
	}
	notif, err := jsonrpc2.NewNotification(protocol.MethodTextDocumentDidChange, params)
	require.NoError(t, err)
	return notif
}

// didChangeIncrementalCall is the incremental counterpart of
// didChangeCall: builds a single content-change event with an
// explicit range + replacement text. Used by I1 tests.
func didChangeIncrementalCall(t *testing.T, u string, version int32, r protocol.Range, newText string) jsonrpc2.Request {
	t.Helper()
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":     u,
			"version": version,
		},
		"contentChanges": []map[string]any{
			{
				"range": map[string]any{
					"start": map[string]any{"line": r.Start.Line, "character": r.Start.Character},
					"end":   map[string]any{"line": r.End.Line, "character": r.End.Character},
				},
				"text": newText,
			},
		},
	}
	notif, err := jsonrpc2.NewNotification(protocol.MethodTextDocumentDidChange, params)
	require.NoError(t, err)
	return notif
}

func didCloseCall(t *testing.T, u string) jsonrpc2.Request {
	t.Helper()
	notif, err := jsonrpc2.NewNotification(protocol.MethodTextDocumentDidClose,
		&protocol.DidCloseTextDocumentParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
		})
	require.NoError(t, err)
	return notif
}

// pxfPos is a tiny constructor so tests don't have to import the pxf
// package just to build expected positions for PxfPosToLSP.
func pxfPos(line, col int) pxfPosition {
	return pxfPosition{Line: line, Column: col}
}

// pxfPosition mirrors pxf.Position's exported fields. Re-declared in
// the test package to avoid an extra package import for one struct;
// PxfPosToLSP accepts the real pxf.Position so type compatibility is
// what matters at the call site.
type pxfPosition = struct {
	Line   int
	Column int
	Offset int
}
