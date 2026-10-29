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
	"go.lsp.dev/uri"
)

// hoverFromHandle runs the hover dispatcher and returns the deserialized
// Hover response (or nil) plus any reply error. Mirrors what a real
// client would see over the wire after JSON round-trip.
func hoverFromHandle(t *testing.T, srv *Server, u string, line, char uint32) *protocol.Hover {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentHover,
		&protocol.HoverParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
				Position:     protocol.Position{Line: line, Character: char},
			},
		})
	require.NoError(t, err)

	var captured *protocol.Hover
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		// Round-trip through JSON so we see the over-the-wire shape.
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var h protocol.Hover
		require.NoError(t, json.Unmarshal(b, &h))
		captured = &h
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return captured
}

func TestHover_ReportsFieldTypeAndDefiningFile(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	// Open the valid Greeting doc so the document store + parsed AST
	// are populated.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	// greetingValid is:
	//   line 0:  @type acme.demo.v1.Greeting
	//   line 1:  (empty)
	//   line 2:  name = "world"
	// Cursor on line 2 lands on the "name" assignment.
	h := hoverFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	require.NotNil(t, h, "hover on a known field should return content")

	assert.Equal(t, protocol.Markdown, h.Contents.Kind)
	body := h.Contents.Value
	assert.Contains(t, body, "**name**", "hover should bold the field name")
	assert.Contains(t, body, "`string`", "hover should report the protobuf kind")
	assert.Contains(t, body, "acme/demo/v1/greeting.proto", "hover should name the defining file")
}

func TestHover_ReturnsNilForUnknownLine(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	// Line 100 is past the end of the document — no entry there.
	h := hoverFromHandle(t, srv, "file:///work/g.pxf", 100, 0)
	assert.Nil(t, h, "cursor outside any entry → no hover")
}

func TestHover_ReturnsNilWhenNoTypeDirective(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingNoTypeDirective)))

	h := hoverFromHandle(t, srv, "file:///work/g.pxf", 0, 0)
	assert.Nil(t, h, "no @type → no schema bound → no hover info")
}

func TestHover_ReturnsNilWhenNoLookupConfigured(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	// No lookup assignment.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	h := hoverFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	assert.Nil(t, h, "no Lookup → no schema info → no hover")
}

func TestHover_UnknownFieldOnKnownMessageShowsFallback(t *testing.T) {
	// When the user writes a field name that doesn't exist on the
	// bound message, hover still fires — but reports "unknown field"
	// rather than nothing, so the user gets immediate confirmation
	// they're looking at a bad name.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingUnknownField)))

	// Line 2 has "stranger = ..."
	h := hoverFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	require.NotNil(t, h)
	assert.Contains(t, h.Contents.Value, "**stranger**")
	assert.Contains(t, h.Contents.Value, "Unknown field")
}

// Hover/go-to-def regression: fields inside an array literal of
// inline blocks weren't resolving. For the canonical case
// `environments = [ { name = "prod" extends = "..." } ]` (chain.pxf
// shape), hovering on `name` or `extends` returned nil — the walker
// only descended into Block.Entries, never into Assignment.Value →
// ListVal → BlockVal. This test exercises the array-element path
// against an Order/Address fixture (`addresses` is repeated
// Address) since the chameleon ChainSpec isn't in the fakeLookup's
// type set.
func TestHover_ArrayElementFieldResolves(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildOrderWithAddressesType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@type acme.demo.v1.Order

addresses = [
  { street = "1 Main St" }
]
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/o.pxf", 1, src)))

	// Line 3 (0-indexed): "  { street = \"1 Main St\" }"
	// Cursor anywhere on the line (col 4) — hover is line-keyed,
	// not column-keyed, so any column works.
	h := hoverFromHandle(t, srv, "file:///work/o.pxf", 3, 4)
	require.NotNil(t, h, "hover on a field inside an array entry should resolve")
	body := h.Contents.Value
	assert.Contains(t, body, "**street**", "field name in markdown")
	assert.Contains(t, body, "`string`", "field type in markdown")
}

func TestInitialize_AdvertisesHoverProvider(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize,
		&protocol.InitializeParams{ClientInfo: &protocol.ClientInfo{Name: "test"}})
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), rep.reply, call))

	result := rep.result.(*initializeResult)
	assert.True(t, result.Capabilities.HoverProvider.(bool),
		"server must advertise hover capability in initialize result")
}
