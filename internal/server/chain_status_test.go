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
)

// chainStatusReply drives MethodChainStatus and decodes the result.
func chainStatusReply(t *testing.T, srv *Server) ChainStatusResult {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodChainStatus, ChainStatusParams{})
	require.NoError(t, err)

	var got ChainStatusResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &got))
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return got
}

const chainStatusSimpleSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "base" path = "base.pxf" for_each = ONCE }
]
`

func TestChainStatus_NotConfigured(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	got := chainStatusReply(t, srv)
	assert.False(t, got.Configured,
		"workspaces without a chameleon block report configured=false")
	assert.Empty(t, got.ChainSpecPath)
	assert.Empty(t, got.Environments)
	assert.Empty(t, got.Errors)
}

func TestChainStatus_ConfiguredWithFullChain(t *testing.T) {
	workspaceDir := t.TempDir()
	specPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(chainStatusSimpleSpec), 0o600))

	const wsConfig = `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "chain.pxf"
  schema_fqn = "myapp.v1.AppConfig"
}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(wsConfig), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	got := chainStatusReply(t, srv)

	assert.True(t, got.Configured)
	assert.Equal(t, specPath, got.ChainSpecPath)
	assert.Equal(t, "1.0.0", got.Version)
	assert.Equal(t, "dev", got.DefaultEnvironment)
	assert.Equal(t, "myapp.v1.AppConfig", got.SchemaFQN)
	assert.Empty(t, got.Errors)

	require.Len(t, got.Environments, 3,
		"all three declared environments surface in declaration order")
	assert.Equal(t, "prod", got.Environments[0].Name)
	assert.Equal(t, []string{"prod"}, got.Environments[0].Ancestors)
	assert.Equal(t, "sandbox", got.Environments[1].Name)
	assert.Equal(t, []string{"prod", "sandbox"}, got.Environments[1].Ancestors)
	assert.Equal(t, "dev", got.Environments[2].Name)
	assert.Equal(t, []string{"prod", "sandbox", "dev"}, got.Environments[2].Ancestors)
}

// TestChainStatus_KeyNameFlowsFromSpec pins the chameleon v0.22.1
// Environment.key_name addition: when an environment declares a
// key_name, it surfaces through the chain parser → ChainStatusResult
// without alteration. Environments that omit the field land with
// KeyName="" — that's the "inherit from parent / fall back to default
// recipient" signal the sops encrypt path consumes.
func TestChainStatus_KeyNameFlowsFromSpec(t *testing.T) {
	workspaceDir := t.TempDir()
	specPath := filepath.Join(workspaceDir, "chain.pxf")
	const spec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" key_name = "prod-key" }
  { name = "sandbox" extends = "prod" key_name = "sandbox-key" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	got := chainStatusReply(t, srv)
	require.Len(t, got.Environments, 3)
	assert.Equal(t, "prod-key", got.Environments[0].KeyName,
		"prod declares its key_name; surfaces unchanged")
	assert.Equal(t, "sandbox-key", got.Environments[1].KeyName)
	assert.Equal(t, "", got.Environments[2].KeyName,
		"dev omits key_name → empty (inherit/fall-back signal)")
}

func TestChainStatus_DisabledBlockReportsNotConfigured(t *testing.T) {
	workspaceDir := t.TempDir()
	specPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(chainStatusSimpleSpec), 0o600))

	const wsConfig = `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "chain.pxf"
  enabled = false
}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(wsConfig), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	got := chainStatusReply(t, srv)
	assert.False(t, got.Configured,
		"`enabled = false` suppresses chain mode even with chain_spec set")
}

func TestChainStatus_MissingChainFileSurfacesAsConfiguredWithNoData(t *testing.T) {
	// .protolsp.pxf declares a chain that doesn't exist on disk.
	// Loadable=false → chain stays nil; configured stays true (the
	// editor's status bar should show "configured but broken").
	workspaceDir := t.TempDir()
	const wsConfig = `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "does-not-exist.pxf"
}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(wsConfig), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	got := chainStatusReply(t, srv)
	assert.True(t, got.Configured)
	assert.Empty(t, got.ChainSpecPath,
		"path stays empty when the load itself failed")
	assert.Empty(t, got.Environments)
}

