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

// completionAt fires textDocument/completion at (uri, line, char)
// and returns the decoded CompletionList (or nil for no
// completions).
func completionAt(t *testing.T, srv *Server, docURI uri.URI, line, char uint32) *protocol.CompletionList {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentCompletion,
		&protocol.CompletionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
				Position:     protocol.Position{Line: line, Character: char},
			},
		})
	require.NoError(t, err)
	var got *protocol.CompletionList
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		got = &protocol.CompletionList{}
		require.NoError(t, json.Unmarshal(b, got))
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return got
}

func labelsOf(list *protocol.CompletionList) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.Label)
	}
	return out
}

func TestFieldRefCompletion_OffersPathsAfterDollarBrace(t *testing.T) {
	srv, _ := newAppConfigServer(t)
	// `api_url = "${|"` — cursor between `${` and `"`.
	const src = `@type c4test.v1.AppConfig

api_url = "${}"
`
	docURI := uri.URI("file:///work/c.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, src)))

	// Cursor: line 2, between `${` and `}` on `api_url = "${}"`.
	// Index map: a(0)p(1)i(2)_(3)u(4)r(5)l(6) (7)=(8) (9)"(10)
	// $(11){(12)}(13)"(14). Cursor at character 13 sits AT the `}`
	// — the position right after `${`.
	list := completionAt(t, srv, docURI, 2, 13)
	require.NotNil(t, list, "completion at ${} cursor must return items")
	labels := labelsOf(list)
	assert.Contains(t, labels, "api_url")
	assert.Contains(t, labels, "db")
	assert.Contains(t, labels, "db.host")
	assert.Contains(t, labels, "db.port")
}

func TestFieldRefCompletion_CursorOutsideDollarBraceReturnsNoFieldRefItems(t *testing.T) {
	srv, _ := newAppConfigServer(t)
	const src = `@type c4test.v1.AppConfig

api_url = "hello world"
`
	docURI := uri.URI("file:///work/c.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, src)))

	// Cursor inside the string literal but NOT inside a ${...}.
	// The field-ref completion mode should NOT fire; the standard
	// flow may return enum / field completions but not full dotted
	// paths.
	list := completionAt(t, srv, docURI, 2, 16) // inside "hello"
	if list == nil {
		return // no completion is acceptable
	}
	// db.host is a path-form only the field-ref mode emits; its
	// absence confirms the mode didn't trigger.
	assert.NotContains(t, labelsOf(list), "db.host",
		"cursor outside ${...} must not trigger field-path completion")
}

func TestFieldRefCompletion_AfterClosingBraceDoesNotTrigger(t *testing.T) {
	srv, _ := newAppConfigServer(t)
	const src = `@type c4test.v1.AppConfig

api_url = "${db.host}!"
`
	docURI := uri.URI("file:///work/c.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, src)))

	// Cursor right after `}` — context is closed. Index map of
	// `api_url = "${db.host}!"`: `}` is at character 20; cursor
	// just after it is character 21 (between `}` and `!`).
	list := completionAt(t, srv, docURI, 2, 21)
	if list != nil {
		assert.NotContains(t, labelsOf(list), "db.host",
			"after `}` the ${...} context is closed; no path completion")
	}
}

func TestFieldRefCompletion_TextEditReplacesPrefix(t *testing.T) {
	srv, _ := newAppConfigServer(t)
	// User typed `${db.h` and asks for completion.
	const src = `@type c4test.v1.AppConfig

api_url = "${db.h"
`
	docURI := uri.URI("file:///work/c.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, src)))

	// Cursor between `h` and the closing `"`. Index map of
	// `api_url = "${db.h"`: `h` is at character 16; `"` at 17;
	// cursor between them is character 17. Content of `${...}`
	// starts at character 13 (the `d`).
	list := completionAt(t, srv, docURI, 2, 17)
	require.NotNil(t, list)
	require.NotEmpty(t, list.Items)

	// Every item must carry a TextEdit whose range covers the
	// user's typed prefix `db.h` so the client doesn't compound
	// it on insertion.
	for _, it := range list.Items {
		require.NotNil(t, it.TextEdit,
			"each item must specify a TextEdit replacement range")
		assert.Equal(t, uint32(2), it.TextEdit.Range.Start.Line)
		assert.Equal(t, uint32(2), it.TextEdit.Range.End.Line)
		assert.Equal(t, uint32(13), it.TextEdit.Range.Start.Character,
			"replacement starts at the first byte after `${`")
		assert.Equal(t, uint32(17), it.TextEdit.Range.End.Character,
			"replacement ends at the cursor")
		assert.Equal(t, it.Label, it.TextEdit.NewText,
			"NewText equals the path label; client filters by label")
	}
}

func TestFieldRefCompletion_NoLookupOrTypeSkips(t *testing.T) {
	// No lookup → no field-ref completion (no descriptor to walk).
	srv, _ := newServerWithFakeConn(t)
	const src = `@type c4test.v1.AppConfig

api_url = "${"
`
	docURI := uri.URI("file:///work/c.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, src)))

	list := completionAt(t, srv, docURI, 2, 13)
	assert.Nil(t, list,
		"no schema bound → no completion (any kind)")
}

// ---------------------------------------------------------------------------
// Unit-level: FieldRefContextAtCursor + EnumerateFieldPaths
// ---------------------------------------------------------------------------
