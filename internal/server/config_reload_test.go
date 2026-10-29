// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// initializeWithChainForReloadTest is a small variant of the
// existing helpers that sets up a workspace + chain spec, like
// the C6/C7 tests use. Returns the server and workspace dir.
func initializeWithChainForReloadTest(t *testing.T, chainSpec string, layers map[string]string) (*Server, string) {
	t.Helper()
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(chainSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	for name, content := range layers {
		require.NoError(t, os.WriteFile(
			filepath.Join(workspaceDir, name), []byte(content), 0o600))
	}
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigWithNestedDb(t))
	srv.Lookups().Swap(lookup, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "preconditions: chain loaded")
	return srv, workspaceDir
}

func TestLiveReload_EditingChainPxfUpdatesChainState(t *testing.T) {
	// User opens chain.pxf and edits its default_environment. The
	// LSP's s.ws.rootChain.Spec.DefaultEnvironment reflects the edit
	// without requiring save.
	const initial = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = []
`
	srv, ws := initializeWithChainForReloadTest(t, initial, nil)
	require.Equal(t, "dev", srv.Workspaces().SnapshotChain().Spec.DefaultEnvironment)

	chainURI := uri.File(filepath.Join(ws, "chain.pxf"))
	// Open chain.pxf with a modified default_environment.
	const edited = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "prod"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = []
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(chainURI), 1, edited)))

	assert.Equal(t, "prod", srv.Workspaces().SnapshotChain().Spec.DefaultEnvironment,
		"didOpen on chain.pxf with edited content triggers a live reload")
}

func TestLiveReload_DidChangeOnChainPxfPropagatesToOtherDocs(t *testing.T) {
	// Open env/dev.pxf (a layer file under the chain).
	// Open chain.pxf and edit it via didChange to remove the env
	// layer entry. After the edit, env/dev.pxf's LayerInfo should
	// reflect that it's no longer a layer.
	const initialChain = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	srv, ws := initializeWithChainForReloadTest(t, initialChain, map[string]string{
		"env/dev.pxf": `@type c4test.v1.AppConfig` + "\n",
	})

	// Open env/dev.pxf — classified as a PER_ENV layer initially.
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig`+"\n")))
	devDoc := srv.Documents().Get(devURI)
	require.True(t, devDoc.LayerInfo.IsLayer,
		"preconditions: env/dev.pxf is a layer under the initial chain")

	// Open chain.pxf and remove the env layer entry via didChange.
	chainURI := uri.File(filepath.Join(ws, "chain.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(chainURI), 1, initialChain)))
	const edited = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = []
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, string(chainURI), 2, edited)))

	devDoc = srv.Documents().Get(devURI)
	require.NotNil(t, devDoc.LayerInfo)
	assert.False(t, devDoc.LayerInfo.IsLayer,
		"after live-reload removes the layer entry, env/dev.pxf classification flips to standalone")
}

func TestLiveReload_EditingProtolspPxfUpdatesFileConfig(t *testing.T) {
	// Empty workspace at first — open .protolsp.pxf with content
	// that introduces a chameleon block. The LSP's fileConfig
	// should pick it up via live-reload.
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.Nil(t, srv.Workspaces().SnapshotFileConfig(), "preconditions: no config at initialize")

	configURI := uri.File(filepath.Join(workspaceDir, ".protolsp.pxf"))
	const opened = `@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "from-buffer:443" }
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(configURI), 1, opened)))

	require.NotNil(t, srv.Workspaces().SnapshotFileConfig(),
		"didOpen on .protolsp.pxf populates fileConfig from the open-doc source")
	assert.Equal(t, "from-buffer:443", srv.Workspaces().SnapshotFileConfig().Registry.Address)
}

func TestLiveReload_DidChangeOnProtolspPxfReplacesFileConfig(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "from-disk:443" }
}
`), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.Workspaces().SnapshotFileConfig())
	require.Equal(t, "from-disk:443", srv.Workspaces().SnapshotFileConfig().Registry.Address)

	// Open the file (matches disk).
	configURI := uri.File(filepath.Join(workspaceDir, ".protolsp.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(configURI), 1, `@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "from-disk:443" }
}
`)))

	// Edit it via didChange — registry address changes in-memory.
	const editedBuffer = `@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "from-buffer:443" }
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, string(configURI), 2, editedBuffer)))

	assert.Equal(t, "from-buffer:443", srv.Workspaces().SnapshotFileConfig().Registry.Address,
		"didChange on .protolsp.pxf updates fileConfig from the open buffer")
}

