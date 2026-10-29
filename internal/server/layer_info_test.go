// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const layerInfoChainSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "base" path = "base.pxf"              for_each = ONCE    required = true }
  { id = "env"  path = "env/${env}.pxf"        for_each = PER_ENV }
  { id = "sec"  path = "secrets/${env}.sops.pxf" for_each = PER_ENV encrypted = true source = "secrets" }
]
`

// layerInfoReply drives MethodLayerInfo for the given doc URI and
// decodes the result.
func layerInfoReply(t *testing.T, srv *Server, docURI uri.URI) LayerInfoResult {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodLayerInfo,
		LayerInfoParams{TextDocument: protocol.TextDocumentIdentifier{URI: docURI}})
	require.NoError(t, err)

	var got LayerInfoResult
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

// initializeWithChain bootstraps a workspace whose .protolsp.pxf
// points at a chain.pxf containing the standard layerInfoChainSpec.
// Returns the server and the workspace dir (callers compute the
// file URI for the doc they want to test).
func initializeWithChain(t *testing.T) (*Server, string) {
	t.Helper()
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(layerInfoChainSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(),
		"chain must load — preconditions for layer-info tests")
	return srv, workspaceDir
}

func TestLayerInfo_PerEnvLayerMatchesEnvironment(t *testing.T) {
	srv, ws := initializeWithChain(t)
	docURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "@type myapp.v1.AppConfig\nname = \"x\"\n")))

	got := layerInfoReply(t, srv, docURI)
	assert.True(t, got.IsLayer, "env/dev.pxf is a known PER_ENV layer")
	assert.Equal(t, "dev", got.Environment)
	assert.Equal(t, "PER_ENV", got.Strategy)
	assert.Equal(t, []string{"prod", "sandbox", "dev"}, got.Ancestors,
		"PER_ENV layer reports root-first env ancestry")
	assert.False(t, got.Encrypted)
}

func TestLayerInfo_OnceLayerHasNoEnvironmentOrAncestors(t *testing.T) {
	srv, ws := initializeWithChain(t)
	docURI := uri.File(filepath.Join(ws, "base.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "name = \"base\"\n")))

	got := layerInfoReply(t, srv, docURI)
	assert.True(t, got.IsLayer)
	assert.Empty(t, got.Environment, "ONCE layers have no env binding")
	assert.Empty(t, got.Ancestors, "ONCE layers have no ancestry to surface")
	assert.Equal(t, "ONCE", got.Strategy)
}

func TestLayerInfo_EncryptedFlagSurfaces(t *testing.T) {
	srv, ws := initializeWithChain(t)
	// PER_ENV with ${env}=prod → "secrets/prod.sops.pxf"
	docURI := uri.File(filepath.Join(ws, "secrets", "prod.sops.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "")))

	got := layerInfoReply(t, srv, docURI)
	assert.True(t, got.IsLayer)
	assert.True(t, got.Encrypted,
		"layer declared `encrypted = true` reports it in layerInfo")
	assert.Equal(t, "secrets", got.Source)
	assert.Equal(t, "prod", got.Environment)
}

// TestLayerInfo_FsSourceRootFallback covers the customer workspace
// pattern where the chain's layer `path` templates don't combine
// cleanly with the source `root` (e.g. an extra path-prefix in the
// template), so the exact-match logic never hits. The permissive
// fallback should classify any .pxf under a declared fs source's
// root as a layer so schema-aware features still light up.
func TestLayerInfo_FsSourceRootFallback(t *testing.T) {
	workspaceDir := t.TempDir()
	// Chain whose layer paths don't actually resolve to the file on
	// disk — `secrets-dev.path = "secrets/${env}.pxf"` plus
	// `sources.secrets.root = "./layers/secrets"` would expand to
	// `layers/secrets/secrets/dev.pxf`, which doesn't exist. The file
	// under test lives at `layers/secrets/dev.pxf`.
	spec := `@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [ { name = "dev" } ]