func TestChainStatus_SpecWithCycleSurfacesErrors(t *testing.T) {
	workspaceDir := t.TempDir()
	const cyclic = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [
  { name = "a" extends = "b" }
  { name = "b" extends = "a" }
]
layers = []
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(cyclic), 0o600))

	const wsConfig = `@type protolsp.v1.WorkspaceConfig

chameleon { chain_spec = "chain.pxf" }
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(wsConfig), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	got := chainStatusReply(t, srv)
	assert.True(t, got.Configured)
	require.NotEmpty(t, got.Errors,
		"cycle diagnostics surface in the chainStatus response")
	foundCycle := false
	for _, e := range got.Errors {
		if assertContainsServer(e.Message, "cycle") {
			foundCycle = true
		}
	}
	assert.True(t, foundCycle)
	// Environments slice still surfaces both declared names; the
	// editor can render them alongside the error.
	require.Len(t, got.Environments, 2)
}

// setupChainStatusWorkspace stages a workspace with the
// chainStatusSimpleSpec chain (envs: prod, sandbox, dev with
// default=dev) and returns an initialized server pointing at it.
// Used by the SetActiveEnvironment tests to drive a real chain
// through the validation + active-env-tracking path.
func setupChainStatusWorkspace(t *testing.T) *Server {
	t.Helper()
	workspaceDir := t.TempDir()
	specPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(chainStatusSimpleSpec), 0o600))
	const wsConfig = `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "chain.pxf"
}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(wsConfig), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	return srv
}

func TestSetActiveEnvironment_DefaultsToChainDefault(t *testing.T) {
	srv := setupChainStatusWorkspace(t)
	got := chainStatusReply(t, srv)
	assert.Equal(t, "dev", got.ActiveEnvironment,
		"unset activeEnv reports as the chain's default_environment")
}

func TestSetActiveEnvironment_RetainsValidEnv(t *testing.T) {
	srv := setupChainStatusWorkspace(t)
	prev, err := srv.SetActiveEnvironment("prod")
	require.NoError(t, err)
	// Previous value still echoes through the default-env fallback —
	// initial state had no override, so prev == default.
	assert.Equal(t, "dev", prev,
		"previous_environment mirrors what ChainStatus would have reported")
	got := chainStatusReply(t, srv)
	assert.Equal(t, "prod", got.ActiveEnvironment,
		"SetActiveEnv flips ChainStatus.activeEnvironment to the new value")
}

func TestSetActiveEnvironment_EmptyStringResetsToDefault(t *testing.T) {
	srv := setupChainStatusWorkspace(t)
	_, err := srv.SetActiveEnvironment("sandbox")
	require.NoError(t, err)
	prev, err := srv.SetActiveEnvironment("")
	require.NoError(t, err)
	assert.Equal(t, "sandbox", prev,
		"empty-string reset returns the previously-active env")
	got := chainStatusReply(t, srv)
	assert.Equal(t, "dev", got.ActiveEnvironment,
		"after reset, ActiveEnvironment falls back to DefaultEnvironment")
}

func TestSetActiveEnvironment_RejectsUnknownEnv(t *testing.T) {
	srv := setupChainStatusWorkspace(t)
	_, err := srv.SetActiveEnvironment("nonsense")
	require.Error(t, err)
	got := chainStatusReply(t, srv)
	assert.Equal(t, "dev", got.ActiveEnvironment,
		"failed validation must leave state untouched")
}

func TestSetActiveEnvironment_RejectsNonEmptyWithoutChain(t *testing.T) {
	// No chain configured at all — there's nothing to validate
	// against. We refuse any non-empty selection so the client can't
	// pin the server to a fictional env.
	srv, _ := newServerWithFakeConn(t)
	_, err := srv.SetActiveEnvironment("anything")
	require.Error(t, err)
}

func TestSetActiveEnvironment_AcceptsEmptyWithoutChain(t *testing.T) {
	// Empty env (reset) is always idempotent — no validation needed
	// because the result either way is "fall back to default."
	srv, _ := newServerWithFakeConn(t)
	prev, err := srv.SetActiveEnvironment("")
	require.NoError(t, err)
	assert.Empty(t, prev,
		"with no chain there's no default to echo back; previous is empty")
}

func TestChainStatus_NullParamsTolerated(t *testing.T) {
	// Editors that pre-encode the request without an explicit params
	// object send `null`. The dispatcher must accept that.
	srv, _ := newServerWithFakeConn(t)
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodChainStatus, nil)
	require.NoError(t, err)
	var got ChainStatusResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &got))
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	assert.False(t, got.Configured)
}

// assertContainsServer mirrors the helper from workspaceconfig tests
// — small substring check without pulling in package strings here
// (consistent with existing test patterns in this package).
func assertContainsServer(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
