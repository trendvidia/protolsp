// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Error-shape consistency between the LSP custom-method surface
// and the parallel Connect-RPC sidecar. The success-side test
// (rpc_lsp_consistency_test.go) covers state agreement on
// happy inputs; this file covers the inverse — broken inputs
// must produce semantically-equivalent error responses on both
// surfaces. A drift here would silently let one surface accept
// an invalid request the other rejects, an asymmetry pxfed's
// switchable transport layer can't easily compensate for.

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// TestLSPRPC_ErrorShape_UnopenedDocReturnsEmpty pairs the
// doc-scoped surfaces (SecretRanges, DatasetRegions, LayerInfo)
// for a URI the server never saw. The contract being pinned:
// both LSP and RPC return an EMPTY (non-error) result — the
// editor's status-bar / sidebar code doesn't model failure for
// these pull queries; a "no such doc" arrives as an empty list.
// A regression where one surface starts returning an error
// would force every consuming client to add an error branch.
func TestLSPRPC_ErrorShape_UnopenedDocReturnsEmpty(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.rpc)

	authed := connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token))
	addr := srv.rpc.address
	ctx := context.Background()
	const missingURI = "file:///nope/never-opened.pxf"

	t.Run("SecretRanges", func(t *testing.T) {
		lspGot := secretRangesReply(t, srv, missingURI)
		assert.NotNil(t, lspGot.Ranges,
			"LSP: empty (non-nil) slice on unopened doc — LSP empty-array convention")
		assert.Empty(t, lspGot.Ranges)

		c := rpcpbconnect.NewSecretsServiceClient(http.DefaultClient, addr, authed)
		resp, err := c.GetRanges(ctx, connect.NewRequest(&rpcpb.SecretsServiceGetRangesRequest{
			TextDocument: &rpcpb.TextDocumentIdentifier{Uri: missingURI},
		}))
		require.NoError(t, err,
			"RPC: unopened doc must NOT surface as an error — both surfaces agree on empty")
		assert.Empty(t, resp.Msg.Ranges,
			"RPC: empty Ranges matches LSP empty Ranges")
	})

	t.Run("DatasetRegions", func(t *testing.T) {
		lspGot := datasetRegionsReply(t, srv, missingURI)
		assert.NotNil(t, lspGot.Regions)
		assert.Empty(t, lspGot.Regions)

		c := rpcpbconnect.NewDatasetsServiceClient(http.DefaultClient, addr, authed)
		resp, err := c.GetRegions(ctx, connect.NewRequest(&rpcpb.DatasetsServiceGetRegionsRequest{
			TextDocument: &rpcpb.TextDocumentIdentifier{Uri: missingURI},
		}))
		require.NoError(t, err)
		assert.Empty(t, resp.Msg.Regions)
	})

	t.Run("LayerInfo", func(t *testing.T) {
		// LayerInfo on an unopened doc returns the zero-value
		// LayerInfoResult — IsLayer=false, empty environment/
		// ancestors. The RPC parallel returns the same.
		lspGot := layerInfoReply(t, srv, missingURI)
		assert.False(t, lspGot.IsLayer)
		assert.Empty(t, lspGot.Environment)
		assert.Empty(t, lspGot.Ancestors)

		c := rpcpbconnect.NewChameleonServiceClient(http.DefaultClient, addr, authed)
		resp, err := c.GetLayerInfo(ctx, connect.NewRequest(&rpcpb.GetLayerInfoRequest{
			TextDocument: &rpcpb.TextDocumentIdentifier{Uri: missingURI},
		}))
		require.NoError(t, err)
		assert.False(t, resp.Msg.IsLayer)
		assert.Empty(t, resp.Msg.Environment)
		assert.Empty(t, resp.Msg.Ancestors)
	})
}

// TestLSPRPC_ErrorShape_UnknownOverrideSection_BothReject pairs
// the setOverrides paths for an unknown section name. The LSP
// dispatcher returns InvalidParams; the RPC handler returns
// CodeInvalidArgument. Both must include the offending section
// name in the error message so editors can surface the actual
// problem to the user — symmetric error shape.
func TestLSPRPC_ErrorShape_UnknownOverrideSection_BothReject(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.rpc)

	const badSection = "protolsp.does.not.exist"

	// LSP side: SetOverrides should reply with InvalidParams +
	// the offending section name.
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodSetOverrides, SetOverridesParams{
		Overrides: map[string]any{badSection: "anything"},
	})
	require.NoError(t, err)
	var lspErr error
	reply := func(_ context.Context, _ interface{}, replyErr error) error {
		lspErr = replyErr
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	require.Error(t, lspErr, "LSP: unknown section must surface an error")
	var jsonErr *jsonrpc2.Error
	require.ErrorAs(t, lspErr, &jsonErr,
		"LSP: error must be a jsonrpc2.Error so transports can route it")
	assert.Equal(t, jsonrpc2.InvalidParams, jsonErr.Code,
		"LSP: unknown section → InvalidParams")
	assert.Contains(t, jsonErr.Message, badSection,
		"LSP: error must name the offending section")

	// RPC side: same input, parallel error contract.
	client := rpcpbconnect.NewEffectiveConfigServiceClient(
		http.DefaultClient,
		srv.rpc.address,
		connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token)),
	)
	_, rpcErr := client.SetOverrides(context.Background(),
		connect.NewRequest(&rpcpb.SetOverridesRequest{
			Overrides: map[string]*rpcpb.ConfigSectionValue{
				badSection: {Value: &rpcpb.ConfigSectionValue_StringValue{StringValue: "anything"}},
			},
		}))
	require.Error(t, rpcErr, "RPC: unknown section must surface an error")
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(rpcErr),
		"RPC: unknown section → CodeInvalidArgument (the typed analogue of LSP InvalidParams)")
	assert.Contains(t, rpcErr.Error(), badSection,
		"RPC: error must name the offending section — matches LSP message contract")
}

// TestLSPRPC_ErrorShape_MalformedParamsRejection pins that the LSP
// dispatcher refuses a JSON-valid-but-shape-wrong request body
// (textDocument supplied as a number rather than an object). The
// RPC parallel runs at Connect-go's framing layer, which has its
// own well-covered rejection tests — what matters for the
// consistency contract is that the LSP side has a typed
// ParseError surface, not silent acceptance. A regression where
// dispatchSecretRanges started accepting malformed params would
// break the symmetry every editor relies on.
func TestLSPRPC_ErrorShape_MalformedParamsRejection(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	rawReq, err := newTestCall(jsonrpc2.NewNumberID(1), MethodSecretRanges,
		json.RawMessage(`{"textDocument": 42}`))
	require.NoError(t, err)
	var lspErr error
	reply := func(_ context.Context, _ interface{}, replyErr error) error {
		lspErr = replyErr
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, rawReq))
	require.Error(t, lspErr, "LSP: malformed params must reject, not crash")
	var jsonErr *jsonrpc2.Error
	require.ErrorAs(t, lspErr, &jsonErr)
	assert.Equal(t, jsonrpc2.ParseError, jsonErr.Code,
		"LSP: malformed body → ParseError per the spec")
	assert.Contains(t, jsonErr.Message, "SecretRangesParams",
		"LSP: error message should name the params type so clients can pinpoint the schema mismatch")
}
