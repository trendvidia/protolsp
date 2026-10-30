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

	"github.com/trendvidia/protolsp/workspaceconfig"
)

// ---------------------------------------------------------------------------
// helpers: drive the three RPCs and decode their replies
// ---------------------------------------------------------------------------

// configListReply drives MethodConfigList and decodes the result.
// Mirrors the effectiveConfigReply pattern from effective_config_test.go.
func configListReply(t *testing.T, srv *Server) ConfigListResult {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodConfigList, nil)
	require.NoError(t, err)
	return decodeReply[ConfigListResult](t, srv, call)
}

func configGetReply(t *testing.T, srv *Server, params ConfigGetParams) ConfigGetResult {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodConfigGet, params)
	require.NoError(t, err)
	return decodeReply[ConfigGetResult](t, srv, call)
}

func configSetReply(t *testing.T, srv *Server, params ConfigSetParams) ConfigSetResult {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodConfigSet, params)
	require.NoError(t, err)
	return decodeReply[ConfigSetResult](t, srv, call)
}

// configSetReplyExpectingError drives MethodConfigSet but expects the
// reply to carry a JSON-RPC error. Returns the error for assertion.
func configSetReplyExpectingError(t *testing.T, srv *Server, params ConfigSetParams) error {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodConfigSet, params)
	require.NoError(t, err)
	var got error
	reply := func(_ context.Context, _ any, replyErr error) error {
		got = replyErr
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return got
}

func decodeReply[T any](t *testing.T, srv *Server, call rpcRequest) T {
	t.Helper()
	var got T
	reply := func(_ context.Context, result any, replyErr error) error {
		require.NoError(t, replyErr)
		b, err := testCodec.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, testCodec.Unmarshal(b, &got))
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return got
}

// rawJSON is sugar for building ConfigSetParams.Value from a Go value.
func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := testCodec.Marshal(v)
	require.NoError(t, err)
	return b
}

// ---------------------------------------------------------------------------
// configList
// ---------------------------------------------------------------------------

func TestConfigList_ReturnsRegistryAttributes(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	got := configListReply(t, srv)

	require.Len(t, got.Attributes, len(workspaceconfig.AttributeRegistry))
	// Spot-check a representative attribute of each Kind. The full
	// list lives in workspaceconfig.AttributeRegistry; we only need
	// to verify the RPC surfaces it intact.
	byName := make(map[string]ConfigAttribute, len(got.Attributes))
	for _, a := range got.Attributes {
		byName[a.Name] = a
	}
	assert.Equal(t, "string", byName["registry.address"].Kind)
	assert.NotEmpty(t, byName["registry.address"].Description)
	assert.Equal(t, "string[]", byName["proto_files.paths"].Kind)
	assert.Equal(t, "bool", byName["chameleon.enabled"].Kind)
}

func TestConfigList_OrderMatchesRegistry(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	got := configListReply(t, srv)

	require.Len(t, got.Attributes, len(workspaceconfig.AttributeRegistry))
	for i, a := range got.Attributes {
		assert.Equal(t, workspaceconfig.AttributeRegistry[i].Name, a.Name,
			"declaration order must match the registry table")
	}
}

// ---------------------------------------------------------------------------
// configGet
// ---------------------------------------------------------------------------

func TestConfigGet_AbsentAttributeReturnsZeroAndPresentFalse(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	got := configGetReply(t, srv, ConfigGetParams{Name: "registry.address"})
	assert.Equal(t, "registry.address", got.Name)
	assert.Equal(t, "string", got.Kind)
	assert.Equal(t, "", got.Value, "zero string value when absent")
	assert.False(t, got.Present)
}

func TestConfigGet_PresentAttributeReadsFromFile(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig

registry {
  address = "stored:443"
}
`), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	got := configGetReply(t, srv, ConfigGetParams{Name: "registry.address"})
	assert.Equal(t, "stored:443", got.Value)
	assert.True(t, got.Present)
}

func TestConfigGet_LiteralValueNotInterpolated(t *testing.T) {
	// ${env.X} must come back verbatim — interpolation happens at
	// render-time, not at Get-time. Otherwise Get→Set round-trips
	// would corrupt the source-of-truth value in the file.
	t.Setenv("MY_TOKEN", "shhh-secret")
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig

registry {
  token = "${env.MY_TOKEN}"
}
`), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	got := configGetReply(t, srv, ConfigGetParams{Name: "registry.token"})
	assert.Equal(t, "${env.MY_TOKEN}", got.Value,
		"Get returns the stored literal, not the interpolated value")
}

