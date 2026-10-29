// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// TestEffectiveConfigSubscribe_DeltasOnOverrideChange exercises the
// EffectiveConfig.Subscribe stream end-to-end: subscribe, trigger a
// config change via MethodSetOverrides, receive the delta event.
//
// Differs from Secrets/Datasets: there is no initial snapshot
// message. Callers do a separate GetEffectiveConfig for the
// snapshot, then keep Subscribe open for deltas (this is the
// behavior promised by the rpc_effective_config_subscribe.go
// stream contract).
func TestEffectiveConfigSubscribe_DeltasOnOverrideChange(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	const workspaceDir = "/work"

	client := rpcpbconnect.NewEffectiveConfigServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type subResult struct {
		stream *connect.ServerStreamForClient[rpcpb.EffectiveConfigServiceSubscribeResponse]
		err    error
	}
	subCh := make(chan subResult, 1)
	go func() {
		stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.EffectiveConfigServiceSubscribeRequest{
			WorkspaceDir: workspaceDir,
		}))
		subCh <- subResult{stream, err}
	}()

	// Wait for the handler to register the subscriber before we
	// publish. The Subscribe RPC may still be blocked on its first
	// response — that's OK; the subscriber registers as soon as the
	// handler enters, and the first event we publish will flush the
	// initial headers and unblock the client.
	settleSubscribers(t, srv, workspaceDir)

	// Trigger an override change via the LSP method. SetOverrides
	// calls NotifyEffectiveConfigChanged which fans out to RPC
	// subscribers.
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodSetOverrides, SetOverridesParams{
		WorkspaceDir: workspaceDir,
		Overrides: map[string]any{
			"protolsp.registry.address": "override.example:443",
		},
	})
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))

	var stream *connect.ServerStreamForClient[rpcpb.EffectiveConfigServiceSubscribeResponse]
	select {
	case r := <-subCh:
		require.NoError(t, r.err)
		stream = r.stream
	case <-time.After(3 * time.Second):
		t.Fatalf("Subscribe never returned — handler may not be flushing headers")
	}
	defer func() { _ = stream.Close() }()

	require.True(t, stream.Receive(),
		"override change should produce a delta event; stream.Err()=%v", stream.Err())
	ev := stream.Msg()
	require.NotNil(t, ev)
	assert.Equal(t, workspaceDir, ev.WorkspaceDir)
	require.Contains(t, ev.Changes, "protolsp.registry.address",
		"changes map carries the section that shifted")
	got := ev.Changes["protolsp.registry.address"]
	require.NotNil(t, got)
	require.NotNil(t, got.Value)
	sv, ok := got.Value.GetValue().(*rpcpb.ConfigSectionValue_StringValue)
	require.True(t, ok, "registry.address is a string section")
	assert.Equal(t, "override.example:443", sv.StringValue)
	assert.Equal(t, rpcpb.ConfigSource_CONFIG_SOURCE_OVERRIDE, got.Source,
		"override tier produces Source=OVERRIDE in the published delta")
}

// TestEffectiveConfigSubscribe_RedactsSecretSections confirms the
// publish path redacts secret-sensitive sections to match the LSP
// notification's on-the-wire shape. Currently the registry.token
// section is the only one in KnownSections that hits the redact
// branch.
func TestEffectiveConfigSubscribe_RedactsSecretSections(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	const workspaceDir = "/work"

	client := rpcpbconnect.NewEffectiveConfigServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type subResult struct {
		stream *connect.ServerStreamForClient[rpcpb.EffectiveConfigServiceSubscribeResponse]
		err    error
	}
	subCh := make(chan subResult, 1)
	go func() {
		stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.EffectiveConfigServiceSubscribeRequest{
			WorkspaceDir: workspaceDir,
		}))
		subCh <- subResult{stream, err}
	}()

	settleSubscribers(t, srv, workspaceDir)

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodSetOverrides, SetOverridesParams{
		WorkspaceDir: workspaceDir,
		Overrides: map[string]any{
			"protolsp.registry.token": "real-secret-token-xyz",
		},
	})
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))

	var stream *connect.ServerStreamForClient[rpcpb.EffectiveConfigServiceSubscribeResponse]
	select {
	case r := <-subCh:
		require.NoError(t, r.err)
		stream = r.stream
	case <-time.After(3 * time.Second):
		t.Fatalf("Subscribe never returned")
	}
	defer func() { _ = stream.Close() }()

	require.True(t, stream.Receive(), "stream.Err()=%v", stream.Err())
	ev := stream.Msg()
	require.NotNil(t, ev)
	got, ok := ev.Changes["protolsp.registry.token"]
	require.True(t, ok, "token section is in the delta map")
	require.NotNil(t, got)
	sv, ok := got.Value.GetValue().(*rpcpb.ConfigSectionValue_StringValue)
	require.True(t, ok)
	assert.NotEqual(t, "real-secret-token-xyz", sv.StringValue,
		"token value must not leak verbatim onto the wire")
}

// settleSubscribers blocks until at least one subscriber is
// registered against the given workspaceDir. Connect-RPC's server
// streaming may withhold response headers (and therefore block the
// client's Subscribe call) until the first Send fires, so we
// observe the server-side subscriber state directly. Fails the
// test if the subscriber doesn't appear within a second — that
// would mean the handler never ran, which is a real bug.
func settleSubscribers(t *testing.T, srv *Server, workspaceDir string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		subs := srv.effectiveConfigSubs()
		subs.mu.Lock()
		_, ok := subs.all[workspaceDir]
		subs.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscriber for %q never registered", workspaceDir)
}
