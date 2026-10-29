// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// TestRPC_AdvertisedAndAuthenticated confirms the Connect-RPC sidecar
// boots during initialize, returns a usable address + token via
// serverInfo, and rejects unauthenticated requests with 401.
func TestRPC_AdvertisedAndAuthenticated(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	require.NotEmpty(t, endpoint.Address)
	require.NotEmpty(t, endpoint.Token)
	t.Cleanup(srv.stopRPC)

	// Unauthenticated probe → 401.
	resp, err := http.Get(endpoint.Address + "/" + rpcpbconnect.ConfigServiceName + "/ListAttributes")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"missing Authorization header → 401")
}

// TestRPC_ConfigService_RoundTrip exercises List/Get/Set against a
// real workspace + sidecar pair. Validates that the typed proto
// surface produces the same disk state as the LSP custom-method path.
func TestRPC_ConfigService_RoundTrip(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.rpc, "initialize must spin up the rpc sidecar")
	require.NotEmpty(t, srv.rpc.address)

	client := rpcpbconnect.NewConfigServiceClient(
		http.DefaultClient,
		srv.rpc.address,
		connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token)),
	)
	ctx := context.Background()

	// ListAttributes returns the registry-canonical set, non-empty.
	listResp, err := client.ListAttributes(ctx, connect.NewRequest(&rpcpb.ListAttributesRequest{}))
	require.NoError(t, err)
	require.NotEmpty(t, listResp.Msg.Attributes)

	// Pick a known string attribute. registry.address is reliable
	// across releases.
	const attrName = "registry.address"
	var found bool
	for _, a := range listResp.Msg.Attributes {
		if a.Name == attrName {
			found = true
			assert.Equal(t, rpcpb.AttributeKind_ATTRIBUTE_KIND_STRING, a.Kind,
				"registry.address is a string attribute")
		}
	}
	require.True(t, found, "%s must be in the attribute registry", attrName)

	// Initially absent.
	getResp, err := client.GetAttribute(ctx, connect.NewRequest(&rpcpb.GetAttributeRequest{
		Name: attrName,
	}))
	require.NoError(t, err)
	assert.False(t, getResp.Msg.Present, "fresh workspace → attribute not set")

	// Set, then Get should reflect the new value.
	_, err = client.SetAttribute(ctx, connect.NewRequest(&rpcpb.SetAttributeRequest{
		Name: attrName,
		Value: &rpcpb.AttributeValue{
			Value: &rpcpb.AttributeValue_StringValue{StringValue: "registry.example:443"},
		},
	}))
	require.NoError(t, err)

	getResp, err = client.GetAttribute(ctx, connect.NewRequest(&rpcpb.GetAttributeRequest{
		Name: attrName,
	}))
	require.NoError(t, err)
	require.True(t, getResp.Msg.Present, "after Set, attribute must be present")
	require.NotNil(t, getResp.Msg.Value)
	sv, ok := getResp.Msg.Value.GetValue().(*rpcpb.AttributeValue_StringValue)
	require.True(t, ok, "value carries the string oneof case")
	assert.Equal(t, "registry.example:443", sv.StringValue)
}

// TestRPC_ConfigService_UnknownAttribute pins the error contract for
// callers that pass a name not in the registry — InvalidArgument with
// the offending name in the message.
func TestRPC_ConfigService_UnknownAttribute(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	client := rpcpbconnect.NewConfigServiceClient(
		http.DefaultClient,
		srv.rpc.address,
		connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token)),
	)

	_, err := client.GetAttribute(context.Background(), connect.NewRequest(&rpcpb.GetAttributeRequest{
		Name: "no.such.attribute",
	}))
	require.Error(t, err)
	connectErr := new(connect.Error)
	require.ErrorAs(t, err, &connectErr)
	assert.Equal(t, connect.CodeInvalidArgument, connectErr.Code())
	assert.Contains(t, connectErr.Message(), "no.such.attribute")
}

