// End-to-end tests for ChameleonService.SetEnvironmentKeyName.
// The unit-level AST edit tests live in internal/chain; this
// file pins the wire contract — what codes flow back to the
// client, that the on-disk chain.pxf actually changes, and that
// the next GetChainStatus reflects the new value without a
// watched-files reload.

package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// setEnvKeyNameSpec is a 3-environment chain with no key_name
// assignments anywhere — the test fixtures start fresh and the
// individual cases drive the RPC to set / replace / clear.
const setEnvKeyNameSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`

// setupSetEnvKeyName stages a chain.pxf + .protolsp.pxf in a tmp
// workspace, initializes the server, returns the chain.pxf path +
// the bearer-auth'd Chameleon client.
func setupSetEnvKeyName(t *testing.T) (
	workspaceDir, chainPath string,
	client rpcpbconnect.ChameleonServiceClient,
	srv *Server,
) {
	t.Helper()
	workspaceDir = t.TempDir()
	chainPath = filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(chainPath, []byte(setEnvKeyNameSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	srv, _ = initializeWithWorkspace(t, workspaceDir, nil)
	require.NotNil(t, srv.rpc)
	client = rpcpbconnect.NewChameleonServiceClient(
		http.DefaultClient,
		srv.rpc.address,
		connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token)),
	)
	return
}

func TestSetEnvironmentKeyName_RPC_AddsField(t *testing.T) {
	_, chainPath, client, srv := setupSetEnvKeyName(t)
	resp, err := client.SetEnvironmentKeyName(context.Background(),
		connect.NewRequest(&rpcpb.SetEnvironmentKeyNameRequest{
			Environment: "prod",
			KeyName:     "prod-key",
		}))
	require.NoError(t, err)
	assert.Equal(t, "prod-key", resp.Msg.GetKeyName())

	// On-disk: the chain.pxf now carries the new assignment.
	bytes, err := os.ReadFile(chainPath)
	require.NoError(t, err)
	assert.Contains(t, string(bytes), `key_name = "prod-key"`)

	// In-memory chain reloaded synchronously so the next status
	// pull sees the new value.
	got := chainStatusReply(t, srv)
	require.Len(t, got.Environments, 3)
	assert.Equal(t, "prod-key", got.Environments[0].KeyName)
}

func TestSetEnvironmentKeyName_RPC_ClearsField(t *testing.T) {
	// Pre-seed an existing key_name on sandbox, then clear it.
	workspaceDir := t.TempDir()
	chainPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" key_name = "sandbox-key" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	client := rpcpbconnect.NewChameleonServiceClient(
		http.DefaultClient, srv.rpc.address,
		connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token)),
	)

	_, err := client.SetEnvironmentKeyName(context.Background(),
		connect.NewRequest(&rpcpb.SetEnvironmentKeyNameRequest{
			Environment: "sandbox",
			KeyName:     "",
		}))
	require.NoError(t, err)

	bytes, err := os.ReadFile(chainPath)
	require.NoError(t, err)
	// Clear path drops the assignment entirely — both forms
	// (`key_name = ""` and "field absent") mean the same thing
	// semantically, but absence is the canonical representation.
	assert.NotContains(t, string(bytes), "key_name")

	got := chainStatusReply(t, srv)
	assert.Equal(t, "", got.Environments[1].KeyName)
}

func TestSetEnvironmentKeyName_RPC_EnvNotFound_InvalidArgument(t *testing.T) {
	_, _, client, _ := setupSetEnvKeyName(t)
	_, err := client.SetEnvironmentKeyName(context.Background(),
		connect.NewRequest(&rpcpb.SetEnvironmentKeyNameRequest{
			Environment: "staging",
			KeyName:     "any",
		}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err),
		"missing env maps to InvalidArgument so the UI can prompt to add the env first")
	assert.Contains(t, err.Error(), "staging")
}

func TestSetEnvironmentKeyName_RPC_EmptyEnv_InvalidArgument(t *testing.T) {
	_, _, client, _ := setupSetEnvKeyName(t)
	_, err := client.SetEnvironmentKeyName(context.Background(),
		connect.NewRequest(&rpcpb.SetEnvironmentKeyNameRequest{
			Environment: "",
			KeyName:     "any",
		}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestSetEnvironmentKeyName_RPC_NoChainConfigured_FailedPrecondition(t *testing.T) {
	// Workspace with no chain configured at all.
	workspaceDir := t.TempDir()
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	client := rpcpbconnect.NewChameleonServiceClient(
		http.DefaultClient, srv.rpc.address,
		connect.WithInterceptors(bearerClientInterceptor(srv.rpc.token)),
	)
	_, err := client.SetEnvironmentKeyName(context.Background(),
		connect.NewRequest(&rpcpb.SetEnvironmentKeyNameRequest{
			Environment: "prod",
			KeyName:     "x",
		}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err),
		"no chain → FailedPrecondition (the editor would gray the affordance out anyway, but the wire contract is precise)")
	assert.Contains(t, strings.ToLower(err.Error()), "chameleon",
		"error message mentions chameleon so the user knows what's missing")
}