func TestLiveReload_NonConfigDocDoesNotTriggerReload(t *testing.T) {
	// Editing a regular .pxf file (not config / not chain spec)
	// shouldn't churn fileConfig or chain state.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "stable:443" }
}
`), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	before := srv.Workspaces().SnapshotFileConfig()

	other := uri.File(filepath.Join(workspaceDir, "other.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(other), 1, `@type acme.demo.v1.Greeting
name = "x"
`)))

	assert.Same(t, before, srv.Workspaces().SnapshotFileConfig(),
		"editing a non-config doc must not retrigger fileConfig reload")
}

func TestLiveReload_DoesNotRecurse(t *testing.T) {
	// If the live-reload step inside RunAnalyzeAndMaybeLiveReload
	// erroneously called RunAnalyzeAndMaybeLiveReload again on
	// itself, analyzeCount would explode. Confirm it stays bounded.
	const initial = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	srv, ws := initializeWithChainForReloadTest(t, initial, map[string]string{
		"env/dev.pxf":  `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
	})
	// Open the chain spec + a layer file.
	chainURI := uri.File(filepath.Join(ws, "chain.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(chainURI), 1, initial)))
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig`+"\n")))

	before := srv.AnalyzeCount()
	// One edit on chain.pxf should produce: 1 analyze for chain.pxf
	// + 1 analyze per OTHER open doc (just env/dev.pxf). The other
	// open doc here is env/dev.pxf only — plus the chain.pxf
	// itself, but reanalyzeOpenDocsExcept skips it.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, string(chainURI), 2, initial))) // same content; still triggers reload

	delta := srv.AnalyzeCount() - before
	assert.LessOrEqual(t, delta, int64(3),
		"one edit on chain.pxf must produce a bounded number of analyze runs (no recursion)")
}

func TestLiveReload_ReanalyzeExceptSkipsTriggerDoc(t *testing.T) {
	// Direct unit-style check on the helper: after a config edit,
	// the reanalyze pass skips the trigger doc URI so we don't
	// double-analyze.
	const initial = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = []
`
	srv, ws := initializeWithChainForReloadTest(t, initial, map[string]string{
		"env/dev.pxf": `@type c4test.v1.AppConfig` + "\n",
	})

	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	chainURI := uri.File(filepath.Join(ws, "chain.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig`+"\n")))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(chainURI), 1, initial)))

	// Snapshot analyze count.
	before := srv.AnalyzeCount()
	srv.ReanalyzeOpenDocsExcept(context.Background(), chainURI)
	delta := srv.AnalyzeCount() - before
	// Should have re-analyzed env/dev.pxf (1) but NOT chain.pxf.
	assert.Equal(t, int64(1), delta,
		"reanalyzeOpenDocsExcept analyzes exactly the non-skipped docs")
}

func TestLiveReload_DetectionByTypeURL(t *testing.T) {
	// IsWorkspaceConfigDoc / isChainSpecDoc should route by @type
	// even when the URI doesn't match a conventional name.
	srv, _ := newServerWithFakeConn(t)

	const wsConfig = `@type protolsp.v1.WorkspaceConfig
security { inline_secrets = "warn" }
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/weird-name.pxf", 1, wsConfig)))
	doc := srv.Documents().Get(uri.URI("file:///work/weird-name.pxf"))
	require.NotNil(t, doc)
	assert.True(t, srv.IsWorkspaceConfigDoc(doc),
		"@type drives detection, not the URI basename")

	const chainSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [{ name = "dev" }]
layers = []
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/oddly-named.pxf", 1, chainSpec)))
	doc2 := srv.Documents().Get(uri.URI("file:///work/oddly-named.pxf"))
	require.NotNil(t, doc2)
	assert.True(t, srv.IsChainSpecDoc(doc2),
		"@type drives chain-spec detection too")
}

// Helper: capture diagnostics published for a specific URI from
// the most recent didOpen / didChange. Not used by all tests but
// keeps the imports honest.
var _ = func(diags []protocol.Diagnostic) {}
