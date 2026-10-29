// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// TestLSPRPC_ConsistencyAcrossSurfaces is the audit's "LSP↔RPC
// consistency" recommendation: one test brings up a fully-loaded
// workspace and asserts every doubled surface (ChainStatus, LayerInfo,
// SecretRanges, DatasetRegions, EffectiveConfig) returns the same
// logical state via the Connect-RPC sidecar that it returns via the
// LSP custom request. Catches marshalling drift between the two code
// paths that share underlying state.
//
// Single test, multiple subtests so failures pinpoint the specific
// surface that drifted.
func TestLSPRPC_ConsistencyAcrossSurfaces(t *testing.T) {
	// 1. Workspace with chain + .protolsp.pxf — exercises chameleon.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(chainStatusSimpleSpec), 0o600))
	const wsConfig = `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "chain.pxf"
  schema_fqn = "myapp.v1.AppConfig"
}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(wsConfig), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.rpc, "initialize must spin up the RPC sidecar")

	// 2. Add a lookup carrying both Credentials (secrets) and Trade
	// (dataset) so the doc-scoped surfaces have something to surface.
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	// 3. Open one doc per surface so each consistency check has
	// per-doc state to compare.
	const secretURI = "file:///work/c.pxf"
	const datasetURI = "file:///work/t.pxf"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, secretURI, 1, secretsScalarSrc)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, datasetURI, 1, tradeDatasetSrc)))

	authed := connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token))
	addr := srv.rpc.address
	ctx := context.Background()

	t.Run("ChainStatus", func(t *testing.T) {
		lspGot := chainStatusReply(t, srv)
		c := rpcpbconnect.NewChameleonServiceClient(http.DefaultClient, addr, authed)
		rpcResp, err := c.GetChainStatus(ctx, connect.NewRequest(&rpcpb.GetChainStatusRequest{}))
		require.NoError(t, err)
		rpcGot := rpcResp.Msg

		assert.Equal(t, lspGot.Configured, rpcGot.Configured)
		assert.Equal(t, lspGot.ChainSpecPath, rpcGot.ChainSpecPath)
		assert.Equal(t, lspGot.Version, rpcGot.Version)
		assert.Equal(t, lspGot.SchemaFQN, rpcGot.SchemaFqn)
		assert.Equal(t, lspGot.DefaultEnvironment, rpcGot.DefaultEnvironment)
		assert.Equal(t, lspGot.ActiveEnvironment, rpcGot.ActiveEnvironment)
		require.Equal(t, len(lspGot.Environments), len(rpcGot.Environments),
			"environment count must match across surfaces")
		for i := range lspGot.Environments {
			assert.Equal(t, lspGot.Environments[i].Name, rpcGot.Environments[i].Name,
				"environment[%d].Name", i)
			assert.Equal(t, lspGot.Environments[i].Ancestors, rpcGot.Environments[i].Ancestors,
				"environment[%d].Ancestors", i)
			assert.Equal(t, lspGot.Environments[i].KeyName, rpcGot.Environments[i].KeyName,
				"environment[%d].KeyName", i)
		}
	})

	t.Run("LayerInfo", func(t *testing.T) {
		lspGot := layerInfoReply(t, srv, secretURI)
		c := rpcpbconnect.NewChameleonServiceClient(http.DefaultClient, addr, authed)
		rpcResp, err := c.GetLayerInfo(ctx, connect.NewRequest(&rpcpb.GetLayerInfoRequest{
			TextDocument: &rpcpb.TextDocumentIdentifier{Uri: string(secretURI)},
		}))
		require.NoError(t, err)
		rpcGot := rpcResp.Msg

		assert.Equal(t, lspGot.IsLayer, rpcGot.IsLayer)
		assert.Equal(t, lspGot.Environment, rpcGot.Environment)
		assert.Equal(t, lspGot.Ancestors, rpcGot.Ancestors)
	})

	t.Run("SecretRanges", func(t *testing.T) {
		lspGot := secretRangesReply(t, srv, secretURI)
		c := rpcpbconnect.NewSecretsServiceClient(http.DefaultClient, addr, authed)
		rpcResp, err := c.GetRanges(ctx, connect.NewRequest(&rpcpb.SecretsServiceGetRangesRequest{
			TextDocument: &rpcpb.TextDocumentIdentifier{Uri: secretURI},
		}))
		require.NoError(t, err)
		rpcGot := rpcResp.Msg

		require.Equal(t, len(lspGot.Ranges), len(rpcGot.Ranges),
			"secret range count must match")
		for i := range lspGot.Ranges {
			lspR := lspGot.Ranges[i]
			rpcR := rpcGot.Ranges[i]
			assert.Equal(t, lspR.FieldName, rpcR.FieldName, "ranges[%d].FieldName", i)
			// LSP wire shape uses "scalar" / "block" strings; the
			// proto enum mirrors them.
			wantEnum := rpcpb.SecretForm_SECRET_FORM_SCALAR
			if lspR.Form == "block" {
				wantEnum = rpcpb.SecretForm_SECRET_FORM_BLOCK
			}
			assert.Equal(t, wantEnum, rpcR.Form, "ranges[%d].Form", i)
		}
	})

	t.Run("DatasetRegions", func(t *testing.T) {
		lspGot := datasetRegionsReply(t, srv, datasetURI)
		c := rpcpbconnect.NewDatasetsServiceClient(http.DefaultClient, addr, authed)
		rpcResp, err := c.GetRegions(ctx, connect.NewRequest(&rpcpb.DatasetsServiceGetRegionsRequest{
			TextDocument: &rpcpb.TextDocumentIdentifier{Uri: datasetURI},
		}))
		require.NoError(t, err)
		rpcGot := rpcResp.Msg

		require.Equal(t, len(lspGot.Regions), len(rpcGot.Regions),
			"dataset region count must match")
		for i := range lspGot.Regions {
			lspR := lspGot.Regions[i]
			rpcR := rpcGot.Regions[i]
			assert.Equal(t, lspR.TypeFQN, rpcR.TypeFqn, "regions[%d].TypeFQN", i)
			assert.Equal(t, int32(lspR.RowCount), rpcR.RowCount, "regions[%d].RowCount", i)
			assert.Equal(t, lspR.Resolvable, rpcR.Resolvable, "regions[%d].Resolvable", i)
			assert.Equal(t, lspR.Huge, rpcR.Huge, "regions[%d].Huge", i)
		}
	})

	t.Run("EffectiveConfig", func(t *testing.T) {
		lspGot := effectiveConfigReply(t, srv, GetEffectiveConfigParams{})
		c := rpcpbconnect.NewEffectiveConfigServiceClient(http.DefaultClient, addr, authed)
		rpcResp, err := c.GetEffectiveConfig(ctx, connect.NewRequest(&rpcpb.GetEffectiveConfigRequest{}))
		require.NoError(t, err)
		rpcGot := rpcResp.Msg

		require.Equal(t, len(lspGot.Values), len(rpcGot.Values),
			"effective-config section count must match")
		for section, lspVal := range lspGot.Values {
			rpcVal, ok := rpcGot.Values[section]
			require.True(t, ok, "section %q present on LSP but missing on RPC", section)
			// Source mapping is exhaustive — every LSP source has a
			// proto enum equivalent (validated by configSourceToProto).
			assert.Equal(t, string(lspVal.Source), simpleConfigSourceName(rpcVal.Source),
				"section %q Source", section)
		}
	})
}

// simpleConfigSourceName collapses the rpcpb ConfigSource enum back
// to the LSP-side ConfigSource string for direct comparison. Kept
// inline in the test rather than reusing internal helpers to avoid
// the test passing trivially when the underlying mapping drifts.
func simpleConfigSourceName(s rpcpb.ConfigSource) string {
	switch s {
	case rpcpb.ConfigSource_CONFIG_SOURCE_DEFAULT:
		return "default"
	case rpcpb.ConfigSource_CONFIG_SOURCE_FILE:
		return "file"
	case rpcpb.ConfigSource_CONFIG_SOURCE_SETTINGS:
		return "settings"
	case rpcpb.ConfigSource_CONFIG_SOURCE_OVERRIDE:
		return "override"
	}
	return ""
}
