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

// Setting up a monorepo workspace with two sub-projects, each
// declaring its own `.protolsp.pxf` + `chain.pxf`. The fixture
// returns the workspace root + the two sub-workspace dirs so
// tests can drive docs against either.
func writeMonorepoFixture(t *testing.T) (root, svcA, svcB string) {
	t.Helper()
	root = t.TempDir()
	svcA = filepath.Join(root, "svc-a")
	svcB = filepath.Join(root, "svc-b")
	require.NoError(t, os.MkdirAll(filepath.Join(svcA, "env"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(svcB, "env"), 0o755))

	// svc-a: chain with PER_ENV layer `env/${env}.pxf`
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, "chain.pxf"), []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, "env", "dev.pxf"),
		[]byte("@type c4test.v1.AppConfig\n"), 0o600))

	// svc-b: same shape but the chain has DIFFERENT environment
	// names so a misrouted doc would land in the wrong chain.
	require.NoError(t, os.WriteFile(
		filepath.Join(svcB, "chain.pxf"), []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
environments = [
  { name = "staging" }
  { name = "prod" extends = "staging" }
]
layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcB, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcB, "env", "prod.pxf"),
		[]byte("@type c4test.v1.AppConfig\n"), 0o600))

	return root, svcA, svcB
}

func TestFindWorkspaceDirFor_FindsNearestAncestor(t *testing.T) {
	root, svcA, _ := writeMonorepoFixture(t)
	docPath := filepath.Join(svcA, "env", "dev.pxf")
	got, ok := FindWorkspaceDirFor(docPath, root)
	require.True(t, ok)
	assert.Equal(t, svcA, got,
		"doc inside svc-a maps to svc-a's .protolsp.pxf, not the root")
}

func TestFindWorkspaceDirFor_FallsBackToRootWhenNoConfigInAncestry(t *testing.T) {
	root := t.TempDir()
	// No .protolsp.pxf anywhere.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	got, ok := FindWorkspaceDirFor(filepath.Join(root, "sub", "f.pxf"), root)
	require.True(t, ok)
	assert.Equal(t, root, got,
		"no .protolsp.pxf in ancestry → root workspace wins")
}

func TestFindWorkspaceDirFor_StopsAtRoot(t *testing.T) {
	// Even if there's a .protolsp.pxf OUTSIDE the LSP workspace
	// root, the walk shouldn't reach it.
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".protolsp.pxf"), []byte(""), 0o600))
	workspaceRoot := filepath.Join(tmp, "inside")
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceRoot, "sub"), 0o755))
	got, ok := FindWorkspaceDirFor(
		filepath.Join(workspaceRoot, "sub", "f.pxf"),
		workspaceRoot)
	require.True(t, ok)
	assert.Equal(t, workspaceRoot, got,
		"walk doesn't traverse past the LSP workspace root")
}

