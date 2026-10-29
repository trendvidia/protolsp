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
)

const secretsScalarSrc = `@type acme.demo.v1.Credentials

username = "alice"
password = "super-secret"
`

const secretsBlockSrc = `@type acme.demo.v1.Credentials

username = "alice"
password {
  value = "block-secret"
  hint = "AWS key"
}
`

// secretRangesReply drives MethodSecretRanges and decodes the
// result.
func secretRangesReply(t *testing.T, srv *Server, docURI protocol.DocumentURI) SecretRangesResult {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodSecretRanges, SecretRangesParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
	})
	require.NoError(t, err)

	var got SecretRangesResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &got))
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return got
}

// newServerWithCredentialsType is the secret-test counterpart to
// newServerWithLookup: registers the Credentials message type that
// has a pxf.Secret-typed field. Returns the server, fakeConn, and
// fakeLookup so individual tests can drive each.
func newServerWithCredentialsType(t *testing.T) (*Server, *fakeConn, *fakeLookup) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	return srv, fc, lookup
}

func TestSecretRanges_ReturnsScalarFormEntry(t *testing.T) {
	srv, _, _ := newServerWithCredentialsType(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	got := secretRangesReply(t, srv, "file:///work/c.pxf")
	require.Len(t, got.Ranges, 1)
	assert.Equal(t, "password", got.Ranges[0].FieldName)
	assert.Equal(t, "scalar", got.Ranges[0].Form,
		"scalar shorthand surfaces with Form=\"scalar\"")
}

func TestSecretRanges_ReturnsBlockFormEntry(t *testing.T) {
	srv, _, _ := newServerWithCredentialsType(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsBlockSrc)))

	got := secretRangesReply(t, srv, "file:///work/c.pxf")
	require.Len(t, got.Ranges, 1)
	assert.Equal(t, "password", got.Ranges[0].FieldName)
	assert.Equal(t, "block", got.Ranges[0].Form,
		"block form surfaces with Form=\"block\"")
}

func TestSecretRanges_NoDocReturnsEmpty(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	got := secretRangesReply(t, srv, "file:///nope.pxf")
	assert.NotNil(t, got.Ranges, "no document → empty array, not null")
	assert.Empty(t, got.Ranges)
}

func TestSecretRanges_DocumentWithoutSecretsReturnsEmpty(t *testing.T) {
	// Greeting fixture has no pxf.Secret fields; the document
	// loads, has no secret ranges.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	got := secretRangesReply(t, srv, "file:///work/g.pxf")
	assert.NotNil(t, got.Ranges)
	assert.Empty(t, got.Ranges,
		"document with no pxf.Secret fields → empty ranges")
}

func TestSecretRangesChanged_FiresOnDidOpenWithSecrets(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)
	// Opt in to legacy-LSP-client mode so the notification gate
	// added in #113 doesn't suppress the notification we're
	// asserting fired. Real clients flip this bit by calling the
	// matching legacy request method; tests flip it directly.
	srv.legacyLspClients.secretRanges.Store(true)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.secretRangesChangedNotifications()
	require.Len(t, notes, 1,
		"didOpen on a document with secrets → one secretRangesChanged notification")
	assert.Equal(t, int32(1), notes[0].Version)
	require.Len(t, notes[0].Ranges, 1)
	assert.Equal(t, "password", notes[0].Ranges[0].FieldName)
}

func TestSecretRangesChanged_SilentForDocumentWithoutSecrets(t *testing.T) {
	// Open a non-secret doc — no notification fires because
	// previous (nil) and current (nil) snapshots match.
	srv, fc, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	assert.Empty(t, fc.secretRangesChangedNotifications(),
		"opening a non-secret document fires no secretRangesChanged notification")
}

// enableLegacySecretRanges flips the legacy-client gate so tests
// that pre-date #113 can exercise the LSP notification path
// without the publish-gate suppressing it.
func enableLegacySecretRanges(srv *Server) {
	srv.legacyLspClients.secretRanges.Store(true)
}

// TestSecretRangesChanged_GatedUntilLegacyDispatchCalled is the
// counterpart to TestSecretRangesChanged_FiresOnDidOpenWithSecrets:
// without calling the legacy protolsp/secretRanges request, no
// notification fires even when the secret-range table changes.
// The RPC publish path still fires (covered by
// rpc_secrets_subscribe_test.go); this test pins the LSP-side gate
// added in #113.
func TestSecretRangesChanged_GatedUntilLegacyDispatchCalled(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))
	assert.Empty(t, fc.secretRangesChangedNotifications(),
		"no notification fires until a legacy request is invoked — this is the #113 gate")

	// After the legacy request fires once, subsequent edits do
	// emit the notification — the client has self-identified as
	// legacy and the gate stays open for the rest of the session.
	_ = secretRangesReply(t, srv, "file:///work/c.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/c.pxf", 2,
			"@type acme.demo.v1.Credentials\n\nusername = \"bob\"\npassword = \"hunter2\"\n")))
	assert.NotEmpty(t, fc.secretRangesChangedNotifications(),
		"after a legacy request the notification flows on subsequent edits")
}

func TestSecretRangesChanged_FiresOnDidChange_NewSecretAdded(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)
	enableLegacySecretRanges(srv)

	// Open WITHOUT a password line, then add one via didChange.
	const beforeSrc = `@type acme.demo.v1.Credentials

username = "alice"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, beforeSrc)))
	// No secret in the initial document → no notification yet.
	require.Empty(t, fc.secretRangesChangedNotifications())

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/c.pxf", 2, secretsScalarSrc)))

	notes := fc.secretRangesChangedNotifications()
	require.Len(t, notes, 1,
		"didChange that introduces a secret fires one notification")
	assert.Equal(t, int32(2), notes[0].Version,
		"notification carries the post-edit document version")
	require.Len(t, notes[0].Ranges, 1)
}

func TestSecretRangesChanged_SilentOnIdenticalReopen(t *testing.T) {
	// didChange that doesn't shift the secret-range table is a
	// no-op for the notification. We achieve that by re-applying
	// the same source — same secret values + ranges.
	srv, fc, _ := newServerWithCredentialsType(t)
	enableLegacySecretRanges(srv)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))
	require.Len(t, fc.secretRangesChangedNotifications(), 1,
		"first open fires once")

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/c.pxf", 2, secretsScalarSrc)))

	assert.Len(t, fc.secretRangesChangedNotifications(), 1,
		"didChange with identical content adds no notification")
}

func TestSecretRanges_NilParamsTolerated(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodSecretRanges, nil)
	require.NoError(t, err)
	var got SecretRangesResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, _ := json.Marshal(result)
		_ = json.Unmarshal(b, &got)
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	assert.NotNil(t, got.Ranges, "null params → empty result, no error")
}