func TestConfigGet_UnknownAttributeErrors(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodConfigGet,
		ConfigGetParams{Name: "registry.bogus"})
	require.NoError(t, err)

	var replyErr error
	reply := func(_ context.Context, _ any, e error) error {
		replyErr = e
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	require.Error(t, replyErr)
	assert.Contains(t, replyErr.Error(), "unknown attribute")
}

// ---------------------------------------------------------------------------
// configSet
// ---------------------------------------------------------------------------

func TestConfigSet_CreatesFileWhenAbsent(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	got := configSetReply(t, srv, ConfigSetParams{
		Name:  "registry.address",
		Value: rawJSON(t, "fresh:443"),
	})
	assert.True(t, got.OK)

	// File now exists on disk and contains the value.
	data, err := os.ReadFile(filepath.Join(workspaceDir, ".protolsp.pxf"))
	require.NoError(t, err)
	assert.Contains(t, string(data), `"fresh:443"`)
	assert.Contains(t, string(data), `@type protolsp.v1.WorkspaceConfig`,
		"Set on absent file seeds the canonical @type directive")
}

func TestConfigSet_MutatesExistingFilePreservingComments(t *testing.T) {
	workspaceDir := t.TempDir()
	original := `@type protolsp.v1.WorkspaceConfig

# the team's shared registry
registry {
  address = "old:443"
  namespace = "team-x"
}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(original), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	got := configSetReply(t, srv, ConfigSetParams{
		Name:  "registry.address",
		Value: rawJSON(t, "new:443"),
	})
	assert.True(t, got.OK)

	data, err := os.ReadFile(filepath.Join(workspaceDir, ".protolsp.pxf"))
	require.NoError(t, err)
	final := string(data)
	assert.Contains(t, final, "# the team's shared registry",
		"comments survive the round-trip")
	assert.Contains(t, final, `"new:443"`)
	assert.NotContains(t, final, `"old:443"`)
	assert.Contains(t, final, `namespace = "team-x"`,
		"sibling assignment is untouched")
}

func TestConfigSet_GetAfterSetReturnsNewValue(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	configSetReply(t, srv, ConfigSetParams{
		Name:  "proto_files.paths",
		Value: rawJSON(t, []string{"proto/", "third_party/"}),
	})
	got := configGetReply(t, srv, ConfigGetParams{Name: "proto_files.paths"})
	assert.True(t, got.Present)
	// JSON round-trip turns []string into []any.
	assert.Equal(t, []any{"proto/", "third_party/"}, got.Value)
}

func TestConfigSet_SetThenSetReplacesValue(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	configSetReply(t, srv, ConfigSetParams{
		Name:  "registry.address",
		Value: rawJSON(t, "first:443"),
	})
	configSetReply(t, srv, ConfigSetParams{
		Name:  "registry.address",
		Value: rawJSON(t, "second:443"),
	})
	got := configGetReply(t, srv, ConfigGetParams{Name: "registry.address"})
	assert.Equal(t, "second:443", got.Value)
}

func TestConfigSet_TypeMismatchErrors(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	// String value into a bool attribute.
	err := configSetReplyExpectingError(t, srv, ConfigSetParams{
		Name:  "chameleon.enabled",
		Value: rawJSON(t, "yes-please"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chameleon.enabled")
}

func TestConfigSet_SemanticValidatorRejects(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	err := configSetReplyExpectingError(t, srv, ConfigSetParams{
		Name:  "security.inline_secrets",
		Value: rawJSON(t, "maybe"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow|warn|refuse")
}

func TestConfigSet_UnknownAttributeErrors(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	err := configSetReplyExpectingError(t, srv, ConfigSetParams{
		Name:  "registry.bogus",
		Value: rawJSON(t, "x"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown attribute")
}

func TestConfigSet_TriggersReloadInjectingValueIntoServerState(t *testing.T) {
	// After Set, the server's parsed file config should reflect the
	// new value — the reload cascade ran. Without this assertion,
	// Set would silently update the file but the running LSP would
	// still serve the previous value until the next watcher event.
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	configSetReply(t, srv, ConfigSetParams{
		Name:  "registry.address",
		Value: rawJSON(t, "reloaded:443"),
	})

	require.NotNil(t, srv.Workspaces().SnapshotFileConfig(),
		"reload cascade populates rootFileConfig from the freshly-written file")
	assert.Equal(t, "reloaded:443", srv.Workspaces().SnapshotFileConfig().Registry.Address)
}

func TestConfigSet_UnknownWorkspaceDirErrors(t *testing.T) {
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	err := configSetReplyExpectingError(t, srv, ConfigSetParams{
		WorkspaceDir: "/tmp/does-not-exist-as-a-subworkspace",
		Name:         "registry.address",
		Value:        rawJSON(t, "x:1"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown workspaceDir")
}
