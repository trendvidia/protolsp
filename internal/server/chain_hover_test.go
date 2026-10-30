// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const c6ChainSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "base" path = "base.pxf"        for_each = ONCE }
  { id = "env"  path = "env/${env}.pxf"  for_each = PER_ENV }
]
`

func initializeC6Chain(t *testing.T, layers map[string]string) (*Server, string) {
	t.Helper()
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(c6ChainSpec), 0o600))
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
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "chain must load for C6 tests")
	return srv, workspaceDir
}

// hoverAt drives the hover dispatcher and returns the rendered
// markdown string (or "" when the server returned no hover).
func hoverAt(t *testing.T, srv *Server, docURI uri.URI, line, char uint32) string {
	t.Helper()
	h := srv.HoverFor(uri.URI(docURI), protocol.Position{Line: line, Character: char})
	if h == nil {
		return ""
	}
	return hoverBody(h)
}

func TestChainHover_RendersResolutionTableAcrossLayers(t *testing.T) {
	// base.pxf sets api_url; env/prod and env/sandbox both override
	// it. dev.pxf doesn't touch it. Deepest setter = env/sandbox.
	srv, ws := initializeC6Chain(t, map[string]string{
		"base.pxf": `@type c4test.v1.AppConfig
api_url = "https://base.example"
`,
		"env/prod.pxf": `@type c4test.v1.AppConfig
api_url = "https://prod.example"
`,
		"env/sandbox.pxf": `@type c4test.v1.AppConfig
api_url = "https://sandbox.example"
`,
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "https://dev.example"
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "https://dev.example"
`)))

	// Cursor on `api_url` (line 1, char 0).
	md := hoverAt(t, srv, devURI, 1, 0)
	require.NotEmpty(t, md)
	assert.Contains(t, md, "Chain resolution:")
	for _, want := range []string{
		"base.pxf",
		"env/prod.pxf",
		"env/sandbox.pxf",
		"env/dev.pxf",
		"https://base.example",
		"https://prod.example",
		"https://sandbox.example",
		"https://dev.example",
	} {
		assert.Contains(t, md, want,
			"chain resolution must list %q", want)
	}
	assert.Contains(t, md, "← effective",
		"deepest setter is marked as effective")
	// dev.pxf is the deepest setter; the effective marker should
	// be on its row.
	effectiveLineFound := false
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "env/dev.pxf") && strings.Contains(line, "← effective") {
			effectiveLineFound = true
		}
	}
	assert.True(t, effectiveLineFound,
		"effective marker is on the env/dev.pxf row (deepest setter)")
}

