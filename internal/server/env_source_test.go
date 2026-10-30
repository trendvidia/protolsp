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
	"go.lsp.dev/uri"
)

const envSourceChainSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]

sources = {
  "env_runtime": { kind = "env" options = { "prefix": "MYAPP_" } }
}

layers = [
  { id = "base"        path = "base.pxf"        for_each = ONCE }
  { id = "env-layer"   path = "env/${env}.pxf"  for_each = PER_ENV }
  { id = "env-runtime" path = "(env)"           source = "env_runtime" for_each = ONCE }
]
`

// initializeC7EnvChain stands up a workspace with an env-kind
// source in the chain and the named pre-written layer files.
// AppConfig (from buildAppConfigWithNestedDb) is the bound schema.
func initializeC7EnvChain(t *testing.T, layers map[string]string) (*Server, string) {
	t.Helper()
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(envSourceChainSpec), 0o600))
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
	require.NotNil(t, srv.Workspaces().SnapshotChain())
	return srv, workspaceDir
}

func TestEnvSource_HoverSurfacesEnvVarName(t *testing.T) {
	srv, ws := initializeC7EnvChain(t, map[string]string{
		"base.pxf":     `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "https://placeholder"
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "https://placeholder"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	require.NotEmpty(t, md)
	assert.Contains(t, md, "Available env var:")
	assert.Contains(t, md, "MYAPP_API_URL",
		"hover shows the env var name the runtime would consult")
	assert.Contains(t, md, "env_runtime",
		"hover names the source that contributes the env var")
}

func TestEnvSource_HoverNestedPathEnvVarName(t *testing.T) {
	srv, ws := initializeC7EnvChain(t, map[string]string{
		"base.pxf":     `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/dev.pxf": `@type c4test.v1.AppConfig
db {
  host = "placeholder"
}
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
db {
  host = "placeholder"
}
`)))

	// Cursor on `host` (line 2 of the body).
	md := hoverAt(t, srv, devURI, 2, 2)
	assert.Contains(t, md, "MYAPP_DB_HOST",
		"nested path encodes as PREFIX_PARENT_FIELD")
}

func TestEnvSource_HoverWithoutEnvSourceProducesNoHint(t *testing.T) {
	// Use the C6 chain spec — no env source.
	srv, ws := initializeC6Chain(t, map[string]string{
		"base.pxf":     `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "x"
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "x"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	assert.NotContains(t, md, "Available env var:",
		"no env source in chain → no env hint")
}

func TestEnvSource_ChainResolutionShowsEnvPlaceholder(t *testing.T) {
	srv, ws := initializeC7EnvChain(t, map[string]string{
		"base.pxf":     `@type c4test.v1.AppConfig` + "\napi_url = \"from-base\"\n",
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "from-dev"
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "from-dev"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	require.Contains(t, md, "Chain resolution:")
	assert.Contains(t, md, "(env source: env_runtime)",
		"env-sourced layer renders with a placeholder file label")
	assert.Contains(t, md, "<env>",
		"env-sourced layer's value column carries an explicit placeholder")
}

func TestEnvSource_RequiredFieldSuppressedWhenEnvSourcePresent(t *testing.T) {
	// AppConfig with required fields (from chain_required_test.go's
	// fixture). With an env source in the chain, the C3 required
	// diagnostic should fall silent — the runtime supplies via env.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(envSourceChainSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	for name, content := range map[string]string{
		"base.pxf":     `@type myapp.v1.AppConfig` + "\n",
		"env/prod.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":  `@type myapp.v1.AppConfig` + "\n",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(workspaceDir, name), []byte(content), 0o600))
	}
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigMessageType(t)) // the variant with pxf.required
	srv.Lookups().Swap(lookup, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain())

	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, devURI)
	assert.Empty(t, diags,
		"env source in chain → required-field check suppressed (runtime would populate via env)")
}

func TestEnvSource_RequiredFieldStillFiresWhenNoEnvSourcePresent(t *testing.T) {
	// Sanity check: without an env source, the required-field
	// diagnostic still fires as expected (C3 behavior unchanged).
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf":        `@type myapp.v1.AppConfig` + "\n",
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type myapp.v1.AppConfig` + "\n",
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, devURI)
	require.NotEmpty(t, diags,
		"no env source → required-field diagnostic fires (C3 behavior preserved)")
}

func TestEnvSource_BareEnvSourceHasNoPrefix(t *testing.T) {
	const bareEnvSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
sources = {
  "env_bare": { kind = "env" }
}
layers = [
  { id = "env"      path = "env/${env}.pxf" for_each = PER_ENV }
  { id = "env-vars" path = "(env)"          source = "env_bare" for_each = ONCE }
]
`
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(bareEnvSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, "env", "prod.pxf"),
		[]byte(`@type c4test.v1.AppConfig`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, "env", "dev.pxf"),
		[]byte(`@type c4test.v1.AppConfig
api_url = "x"
`), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigWithNestedDb(t))
	srv.Lookups().Swap(lookup, nil)

	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "x"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	assert.Contains(t, md, "API_URL",
		"bare uppercase form when source has no prefix option")
	assert.NotContains(t, md, "MYAPP_API_URL",
		"no extraneous prefix when source didn't declare one")
}

func TestEnvSource_StandaloneDocGetsNoEnvHint(t *testing.T) {
	srv, fc := newAppConfigServer(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work")
	_ = fc

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/scratch.pxf", 1, `@type c4test.v1.AppConfig
api_url = "x"
`)))
	md := hoverAt(t, srv, "file:///work/scratch.pxf", 1, 0)
	assert.NotContains(t, md, "Available env var:",
		"standalone doc → no env hint, even on a field that might be env-sourced elsewhere")
}
