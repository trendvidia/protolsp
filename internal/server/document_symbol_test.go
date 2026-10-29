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

// documentSymbolsFromHandle drives the documentSymbol dispatcher
// through jsonrpc2 and round-trips the response through JSON so tests
// observe the same shape a real client would.
func documentSymbolsFromHandle(t *testing.T, srv *Server, u string) []protocol.DocumentSymbol {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentDocumentSymbol,
		&protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
		})
	require.NoError(t, err)

	var captured []protocol.DocumentSymbol
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &captured))
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return captured
}

func TestDocumentSymbol_ReportsTypeAndTopLevelField(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	symbols := documentSymbolsFromHandle(t, srv, "file:///work/g.pxf")
	require.NotEmpty(t, symbols, "valid doc with @type + field should yield symbols")

	// First symbol is the @type directive.
	assert.Equal(t, "@type acme.demo.v1.Greeting", symbols[0].Name)
	assert.Equal(t, protocol.SymbolKindNamespace, symbols[0].Kind)

	// The `name = "world"` assignment is reported as a Field symbol
	// with the resolved protobuf type in Detail.
	var nameField *protocol.DocumentSymbol
	for i := range symbols {
		if symbols[i].Name == "name" {
			nameField = &symbols[i]
			break
		}
	}
	require.NotNil(t, nameField, "expected a `name` field symbol")
	assert.Equal(t, protocol.SymbolKindField, nameField.Kind)
	assert.Equal(t, "string", nameField.Detail, "field detail carries protobuf kind label")
}

func TestDocumentSymbol_ReturnsNilWhenDocumentNotOpen(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	symbols := documentSymbolsFromHandle(t, srv, "file:///work/missing.pxf")
	assert.Nil(t, symbols, "no open document → no symbols")
}

func TestDocumentSymbol_NestedBlocksProduceChildren(t *testing.T) {
	const nested = `@type acme.demo.v1.Greeting

outer {
  inner_field = "x"
}
`
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/n.pxf", 1, nested)))

	symbols := documentSymbolsFromHandle(t, srv, "file:///work/n.pxf")
	require.NotEmpty(t, symbols)

	var outer *protocol.DocumentSymbol
	for i := range symbols {
		if symbols[i].Name == "outer" {
			outer = &symbols[i]
			break
		}
	}
	require.NotNil(t, outer, "expected `outer` block symbol")
	assert.Equal(t, protocol.SymbolKindObject, outer.Kind)
	require.Len(t, outer.Children, 1, "outer should expose its single nested entry")
	assert.Equal(t, "inner_field", outer.Children[0].Name)
}

func TestDocumentSymbol_TypeOnlyDocReturnsOnlyTypeSymbol(t *testing.T) {
	const typeOnly = `@type acme.demo.v1.Greeting
`
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, typeOnly)))

	symbols := documentSymbolsFromHandle(t, srv, "file:///work/t.pxf")
	require.Len(t, symbols, 1)
	assert.Equal(t, "@type acme.demo.v1.Greeting", symbols[0].Name)
}

func TestDocumentSymbol_NoTypeDirectiveStillReportsEntries(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/u.pxf", 1, greetingNoTypeDirective)))

	symbols := documentSymbolsFromHandle(t, srv, "file:///work/u.pxf")
	require.NotEmpty(t, symbols, "missing @type shouldn't suppress field symbols")

	// The first (and only) symbol should be the `name` field. With
	// no @type the descriptor lookup is skipped so Detail is empty —
	// the outline still shows the structure, just without type hints.
	assert.Equal(t, "name", symbols[0].Name)
	assert.Equal(t, protocol.SymbolKindField, symbols[0].Kind)
	assert.Empty(t, symbols[0].Detail)
}

func TestDocumentSymbol_AdvertisesCapability(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	// Drive an initialize call so we observe the capabilities the
	// server returns. Empty params → no registry config; that's fine,
	// the capability advertisement doesn't depend on a registry.
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodInitialize,
		&protocol.InitializeParams{})
	require.NoError(t, err)
	var caps map[string]interface{}
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var probe struct {
			Capabilities map[string]interface{} `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(b, &probe))
		caps = probe.Capabilities
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	require.NotNil(t, caps)
	v, ok := caps["documentSymbolProvider"]
	require.True(t, ok, "initialize result must advertise documentSymbolProvider")
	assert.Equal(t, true, v)
}
