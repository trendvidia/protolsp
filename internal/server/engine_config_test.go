// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// protolsp/engineConfig: the workspace's effective RFC-001
// §9.4 engine configuration, resolved through protocompile's shared
// engineconfig loader — the same discovery/precedence `pxf build`
// uses, so the LSP and the build tool always agree.

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
	"go.lsp.dev/uri"
)

func engineConfigReply(t *testing.T, srv *Server) EngineConfigResult {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodEngineConfig, nil)
	require.NoError(t, err)

	var got EngineConfigResult
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

// newServerWithRoot stages a server whose workspace root is a fresh
// temp directory, with PROTOWIRE_CONFIG cleared so the invoking
// environment can't leak a config into the test.
func newServerWithRoot(t *testing.T) (root string, srv *Server) {
	t.Helper()
	t.Setenv("PROTOWIRE_CONFIG", "")
	root = t.TempDir()
	srv, _ = newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	return root, srv
}

// A discovered protowire.config.textproto at the workspace root
// resolves with every configured field, source "discovered", and the
// file's path.
func TestEngineConfig_DiscoveredAtRoot(t *testing.T) {
	root, srv := newServerWithRoot(t)
	cfgPath := filepath.Join(root, "protowire.config.textproto")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
engine: "cel"
function_libraries: "myco/commons/validator.proto"
catalog_libraries: "myco/i18n/en.proto"
default_mode: EXECUTION_MODE_FAIL_FAST
max_recursion_depth: 16
strict_validation: true
`), 0o600))

	got := engineConfigReply(t, srv)
	assert.Empty(t, got.Error)
	assert.True(t, got.Configured)
	assert.Equal(t, "discovered", got.Source)
	assert.Equal(t, cfgPath, got.Path)
	assert.Equal(t, "cel", got.Engine)
	assert.Equal(t, []string{"myco/commons/validator.proto"}, got.FunctionLibraries)
	assert.Equal(t, []string{"myco/i18n/en.proto"}, got.CatalogLibraries)
	assert.Equal(t, "EXECUTION_MODE_FAIL_FAST", got.DefaultMode)
	assert.Equal(t, uint32(16), got.MaxRecursionDepth)
	assert.True(t, got.StrictValidation)
}

// No configuration file anywhere up the tree resolves to the §9.4
// built-in defaults: not configured, no engine (the field is reserved
// since protowire#282 — there is no default to report), COLLECT_ALL,
// depth 64.
func TestEngineConfig_Defaults(t *testing.T) {
	_, srv := newServerWithRoot(t)

	got := engineConfigReply(t, srv)
	assert.Empty(t, got.Error)
	assert.False(t, got.Configured)
	assert.Equal(t, "defaults", got.Source)
	assert.Empty(t, got.Path)
	assert.Empty(t, got.Engine)
	assert.Equal(t, "EXECUTION_MODE_COLLECT_ALL", got.DefaultMode)
	assert.Equal(t, uint32(64), got.MaxRecursionDepth)
	assert.False(t, got.StrictValidation)
}

// Malformed configuration is a hard loader error per §9.4 — no
// silent fallback to defaults — and travels in the Error field
// rather than failing the request.
func TestEngineConfig_MalformedFileReportsError(t *testing.T) {
	root, srv := newServerWithRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "protowire.config.textproto"),
		[]byte(`engine: "cel" no_such_field: true`), 0o600))

	got := engineConfigReply(t, srv)
	assert.NotEmpty(t, got.Error, "a malformed file must not silently default")
	assert.False(t, got.Configured)
	assert.Empty(t, got.Engine)
}

// The nearest config walking upward from the root wins — a config in
// the root's parent applies when the root itself has none, matching
// running `pxf build` from the workspace root.
func TestEngineConfig_NearestAncestorWins(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	parent := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(parent, "protowire.config.textproto"),
		[]byte(`engine: "starlark"`), 0o600))
	root := filepath.Join(parent, "app")
	require.NoError(t, os.MkdirAll(root, 0o750))

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))

	got := engineConfigReply(t, srv)
	assert.Empty(t, got.Error)
	assert.True(t, got.Configured)
	assert.Equal(t, "starlark", got.Engine)
	assert.Equal(t, filepath.Join(parent, "protowire.config.textproto"), got.Path)
}
