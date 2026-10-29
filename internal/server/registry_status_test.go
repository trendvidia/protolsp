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

func registryStatusReply(t *testing.T, srv *Server) RegistryStatusResult {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodRegistryStatus, nil)
	require.NoError(t, err)

	var got RegistryStatusResult
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

func TestRegistryStatus_LiveResolverReportsConfiguredNotStale(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)

	got := registryStatusReply(t, srv)
	assert.True(t, got.Configured)
	assert.False(t, got.Stale)
}

func TestRegistryStatus_NoLookupReportsNotConfigured(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	// No s.lookup set.

	got := registryStatusReply(t, srv)
	assert.False(t, got.Configured, "no lookup → configured=false")
	assert.False(t, got.Stale)
}

func TestRegistryStatus_StaleLookupReportsStale(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.staleFlag = true
	srv.Lookups().Swap(lookup, nil)

	got := registryStatusReply(t, srv)
	assert.True(t, got.Configured)
	assert.True(t, got.Stale, "fakeLookup with staleFlag must surface as stale=true")
}

// TestInitialized_StaleSchemaSurfacesShowMessage exercises the
// startup nudge: a stale-loaded resolver triggers a
// window/showMessage warning when the client sends `initialized`.
func TestInitialized_StaleSchemaSurfacesShowMessage(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.staleFlag = true
	srv.Lookups().Swap(lookup, nil)

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))
	srv.WaitInitialized()

	msgs := fc.shownMessages()
	require.Len(t, msgs, 1, "stale lookup must trigger exactly one showMessage at initialized")
	assert.Equal(t, protocol.MessageTypeWarning, msgs[0].Type)
	assert.Contains(t, msgs[0].Message, "disk cache")
}

func TestInitialized_LiveSchemaIsSilent(t *testing.T) {
	srv, fc, _ := newServerWithLookup(t)

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))
	srv.WaitInitialized()

	assert.Empty(t, fc.shownMessages(),
		"live (non-stale) lookup must not pop up the stale-schema warning")
}

func TestInitialized_NoLookupIsSilent(t *testing.T) {
	// Editor running without a registry config — no notification.
	srv, fc := newServerWithFakeConn(t)

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))
	srv.WaitInitialized()

	assert.Empty(t, fc.shownMessages())
}