func TestEnsureWorkspaceForDoc_PopulatesWorkspaceDir(t *testing.T) {
	root, svcA, _ := writeMonorepoFixture(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	doc := srv.Documents().Get(devURI)
	require.NotNil(t, doc)
	assert.Equal(t, svcA, doc.WorkspaceDir(),
		"doc inside svc-a is bound to svc-a's workspace")
}

func TestEnsureWorkspaceForDoc_LazyLoadsSubWorkspaceState(t *testing.T) {
	root, svcA, _ := writeMonorepoFixture(t)
	srv, _ := initializeWithWorkspace(t, root, nil)
	require.Zero(t, srv.SubWorkspaceCount(),
		"no sub-workspaces loaded at initialize")

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	require.Equal(t, 1, srv.SubWorkspaceCount())
	sw := subWS(t, srv, svcA)
	require.NotNil(t, sw)
	require.NotNil(t, sw.FileConfig)
	assert.Equal(t, "chain.pxf", sw.FileConfig.Chameleon.ChainSpec)
	require.NotNil(t, sw.Chain)
	assert.Contains(t, sw.Chain.Environments, "dev",
		"svc-a's chain knows about its own envs (dev, prod)")
	assert.NotContains(t, sw.Chain.Environments, "staging",
		"svc-a's chain does NOT have svc-b's `staging` env")
}

func TestLayerInfo_PerWorkspaceClassification(t *testing.T) {
	// A doc under svc-a should be classified against svc-a's
	// chain, not the root's (which doesn't exist).
	root, svcA, svcB := writeMonorepoFixture(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	devDoc := srv.Documents().Get(devURI)
	require.NotNil(t, devDoc.LayerInfo)
	assert.True(t, devDoc.LayerInfo.IsLayer,
		"env/dev.pxf inside svc-a is classified as a PER_ENV layer")
	assert.Equal(t, "dev", devDoc.LayerInfo.Environment,
		"the environment comes from svc-a's chain")

	// And the svc-b doc with env=prod also classifies in its OWN chain.
	prodURI := uri.File(filepath.Join(svcB, "env", "prod.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	prodDoc := srv.Documents().Get(prodURI)
	require.NotNil(t, prodDoc.LayerInfo)
	assert.True(t, prodDoc.LayerInfo.IsLayer,
		"env/prod.pxf inside svc-b is classified as a layer too")
	assert.Equal(t, "prod", prodDoc.LayerInfo.Environment,
		"the environment is svc-b's prod (not svc-a's prod, which would also exist but the routing should still pick svc-b)")
}

func TestChainStatus_StillReportsRootWhenNoRootConfig(t *testing.T) {
	// Confirm sub-workspace discovery doesn't unintentionally
	// affect the root chainStatus. The root has no chain; the
	// status is "not configured" regardless of how many sub-
	// workspaces exist.
	root, _, _ := writeMonorepoFixture(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	got := chainStatusReply(t, srv)
	assert.False(t, got.Configured,
		"root has no .protolsp.pxf at root dir → root chainStatus is 'not configured', sub-workspace state notwithstanding")
}

func TestRootWorkspace_StillWorksWhenOnlyOneConfigAtRoot(t *testing.T) {
	// Single-workspace case: no sub-projects, .protolsp.pxf at
	// root. Behavior should be identical to pre-W4.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "env"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "chain.pxf"), []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
environments = [{ name = "dev" }]
layers = [{ id = "env" path = "env/${env}.pxf" for_each = PER_ENV }]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "env", "dev.pxf"),
		[]byte("@type c4test.v1.AppConfig\n"), 0o600))

	srv, _ := initializeWithWorkspace(t, root, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(),
		"root workspace state loads at initialize (existing behavior)")
	assert.Zero(t, srv.SubWorkspaceCount(),
		"single-workspace setup populates no sub-workspaces")

	devURI := uri.File(filepath.Join(root, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	devDoc := srv.Documents().Get(devURI)
	require.NotNil(t, devDoc.LayerInfo)
	assert.True(t, devDoc.LayerInfo.IsLayer,
		"single-workspace classification still works")
	assert.Equal(t, root, devDoc.WorkspaceDir(),
		"doc inside root maps to root dir (no sub-workspace entry created)")
}

func TestLiveReload_TargetsTheRightWorkspace(t *testing.T) {
	// Edit svc-a's chain.pxf via didChange. svc-a's chain
	// reloads; svc-b's chain is untouched.
	root, svcA, svcB := writeMonorepoFixture(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	// Open env/dev.pxf in svc-a to trigger sub-workspace load.
	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	// Also open svc-b to load its state.
	prodURI := uri.File(filepath.Join(svcB, "env", "prod.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	beforeA := subWS(t, srv, svcA).Chain
	beforeB := subWS(t, srv, svcB).Chain
	require.NotNil(t, beforeA)
	require.NotNil(t, beforeB)
	require.Equal(t, beforeA.Spec.Environments[1].Name, "dev")

	// Open + edit svc-a's chain.pxf via didChange.
	chainAURI := uri.File(filepath.Join(svcA, "chain.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(chainAURI), 1, `@type chameleon.v1.ChainSpec
version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`)))

	const edited = `@type chameleon.v1.ChainSpec
version = "2.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, string(chainAURI), 2, edited)))

	// svc-a's chain reflects the bump.
	afterA := subWS(t, srv, svcA).Chain
	require.NotNil(t, afterA)
	assert.Equal(t, "2.0.0", afterA.Spec.Version,
		"svc-a chain edit propagates to svc-a's sub-workspace state")

	// svc-b's chain is unchanged (same pointer / same version).
	afterB := subWS(t, srv, svcB).Chain
	require.NotNil(t, afterB)
	assert.Equal(t, beforeB.Spec.Version, afterB.Spec.Version,
		"svc-b's chain untouched by an edit in svc-a")
}
