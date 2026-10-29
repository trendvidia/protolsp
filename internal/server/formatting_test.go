// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// formattingFromHandle drives the formatting dispatcher and returns
// the decoded TextEdit slice (nil = "no edits available").
func formattingFromHandle(t *testing.T, srv *Server, u string) []protocol.TextEdit {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentFormatting,
		&protocol.DocumentFormattingParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
			Options:      protocol.FormattingOptions{InsertSpaces: true, TabSize: 2},
		})
	require.NoError(t, err)

	var captured []protocol.TextEdit
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

func TestFormatting_AlreadyCanonicalReturnsEmptySlice(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	edits := formattingFromHandle(t, srv, "file:///work/g.pxf")
	require.NotNil(t, edits, "canonical input → non-nil slice (zero edits)")
	assert.Empty(t, edits, "canonical input should produce no edits")
}

func TestFormatting_RewritesNonCanonicalSource(t *testing.T) {
	// Excessive blank lines + non-standard spacing around `=` —
	// protowire-go's FormatDocument re-emits canonical layout.
	const messy = "@type acme.demo.v1.Greeting\n\n\n\nname    =     \"world\"\n"
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/m.pxf", 1, messy)))

	edits := formattingFromHandle(t, srv, "file:///work/m.pxf")
	require.Len(t, edits, 1, "non-canonical input should produce one whole-document edit")
	assert.Equal(t, uint32(0), edits[0].Range.Start.Line)
	assert.Equal(t, uint32(0), edits[0].Range.Start.Character)
	assert.NotEqual(t, messy, edits[0].NewText, "edit should rewrite the source")
	// Sanity-check the canonical form: single space around `=`, no
	// run of blank lines between @type and the first field.
	assert.Contains(t, edits[0].NewText, `name = "world"`)
	assert.NotContains(t, edits[0].NewText, "\n\n\n\n")
}

func TestFormatting_ReturnsNilWhenDocumentNotOpen(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	edits := formattingFromHandle(t, srv, "file:///work/missing.pxf")
	assert.Nil(t, edits)
}

func TestFormatting_ReturnsNilOnParseError(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	// Parse error: assignment with no key.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/bad.pxf", 1, "= 1\n")))

	edits := formattingFromHandle(t, srv, "file:///work/bad.pxf")
	assert.Nil(t, edits, "broken parse → no edits (don't clobber mid-edit buffer)")
}

// ---------------------------------------------------------------------------
// Proto formatting (phase 5a) — driven through buf via the buftool package
// ---------------------------------------------------------------------------

// writeFakeBufForServer stages a shell script that the buftool
// invocations route to via PROTOLSP_BUF_PATH. Branches on the
// BUFTOOL_TEST_MODE env var to let individual tests dial behavior:
//
//	"format-uppercase" — uppercase the input
//	"format-passthrough" — return the input unchanged (already canonical)
//	"format-fail" — exit 1 with a stderr message
//
// Mirrors the helper in buftool_test.go but lives here too so the
// server integration tests don't have to import an internal _test
// package.
func writeFakeBufForServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-buf.sh")
	const script = `#!/bin/sh
if [ "$1" != "format" ]; then
  exit 2
fi
file="$2"
case "${BUFTOOL_TEST_MODE:-format-passthrough}" in
  format-uppercase) tr '[:lower:]' '[:upper:]' < "$file" ;;
  format-passthrough) cat "$file" ;;
  format-fail) echo "fake-buf: simulated failure" 1>&2; exit 1 ;;
  *) exit 3 ;;
esac
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestProtoFormatting_BufRewrites(t *testing.T) {
	t.Setenv("PROTOLSP_BUF_PATH", writeFakeBufForServer(t))
	t.Setenv("BUFTOOL_TEST_MODE", "format-uppercase")

	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/foo.proto"
	const src = `syntax = "proto3";

message Foo {
  string name = 1;
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))

	edits := formattingFromHandle(t, srv, u)
	require.Len(t, edits, 1, "non-canonical source produces a single whole-doc edit")
	assert.Equal(t, strings.ToUpper(src), edits[0].NewText)
	// Range covers the whole document — start at (0,0), end at the
	// past-the-last position.
	assert.Equal(t, uint32(0), edits[0].Range.Start.Line)
	assert.Equal(t, uint32(0), edits[0].Range.Start.Character)
}

func TestProtoFormatting_AlreadyCanonicalReturnsEmptySlice(t *testing.T) {
	t.Setenv("PROTOLSP_BUF_PATH", writeFakeBufForServer(t))
	t.Setenv("BUFTOOL_TEST_MODE", "format-passthrough")

	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/foo.proto"
	const src = "syntax = \"proto3\";\n"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))

	edits := formattingFromHandle(t, srv, u)
	require.NotNil(t, edits,
		"canonical source returns an empty slice, not nil (signals formatter ran, no edits)")
	assert.Empty(t, edits)
}

func TestProtoFormatting_NilWhenBufMissing(t *testing.T) {
	// Point env var at a non-existent path. buftool surfaces this
	// as ErrBufNotFound; the server returns nil so the client
	// sees "formatter unavailable" rather than "format failed".
	t.Setenv("PROTOLSP_BUF_PATH", "/nonexistent/buf-binary")
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/foo.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, "syntax = \"proto3\";\n")))

	edits := formattingFromHandle(t, srv, u)
	assert.Nil(t, edits)
}

func TestProtoFormatting_NilWhenBufFails(t *testing.T) {
	// Buf invocation itself errors (e.g. unparseable mid-edit
	// source). Same nil-result posture as missing-binary so the
	// client doesn't render a stale partial edit.
	t.Setenv("PROTOLSP_BUF_PATH", writeFakeBufForServer(t))
	t.Setenv("BUFTOOL_TEST_MODE", "format-fail")

	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/foo.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, "anything\n")))

	edits := formattingFromHandle(t, srv, u)
	assert.Nil(t, edits)
}

// ---------------------------------------------------------------------------

func TestFormatting_AdvertisesCapability(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
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
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	assert.Equal(t, true, caps["documentFormattingProvider"])
}
