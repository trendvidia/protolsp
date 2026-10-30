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

// semanticTokensFromHandle drives the dispatcher and returns the
// decoded SemanticTokens response (or nil on absence). Round-trips
// through JSON so tests see the on-wire shape.
func semanticTokensFromHandle(t *testing.T, srv *Server, u string) *protocol.SemanticTokens {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentSemanticTokensFull,
		&protocol.SemanticTokensParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
		})
	require.NoError(t, err)

	var captured *protocol.SemanticTokens
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var st protocol.SemanticTokens
		require.NoError(t, json.Unmarshal(b, &st))
		captured = &st
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return captured
}

// decodedToken is the inflated form of one 5-uint32 LSP semantic-token
// entry. Tests compare against this so failures point at the actual
// fields rather than positions inside a flat uint32 slice.
type decodedToken struct {
	line, char, length uint32
	tokenType          protocol.SemanticTokenTypes
	modifiers          uint32 // bitset over semanticTokenModifiers
}

func decodeSemanticTokens(t *testing.T, data []uint32) []decodedToken {
	t.Helper()
	require.Zero(t, len(data)%5, "semantic-token data must be a multiple of 5 elements")
	out := make([]decodedToken, 0, len(data)/5)
	var line, char uint32
	for i := 0; i < len(data); i += 5 {
		dLine := data[i]
		dStart := data[i+1]
		line += dLine
		if dLine == 0 {
			char += dStart
		} else {
			char = dStart
		}
		typeIdx := data[i+3]
		require.Less(t, int(typeIdx), len(SemanticTokenTypes), "token type index in legend bounds")
		out = append(out, decodedToken{
			line:      line,
			char:      char,
			length:    data[i+2],
			tokenType: SemanticTokenTypes[typeIdx],
			modifiers: data[i+4],
		})
	}
	return out
}

func TestSemanticTokens_TypeDirectiveAndField(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	st := semanticTokensFromHandle(t, srv, "file:///work/g.pxf")
	require.NotNil(t, st)
	tokens := decodeSemanticTokens(t, st.Data)
	require.NotEmpty(t, tokens, "expected tokens for @type + name field + string value")

	// Token 0: `@type` macro on line 0, col 0, length 5.
	assert.Equal(t, decodedToken{line: 0, char: 0, length: 5, tokenType: protocol.SemanticTokenTypesMacro}, tokens[0])
	// Token 1: `acme.demo.v1.Greeting` namespace on line 0 col 6.
	assert.Equal(t, decodedToken{
		line: 0, char: 6, length: uint32(len("acme.demo.v1.Greeting")),
		tokenType: protocol.SemanticTokenTypesNamespace,
	}, tokens[1])

	// Locate the `name` property + the "world" string. Both sit on
	// line 2 (after the @type line + the blank line).
	var nameTok, strTok *decodedToken
	for i := range tokens {
		if tokens[i].tokenType == protocol.SemanticTokenTypesProperty {
			nameTok = &tokens[i]
		}
		if tokens[i].tokenType == protocol.SemanticTokenTypesString {
			strTok = &tokens[i]
		}
	}
	require.NotNil(t, nameTok, "expected a property token for `name`")
	assert.Equal(t, uint32(2), nameTok.line)
	assert.Equal(t, uint32(0), nameTok.char)
	assert.Equal(t, uint32(4), nameTok.length)

	require.NotNil(t, strTok, "expected a string token for the value")
	assert.Equal(t, uint32(2), strTok.line)
	assert.Equal(t, uint32(len(`"world"`)), strTok.length, "string token covers the quoted span")
}

func TestSemanticTokens_NumberAndBoolValues(t *testing.T) {
	const src = `@type acme.demo.v1.Greeting

count = 42
flag = true
`
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/n.pxf", 1, src)))

	st := semanticTokensFromHandle(t, srv, "file:///work/n.pxf")
	require.NotNil(t, st)
	tokens := decodeSemanticTokens(t, st.Data)

	var sawNumber, sawKeyword bool
	for _, tk := range tokens {
		if tk.tokenType == protocol.SemanticTokenTypesNumber {
			sawNumber = true
		}
		if tk.tokenType == protocol.SemanticTokenTypesKeyword {
			sawKeyword = true
		}
	}
	assert.True(t, sawNumber, "IntVal should produce a number token")
	assert.True(t, sawKeyword, "BoolVal should produce a keyword token")
}

func TestSemanticTokens_ReturnsEmptyWhenParseFails(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	// `=` with nothing on the left side is a parse error in the
	// grammar — we still return an empty (not nil) SemanticTokens
	// so clients don't drop their cached state.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/bad.pxf", 1, "= 1\n")))

	st := semanticTokensFromHandle(t, srv, "file:///work/bad.pxf")
	require.NotNil(t, st, "open document → non-nil response even when parse fails")
	assert.Empty(t, st.Data)
}

func TestSemanticTokens_ReturnsNilWhenDocumentNotOpen(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	st := semanticTokensFromHandle(t, srv, "file:///work/missing.pxf")
	assert.Nil(t, st)
}

func TestSemanticTokens_NestedBlockEmitsStructAndChildProperty(t *testing.T) {
	const nested = `@type acme.demo.v1.Greeting

outer {
  inner_field = "x"
}
`
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/n.pxf", 1, nested)))

	st := semanticTokensFromHandle(t, srv, "file:///work/n.pxf")
	require.NotNil(t, st)
	tokens := decodeSemanticTokens(t, st.Data)

	var structTok, innerTok *decodedToken
	for i := range tokens {
		if tokens[i].tokenType == protocol.SemanticTokenTypesStruct {
			structTok = &tokens[i]
		}
		if tokens[i].tokenType == protocol.SemanticTokenTypesProperty &&
			tokens[i].length == uint32(len("inner_field")) {
			innerTok = &tokens[i]
		}
	}
	require.NotNil(t, structTok, "block name should produce a struct token")
	require.NotNil(t, innerTok, "nested assignment key should produce a property token")
	assert.Greater(t, innerTok.line, structTok.line, "inner field appears below the block header")
}

func TestSemanticTokens_AdvertisesCapabilityAndLegend(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodInitialize,
		&protocol.InitializeParams{})
	require.NoError(t, err)
	var caps map[string]interface{}
	reply := func(_ context.Context, result interface{}, _ error) error {
		b, _ := json.Marshal(result)
		var probe struct {
			Capabilities map[string]interface{} `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(b, &probe))
		caps = probe.Capabilities
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))

	provider, ok := caps["semanticTokensProvider"].(map[string]interface{})
	require.True(t, ok, "initialize result must advertise SemanticTokensProvider")
	assert.Equal(t, true, provider["full"], "MVP advertises full=true")
	assert.Equal(t, false, provider["range"], "MVP does not advertise range support")

	legend, ok := provider["legend"].(map[string]interface{})
	require.True(t, ok)
	types, ok := legend["tokenTypes"].([]interface{})
	require.True(t, ok)
	// Index 0 must remain `macro` — the stMacro constant is hard-
	// coded to 0 and changing legend order is a wire-breaking edit.
	require.NotEmpty(t, types)
	assert.Equal(t, string(protocol.SemanticTokenTypesMacro), types[0])
}