func TestChainHover_AbsentLayersOmittedFromTable(t *testing.T) {
	// Only env/sandbox sets api_url; base/prod/dev don't. Those
	// rows should be omitted to keep the table tight.
	srv, ws := initializeC6Chain(t, map[string]string{
		"base.pxf":     `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type c4test.v1.AppConfig
api_url = "from-sandbox"
`,
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "${api_url}-suffix"
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "${api_url}-suffix"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	require.Contains(t, md, "Chain resolution:")
	// sandbox + dev set it; base + prod don't.
	assert.Contains(t, md, "env/sandbox.pxf")
	assert.Contains(t, md, "env/dev.pxf")
	assert.NotContains(t, md, "base.pxf",
		"layers without an assignment are dropped from the table")
	assert.NotContains(t, md, "env/prod.pxf",
		"layers without an assignment are dropped from the table")
}

func TestChainHover_NestedPathRenders(t *testing.T) {
	// Cursor on `host` inside `db { host = ... }`. Path is
	// "db.host". Ancestors set db.host; effective should mark
	// the deepest.
	srv, ws := initializeC6Chain(t, map[string]string{
		"base.pxf": `@type c4test.v1.AppConfig
db { host = "base-host" }
`,
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type c4test.v1.AppConfig
db { host = "sandbox-host" }
`,
		"env/dev.pxf": `@type c4test.v1.AppConfig
db {
  host = "dev-host"
}
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
db {
  host = "dev-host"
}
`)))

	// Cursor on `host` (line 2 in the dev file: "  host = ...").
	md := hoverAt(t, srv, devURI, 2, 2)
	require.Contains(t, md, "Chain resolution:")
	assert.Contains(t, md, "base-host")
	assert.Contains(t, md, "sandbox-host")
	assert.Contains(t, md, "dev-host")
	assert.NotContains(t, md, "env/prod.pxf",
		"prod row omitted (doesn't set db.host)")
}

func TestChainHover_EncryptedLayerShowsPlaceholder(t *testing.T) {
	// Encrypted layer renders as <encrypted> regardless of its
	// actual content (which the LSP doesn't read). Effective
	// marker can't be on encrypted rows.
	const encryptedSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "sec" path = "secrets.pxf"    for_each = ONCE encrypted = true }
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	ws := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(ws, "chain.pxf"), []byte(encryptedSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(ws, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "env"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "secrets.pxf"),
		[]byte(`@type c4test.v1.AppConfig
api_url = "from-encrypted"
`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "env", "prod.pxf"),
		[]byte(`@type c4test.v1.AppConfig`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "env", "dev.pxf"),
		[]byte(`@type c4test.v1.AppConfig
api_url = "from-dev"
`), 0o600))

	srv, _ := initializeWithWorkspace(t, ws, nil)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigWithNestedDb(t))
	srv.Lookups().Swap(lookup, nil)

	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "from-dev"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	require.Contains(t, md, "Chain resolution:")
	assert.Contains(t, md, "<encrypted>",
		"encrypted layer is shown with a placeholder value")
	// Effective should be on env/dev.pxf, NOT secrets.pxf (which
	// is opaque — we can't claim it's winning the merge).
	effectiveOnDev := false
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "env/dev.pxf") && strings.Contains(line, "← effective") {
			effectiveOnDev = true
		}
	}
	assert.True(t, effectiveOnDev,
		"effective marker is on the deepest READABLE setter; encrypted is opaque")
}

func TestChainHover_SecretFieldRedactsAllValues(t *testing.T) {
	// Set up a separate workspace: chain.pxf + .protolsp.pxf +
	// layer files using Credentials (which has a pxf.Secret field).
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(c6ChainSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	for name, content := range map[string]string{
		"base.pxf": `@type acme.demo.v1.Credentials
password = "base-secret"
`,
		"env/prod.pxf": `@type acme.demo.v1.Credentials` + "\n",
		"env/sandbox.pxf": `@type acme.demo.v1.Credentials
password = "sandbox-secret"
`,
		"env/dev.pxf": `@type acme.demo.v1.Credentials
password = "dev-secret"
`,
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(workspaceDir, name), []byte(content), 0o600))
	}
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain())

	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type acme.demo.v1.Credentials
password = "dev-secret"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	require.Contains(t, md, "Chain resolution:")
	// None of the layer plaintexts should appear in the hover.
	for _, leaked := range []string{"base-secret", "sandbox-secret", "dev-secret"} {
		assert.NotContains(t, md, leaked,
			"pxf.Secret values must not appear verbatim in the chain table; got leaked %q", leaked)
	}
	assert.Contains(t, md, "<redacted>",
		"secret fields show <redacted> across every set row")
}

func TestChainHover_StandaloneDocHasNoChainSection(t *testing.T) {
	// File outside any chain → no chain-resolution section.
	srv, fc := newAppConfigServer(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work")
	_ = fc

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/scratch.pxf", 1, `@type c4test.v1.AppConfig
api_url = "x"
`)))
	md := hoverAt(t, srv, "file:///work/scratch.pxf", 1, 0)
	require.NotEmpty(t, md, "standalone hover still produces default markdown")
	assert.NotContains(t, md, "Chain resolution:",
		"standalone docs don't get the chain section")
}

func TestChainHover_OnceLayerHasNoChainSection(t *testing.T) {
	// base.pxf is ONCE. Hovering inside it should NOT show a
	// chain section — the ONCE strategy has no env-bound resolve
	// order to render.
	srv, ws := initializeC6Chain(t, map[string]string{
		"base.pxf": `@type c4test.v1.AppConfig
api_url = "from-base"
`,
		"env/prod.pxf":    `@type c4test.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type c4test.v1.AppConfig` + "\n",
	})
	baseURI := uri.File(filepath.Join(ws, "base.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(baseURI), 1, `@type c4test.v1.AppConfig
api_url = "from-base"
`)))

	md := hoverAt(t, srv, baseURI, 1, 0)
	require.NotEmpty(t, md, "ONCE-layer hover still has the default markdown")
	assert.NotContains(t, md, "Chain resolution:",
		"ONCE chain layers don't get a chain-resolution section")
}

func TestChainHover_UnsavedEditOnAncestorReflectsInTable(t *testing.T) {
	// env/sandbox.pxf is open with an unsaved edit that introduces
	// a new value for api_url. The dev hover must reflect the open
	// buffer, not disk.
	srv, ws := initializeC6Chain(t, map[string]string{
		"base.pxf":        `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf":    `@type c4test.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type c4test.v1.AppConfig` + "\n",
	})
	sandboxURI := uri.File(filepath.Join(ws, "env", "sandbox.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(sandboxURI), 1, `@type c4test.v1.AppConfig
api_url = "unsaved-from-sandbox"
`)))

	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "dev-value"
`)))

	md := hoverAt(t, srv, devURI, 1, 0)
	require.Contains(t, md, "Chain resolution:")
	assert.Contains(t, md, "unsaved-from-sandbox",
		"the table consults open-document state for ancestor layers")
}

// ---------------------------------------------------------------------------
// Unit-level helpers
// ---------------------------------------------------------------------------
