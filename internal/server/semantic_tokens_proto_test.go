// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Semantic tokens for .proto documents (#218 Phase A). The server
// highlights ONLY the RFC-001 schema-extension constructs — stock
// proto syntax (message/enum/rpc/...) stays with the editor's own
// grammar layer, so these tests also pin that classic constructs
// emit no tokens.

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

const semanticTokenDecorator = protocol.SemanticTokenTypes("decorator")

// rfc001TokensProto exercises every RFC-001 token shape at a known
// position: the three declaration keywords, a leading use on a
// message, a trailing use on a field, and an unbound (orphan) use.
const rfc001TokensProto = `syntax = "proto3";

package test.rfc001.tokens;

type UserID = string;
function is_email(s: string);
annotation required;
annotation description(text: string);

@description("primary email")
message Email {
  string value = 1 @description("RFC-5322 address");
}

@orphan_use
`

func TestSemanticTokens_ProtoRFC001KeywordsAndUses(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/rfc001.proto", 1, rfc001TokensProto)))

	st := semanticTokensFromHandle(t, srv, "file:///work/rfc001.proto")
	require.NotNil(t, st)
	tokens := decodeSemanticTokens(t, st.Data)

	assert.Equal(t, []decodedToken{
		// Declaration keywords — the constructs stock proto grammars
		// mis-tokenize (a leading `type` reads as a field type).
		{line: 4, char: 0, length: uint32(len("type")), tokenType: protocol.SemanticTokenKeyword},
		{line: 5, char: 0, length: uint32(len("function")), tokenType: protocol.SemanticTokenKeyword},
		{line: 6, char: 0, length: uint32(len("annotation")), tokenType: protocol.SemanticTokenKeyword},
		{line: 7, char: 0, length: uint32(len("annotation")), tokenType: protocol.SemanticTokenKeyword},
		// Leading use bound to `message Email` — token covers
		// `@description` (sigil + name), not the argument list.
		{line: 9, char: 0, length: uint32(len("@description")), tokenType: semanticTokenDecorator},
		// Trailing use on the field, mid-line.
		{line: 11, char: 19, length: uint32(len("@description")), tokenType: semanticTokenDecorator},
		// Unbound use at EOF — nothing follows for the parser to
		// attach it to; it must still highlight while the user types.
		{line: 14, char: 0, length: uint32(len("@orphan_use")), tokenType: semanticTokenDecorator},
	}, tokens)
}

func TestSemanticTokens_ClassicProtoEmitsNoTokens(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/classic.proto", 1, validProto)))

	st := semanticTokensFromHandle(t, srv, "file:///work/classic.proto")
	require.NotNil(t, st, "open .proto → non-nil response")
	assert.Empty(t, st.Data,
		"pre-RFC-001 proto is entirely the editor grammar's job — the semantic layer stays silent")
}

// A broken .proto still produces tokens for whatever parsed — the
// fork's parser is fault-tolerant, and highlighting must not flicker
// off mid-edit just because the buffer is transiently invalid.
func TestSemanticTokens_MalformedProtoStillHighlightsParsedParts(t *testing.T) {
	const src = `syntax = "proto3";
package t;
annotation required
type Broken
`
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/broken.proto", 1, src)))

	st := semanticTokensFromHandle(t, srv, "file:///work/broken.proto")
	require.NotNil(t, st)
	tokens := decodeSemanticTokens(t, st.Data)

	var kinds []protocol.SemanticTokenTypes
	for _, tk := range tokens {
		kinds = append(kinds, tk.tokenType)
	}
	assert.Contains(t, kinds, protocol.SemanticTokenKeyword,
		"keyword tokens survive a partial parse")
}

func TestSemanticTokens_LegendIncludesDecorator(t *testing.T) {
	legend := SemanticTokensLegend()
	require.Greater(t, len(legend.TokenTypes), 8)
	// Index 8 must remain `decorator` — like the other indices it is
	// wire-frozen: clients resolve legend entries by position.
	assert.Equal(t, semanticTokenDecorator, legend.TokenTypes[8])
}
