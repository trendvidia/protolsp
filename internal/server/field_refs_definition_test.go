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

const c5ChainSpec = `@type chameleon.v1.ChainSpec

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

// initializeC5Chain stands up a chain workspace + the AppConfig
// with nested DbConfig (so dotted paths exercise both top-level
// and nested-block assignments) + the layer files passed in.
func initializeC5Chain(t *testing.T, layers map[string]string) (*Server, string) {
	t.Helper()
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(c5ChainSpec), 0o600))
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
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "chain must load for C5 tests")
	return srv, workspaceDir
}

func TestFieldRefDef_JumpsToDeepestLayerSettingPath(t *testing.T) {
	// Both env/prod.pxf and env/sandbox.pxf set db.host. dev's
	// ancestry is [prod, sandbox, dev]; deepest setter is sandbox.
	srv, ws := initializeC5Chain(t, map[string]string{
		"base.pxf": `@type c4test.v1.AppConfig
api_url = "https://base.example"
`,
		"env/prod.pxf": `@type c4test.v1.AppConfig
db {
  host = "prod-host"
}
`,
		"env/sandbox.pxf": `@type c4test.v1.AppConfig
db {
  host = "sandbox-host"
}
`,
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "https://${db.host}/v1"
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "https://${db.host}/v1"
`)))

	// Cursor inside `${db.host}` on line 1 (0-indexed). The string
	// `api_url = "https://${db.host}/v1"` puts `db.host` at chars
	// 20..26 (the `db` is at char 20 after `"https://${`).
	locs := definitionFromHandle(t, srv, string(devURI), 1, 23) // mid-token
	require.Len(t, locs, 1, "go-to-definition resolves to one location")
	sandboxURI := uri.File(filepath.Join(ws, "env", "sandbox.pxf"))
	assert.Equal(t, sandboxURI, locs[0].URI,
		"deepest layer setting db.host is env/sandbox.pxf (closer to leaf than prod)")
	// Location is on the `host = ...` line (line 2, 0-indexed) in
	// the nested block.
	assert.Equal(t, uint32(2), locs[0].Range.Start.Line,
		"location anchors on the `host = ...` line inside the block")
}

func TestFieldRefDef_FallsBackToSchemaWhenNoLayerSetsPath(t *testing.T) {
	// No layer sets `debug`; reference resolves to the schema's
	// defining .proto file (via the existing definition pipeline).
	srv, ws := initializeC5Chain(t, map[string]string{
		"base.pxf":        `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf":    `@type c4test.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "${debug}"
`,
	})
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "${debug}"
`)))

	// Cursor inside `${debug}`. `api_url = "${debug}"` — the `d`
	// of debug is at char 13; cursor at char 15 (mid-token).
	locs := definitionFromHandle(t, srv, string(devURI), 1, 15)
	require.Len(t, locs, 1,
		"fallback resolves to the schema's defining .proto file")
	expected := uri.File(filepath.Join(ws, "c4test", "v1", "app_config.proto"))
	assert.Equal(t, expected, locs[0].URI,
		"schema fallback URI is the descriptor's file path under workspace root")
}

func TestFieldRefDef_StandaloneDocFallsBackToSchema(t *testing.T) {
	// A standalone (non-chain) document with a reference — should
	// just hit the schema fallback.
	srv, fc := newAppConfigServer(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work")
	_ = fc

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/scratch.pxf", 1, `@type c4test.v1.AppConfig
api_url = "${debug}"
`)))
	locs := definitionFromHandle(t, srv, "file:///work/scratch.pxf", 1, 14)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file:///work/c4test/v1/app_config.proto"), locs[0].URI,
		"standalone document jumps to the schema descriptor's file")
}

func TestFieldRefDef_EnvAndCtxPrefixesReturnNil(t *testing.T) {
	srv, _ := newAppConfigServer(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work")

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/s.pxf", 1, `@type c4test.v1.AppConfig
api_url = "${env.HOST}-${ctx.region}"
`)))
	// Cursor on env.HOST.
	locs := definitionFromHandle(t, srv, "file:///work/s.pxf", 1, 16)
	assert.Empty(t, locs,
		"env.* has no edit-time definition; handler returns nil")
}

func TestFieldRefDef_UnsavedEditInAncestorWins(t *testing.T) {
	// Open env/sandbox.pxf with a fresh assignment of db.host in
	// the editor (disk version doesn't have it). The reference
	// from dev should still resolve to the open buffer.
	srv, ws := initializeC5Chain(t, map[string]string{
		"base.pxf":     `@type c4test.v1.AppConfig` + "\n",
		"env/prod.pxf": `@type c4test.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type c4test.v1.AppConfig
db {
  port = 5432
}
`,
		"env/dev.pxf": `@type c4test.v1.AppConfig
api_url = "${db.host}"
`,
	})
	sandboxURI := uri.File(filepath.Join(ws, "env", "sandbox.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(sandboxURI), 1, `@type c4test.v1.AppConfig
db {
  host = "unsaved-edit"
  port = 5432
}
`)))

	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
api_url = "${db.host}"
`)))

	locs := definitionFromHandle(t, srv, string(devURI), 1, 15)
	require.Len(t, locs, 1)
	assert.Equal(t, sandboxURI, locs[0].URI,
		"unsaved editor buffer for an ancestor is consulted before disk")
}

func TestFieldRefDef_CursorOutsideRefFallsThroughToFieldAtCursor(t *testing.T) {
	// Cursor on `api_url` (the field name on the assignment),
	// NOT on the ${...} content. The existing field-at-cursor
	// resolver should fire and jump to the schema.
	srv, fc := newAppConfigServer(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work")
	_ = fc

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/s.pxf", 1, `@type c4test.v1.AppConfig
api_url = "${db.host}"
`)))
	// Cursor on char 0 of line 1 = on `a` of `api_url`.
	locs := definitionFromHandle(t, srv, "file:///work/s.pxf", 1, 0)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file:///work/c4test/v1/app_config.proto"), locs[0].URI,
		"cursor on the field name resolves via the existing pipeline, not C5")
}

func TestFieldRefDef_EncryptedAncestorSkipped(t *testing.T) {
	// An encrypted ancestor "sets" the field on paper — but it's
	// opaque to the LSP, so the resolver should skip it and look
	// further down (or fall through to schema if no readable
	// layer matches).
	const encryptedSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "sec" path = "secrets.pxf"   for_each = ONCE encrypted = true }
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(encryptedSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "secrets.pxf"), []byte(`@type c4test.v1.AppConfig
api_url = "from-encrypted"
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "prod.pxf"),
		[]byte(`@type c4test.v1.AppConfig`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "dev.pxf"),
		[]byte(`@type c4test.v1.AppConfig`+"\n"), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigWithNestedDb(t))
	srv.Lookups().Swap(lookup, nil)

	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig
greeting = "${api_url}"
`)))
	// Cursor inside ${api_url}.
	locs := definitionFromHandle(t, srv, string(devURI), 1, 16)
	require.Len(t, locs, 1)
	// Should NOT be secrets.pxf — that's encrypted, skipped.
	assert.NotEqual(t, uri.File(filepath.Join(workspaceDir, "secrets.pxf")), locs[0].URI,
		"encrypted layer is skipped on go-to-definition")
	// Falls through to schema.
	assert.Equal(t, uri.File(filepath.Join(workspaceDir, "c4test", "v1", "app_config.proto")), locs[0].URI)
}

// ---------------------------------------------------------------------------
// Unit-level tests of the helpers
// ---------------------------------------------------------------------------