sources = {
  "secrets": { kind = "fs" options = { "root": "./layers/secrets" } }
}
layers = [
  { id = "secrets-dev" path = "secrets/${env}.pxf" for_each = ONCE source = "secrets" }
]
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(spec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "preconditions: chain loaded")

	docURI := uri.File(filepath.Join(workspaceDir, "layers", "secrets", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "name = \"x\"\n")))

	got := layerInfoReply(t, srv, docURI)
	assert.True(t, got.IsLayer,
		"file under declared fs source root is a layer even when no expanded path matches exactly")
	assert.Equal(t, "secrets", got.Source,
		"fallback binds the source so source-scoped affordances (e.g. encryption) still apply")
}

// TestLayerInfo_FsSourceFallbackOnlyMatchesPxf guards against the
// fallback over-matching: a .proto file under a source root must
// still report IsLayer=false (it's a different file class entirely
// and shouldn't get chain-layer treatment).
func TestLayerInfo_FsSourceFallbackOnlyMatchesPxf(t *testing.T) {
	workspaceDir := t.TempDir()
	spec := `@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [ { name = "dev" } ]
sources = {
  "secrets": { kind = "fs" options = { "root": "./layers/secrets" } }
}
layers = [
  { id = "secrets-dev" path = "${env}.pxf" for_each = ONCE source = "secrets" }
]
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(spec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain())

	docURI := uri.File(filepath.Join(workspaceDir, "layers", "secrets", "schema.proto"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "syntax = \"proto3\";\n")))

	got := layerInfoReply(t, srv, docURI)
	assert.False(t, got.IsLayer,
		".proto file under an fs source root is not a chain layer")
}

func TestLayerInfo_NonLayerFileIsStandalone(t *testing.T) {
	srv, ws := initializeWithChain(t)
	// Random .pxf at the workspace root — not in any layer template.
	docURI := uri.File(filepath.Join(ws, "scratch.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "name = \"x\"\n")))

	got := layerInfoReply(t, srv, docURI)
	assert.False(t, got.IsLayer, "unmatched file is not a layer")
}

func TestLayerInfo_NoChainConfiguredYieldsIsLayerFalse(t *testing.T) {
	// No .protolsp.pxf at all → no chain.
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.Nil(t, srv.Workspaces().SnapshotChain(), "preconditions: no chain")

	docURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "")))
	got := layerInfoReply(t, srv, docURI)
	assert.False(t, got.IsLayer,
		"no chain means every file is standalone, regardless of name")
}

func TestLayerInfo_FileOutsideWorkspaceIsStandalone(t *testing.T) {
	srv, _ := initializeWithChain(t)
	// File in a totally unrelated directory.
	outside := filepath.Join(t.TempDir(), "elsewhere.pxf")
	docURI := uri.File(outside)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "")))

	got := layerInfoReply(t, srv, docURI)
	assert.False(t, got.IsLayer,
		"files outside the workspace root never match layers")
}

func TestLayerInfo_DocumentNotOpenStillReturnsSnapshot(t *testing.T) {
	// Editor may call layerInfo before didOpen fires — e.g. when
	// deciding whether to even open the file. Handler must still
	// answer rather than 404'ing.
	srv, ws := initializeWithChain(t)
	docURI := uri.File(filepath.Join(ws, "env", "sandbox.pxf"))
	// Note: no didOpen called.
	got := layerInfoReply(t, srv, docURI)
	assert.True(t, got.IsLayer,
		"layerInfo works on unopened documents — it's path-based")
	assert.Equal(t, "sandbox", got.Environment)
}

func TestLayerInfo_NullParamsTolerated(t *testing.T) {
	srv, _ := initializeWithChain(t)
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodLayerInfo, nil)
	require.NoError(t, err)
	var got LayerInfoResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &got))
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	assert.False(t, got.IsLayer,
		"null params (no URI) yields IsLayer=false rather than RPC error")
}

func TestLayerInfo_CachedOnDocumentAfterDidOpen(t *testing.T) {
	// DetectLayerInfo runs in dispatchDidOpen — the Document's
	// LayerInfo field should be populated before any layerInfo
	// request fires.
	srv, ws := initializeWithChain(t)
	docURI := uri.File(filepath.Join(ws, "env", "prod.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, "")))

	doc := srv.Documents().Get(docURI)
	require.NotNil(t, doc)
	require.NotNil(t, doc.LayerInfo,
		"didOpen must populate Document.LayerInfo")
	assert.True(t, doc.LayerInfo.IsLayer)
	assert.Equal(t, "prod", doc.LayerInfo.Environment)
}
