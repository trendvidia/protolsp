// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

// initializeWithPositionEncodings drives initialize with the
// client advertising the given positionEncodings list (LSP 3.17
// shape). Returns the server, the fake conn, and the decoded
// initialize result so tests can inspect the negotiated value.
// initializeWire is the initialize result as a client decodes it.
type initializeWire struct {
	Capabilities struct {
		PositionEncoding string `json:"positionEncoding"`
	} `json:"capabilities"`
}

func initializeWithPositionEncodings(t *testing.T, clientEncodings []string) (*Server, *fakeConn, *initializeWire) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	// Build raw initialize params with the LSP 3.17 field at
	// capabilities.general.positionEncodings. protocol@v0.12.0's
	// typed InitializeParams doesn't model `general`, so we hand-
	// build the JSON.
	rawParams := map[string]any{
		"capabilities": map[string]any{
			"general": map[string]any{
				"positionEncodings": clientEncodings,
			},
		},
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, rawParams)
	require.NoError(t, err)

	var captured *initializeWire
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result != nil {
			b, err := json.Marshal(result)
			require.NoError(t, err)
			captured = &initializeWire{}
			require.NoError(t, json.Unmarshal(b, captured))
		}
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return srv, fc, captured
}

func TestPositionEncoding_NegotiatesUTF8WhenClientOffersIt(t *testing.T) {
	srv, _, result := initializeWithPositionEncodings(t, []string{"utf-16", "utf-8"})
	assert.Equal(t, positions.UTF8, srv.PositionEncoding(),
		"client advertising utf-8 → server picks utf-8")
	require.NotNil(t, result)
	assert.Equal(t, "utf-8", result.Capabilities.PositionEncoding,
		"server advertises its choice in the initialize response")
}

func TestPositionEncoding_FallsBackToUTF16WhenUTF8NotOffered(t *testing.T) {
	srv, _, result := initializeWithPositionEncodings(t, []string{"utf-16"})
	assert.Equal(t, positions.UTF16, srv.PositionEncoding(),
		"only utf-16 in client list → utf-16")
	require.NotNil(t, result)
	assert.Equal(t, "utf-16", result.Capabilities.PositionEncoding)
}

func TestPositionEncoding_FallsBackToUTF16WhenListAbsent(t *testing.T) {
	// Pre-3.17 client: no positionEncodings field at all.
	srv, fc := newServerWithFakeConn(t)
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize,
		&protocol.InitializeParams{ClientInfo: protocol.ClientInfo{Name: "old-client"}})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))
	_ = fc
	assert.Equal(t, positions.UTF16, srv.PositionEncoding(),
		"pre-LSP-3.17 client (no positionEncodings field) → utf-16 default")
}

func TestPositionEncoding_DidChangeUsesNegotiatedEncoding(t *testing.T) {
	// End-to-end: server negotiated to UTF-8, didChange edit
	// targets a position by byte offset. Confirms that the
	// edit-apply path is encoding-aware — a UTF-16 interpretation
	// would land on a different byte for non-ASCII content.
	srv, _, _ := initializeWithPositionEncodings(t, []string{"utf-8"})
	require.Equal(t, positions.UTF8, srv.PositionEncoding())

	// Open with content that has a 4-byte emoji + ASCII suffix.
	// "😀ab" — emoji is bytes 0-3, 'a' is byte 4, 'b' is byte 5.
	const initial = "😀ab"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/u.pxf", 1, initial)))

	// Replace 'a' (byte 4 in UTF-8) with 'X'. With UTF-8
	// negotiated, character=4 IS byte 4.
	change := &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{
				URI: "file:///work/u.pxf",
			},
			Version: 2,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{&protocol.TextDocumentContentChangePartial{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 4},
				End:   protocol.Position{Line: 0, Character: 5},
			},
			Text: "X",
		}},
	}
	notif, err := newTestNotification(protocol.MethodTextDocumentDidChange, change)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, notif))

	doc := srv.Documents().Get("file:///work/u.pxf")
	require.NotNil(t, doc)
	assert.Equal(t, "😀Xb", string(doc.Source),
		"UTF-8 negotiated: character index 4 maps to byte 4 (the 'a' after the emoji)")
}

func TestPositionEncoding_DidChangeWithUTF16InterpretsNonBMPCorrectly(t *testing.T) {
	// Default UTF-16 client. Same source: emoji uses 2 UTF-16
	// code units; ASCII chars after the emoji are at character 2
	// (NOT 4).
	srv, _ := newServerWithFakeConn(t)
	require.Equal(t, positions.UTF16, srv.PositionEncoding(),
		"no initialize → defaults to UTF-16")

	const initial = "😀ab"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/u.pxf", 1, initial)))

	// Replace 'a' — character=2 in UTF-16 (after the 2 surrogate
	// code units of the emoji).
	change := &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{
				URI: "file:///work/u.pxf",
			},
			Version: 2,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{&protocol.TextDocumentContentChangePartial{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 2},
				End:   protocol.Position{Line: 0, Character: 3},
			},
			Text: "X",
		}},
	}
	notif, err := newTestNotification(protocol.MethodTextDocumentDidChange, change)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, notif))

	doc := srv.Documents().Get("file:///work/u.pxf")
	require.NotNil(t, doc)
	assert.Equal(t, "😀Xb", string(doc.Source),
		"UTF-16 default: character index 2 maps past the surrogate pair to the 'a'")
}

func TestPositionEncoding_AdvertisedInInitializeResult(t *testing.T) {
	// Even without a positionEncodings list from the client, the
	// server advertises its chosen value (utf-16 by default).
	// Some clients require the field to be present.
	_, _, result := initializeWithPositionEncodings(t, nil)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.Capabilities.PositionEncoding,
		"server always advertises a position encoding in the initialize response")
}