// TestRPC_CORSPreflight pins the contract Electron renderers depend
// on: an OPTIONS preflight succeeds with permissive headers BEFORE
// bearer-auth runs, so the browser ever issues the real POST. The
// origin we send is arbitrary — Allow-Origin: * accepts anything as
// long as the request doesn't set credentials mode.
func TestRPC_CORSPreflight(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	req, err := http.NewRequest(http.MethodOptions,
		endpoint.Address+"/protolsp.v1.rpc.ConfigService/ListAttributes",
		nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,connect-protocol-version")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNoContent, resp.StatusCode,
		"preflight must succeed without bearer token — otherwise the real POST never fires")
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Authorization")
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Content-Type")
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Connect-Protocol-Version")
}

// TestRPC_RegistrySchemaChameleon_Wired smoke-tests the unary
// services added in Phase 5: each returns the same shape its
// LSP-side counterpart returns. Doesn't exhaustively re-cover the
// existing dispatch* tests — just confirms the Connect plumbing
// routes correctly and the field translation is right.
func TestRPC_RegistrySchemaChameleon_Wired(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.rpc)

	authed := func() connect.Option {
		return connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token))
	}
	ctx := context.Background()

	t.Run("registry GetStatus", func(t *testing.T) {
		c := rpcpbconnect.NewRegistryServiceClient(http.DefaultClient, srv.rpc.address, authed())
		resp, err := c.GetStatus(ctx, connect.NewRequest(&rpcpb.RegistryServiceGetStatusRequest{}))
		require.NoError(t, err)
		// No registry configured in this test workspace, so configured=false.
		assert.False(t, resp.Msg.Configured)
		assert.False(t, resp.Msg.Stale)
	})

	t.Run("schema ListMessages", func(t *testing.T) {
		c := rpcpbconnect.NewSchemaServiceClient(http.DefaultClient, srv.rpc.address, authed())
		resp, err := c.ListMessages(ctx, connect.NewRequest(&rpcpb.ListMessagesRequest{}))
		require.NoError(t, err)
		// No lookup → empty messages (intentional contract).
		assert.Empty(t, resp.Msg.Messages)
	})

	t.Run("chameleon GetChainStatus", func(t *testing.T) {
		c := rpcpbconnect.NewChameleonServiceClient(http.DefaultClient, srv.rpc.address, authed())
		resp, err := c.GetChainStatus(ctx, connect.NewRequest(&rpcpb.GetChainStatusRequest{}))
		require.NoError(t, err)
		assert.False(t, resp.Msg.Configured, "no chain spec in test workspace")
	})

	t.Run("effective config GetEffectiveConfig", func(t *testing.T) {
		c := rpcpbconnect.NewEffectiveConfigServiceClient(http.DefaultClient, srv.rpc.address, authed())
		resp, err := c.GetEffectiveConfig(ctx, connect.NewRequest(&rpcpb.GetEffectiveConfigRequest{}))
		require.NoError(t, err)
		// Returns at least the known sections (registry.* + security.inline_secrets).
		require.NotEmpty(t, resp.Msg.Values)
	})
}

// bearerClientInterceptor injects "Authorization: Bearer <token>" on
// every outgoing unary + stream client request. Mirrors the server-
// side check in bearerAuth.
//
// connect.UnaryInterceptorFunc only catches unary calls; streaming
// RPCs go through WrapStreamingClient. Implementing the full
// Interceptor surface here covers both — pxfed's renderer-side
// interceptor does the same in TypeScript via @connectrpc/connect-web.
func bearerClientInterceptor(token string) connect.Interceptor {
	return &bearerAuthInterceptor{token: token}
}

type bearerAuthInterceptor struct {
	token string
}

func (b *bearerAuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !strings.HasPrefix(req.Header().Get("Authorization"), "Bearer ") {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		return next(ctx, req)
	}
}

func (b *bearerAuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

func (b *bearerAuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
