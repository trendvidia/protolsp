// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/trendvidia/protolsp/schema"
)

// stubLookup is a no-op schema.Lookup with an identifying tag so
// tests can assert which lookup was used. Mirrors instrumentedLookup
// in lookup_rebuild_test.go but skips the close-tracking.
type stubLookup struct {
	tag string
}

func (l *stubLookup) FindMessageByName(_ protoreflect.FullName) (protoreflect.MessageType, error) {
	return nil, protoregistry.NotFound
}
func (l *stubLookup) RangeMessages(_ func(protoreflect.MessageType) bool) {}
func (l *stubLookup) FindFileByPathWithOrigin(_ string) (protoreflect.FileDescriptor, string, error) {
	return nil, "", protoregistry.NotFound
}
func (l *stubLookup) GetSource(_ context.Context, _ string) ([]byte, error) {
	return nil, protoregistry.NotFound
}
func (l *stubLookup) IsStale() bool { return false }

var _ schema.Lookup = (*stubLookup)(nil)

// writeMonorepoWithDifferentRegistries sets up a workspace
// where svc-a and svc-b declare DIFFERENT registry addresses.
// Returns the workspace root and the two sub-workspace dirs.
func writeMonorepoWithDifferentRegistries(t *testing.T) (root, svcA, svcB string) {
	t.Helper()
	root = t.TempDir()
	svcA = filepath.Join(root, "svc-a")
	svcB = filepath.Join(root, "svc-b")
	require.NoError(t, os.MkdirAll(filepath.Join(svcA, "env"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(svcB, "env"), 0o755))

	// svc-a declares its own registry — would dial registry-a:443.
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
registry { address = "registry-a.invalid:443" namespace = "team-a" }
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, "env", "dev.pxf"),
		[]byte("@type c4test.v1.AppConfig\n"), 0o600))

	// svc-b declares a DIFFERENT registry.
	require.NoError(t, os.WriteFile(
		filepath.Join(svcB, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
registry { address = "registry-b.invalid:443" namespace = "team-b" }
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcB, "env", "prod.pxf"),
		[]byte("@type c4test.v1.AppConfig\n"), 0o600))

	return root, svcA, svcB
}

func TestSubWorkspace_DialsItsOwnRegistry(t *testing.T) {
	// Each sub-workspace with its own `registry` block triggers
	// a lookup dial scoped to that workspace. The registry
	// addresses are invalid so the dials fail — but the
	// rebuild bookkeeping still records that we tried.
	root, svcA, svcB := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	prodURI := uri.File(filepath.Join(svcB, "env", "prod.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	swA := subWS(t, srv, svcA)
	swB := subWS(t, srv, svcB)
	require.NotNil(t, swA)
	require.NotNil(t, swB)
	assert.Equal(t, "registry-a.invalid:443", swA.LastRebuiltConfig().Address,
		"svc-a's recorded effective config is svc-a's registry")
	assert.Equal(t, "registry-b.invalid:443", swB.LastRebuiltConfig().Address,
		"svc-b's recorded effective config is svc-b's registry")
}

func TestSubWorkspace_LookupForDocRoutesByWorkspace(t *testing.T) {
	// Manually install fake lookups on each sub-workspace and
	// confirm lookupForDoc picks the matching one.
	root, svcA, svcB := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	prodURI := uri.File(filepath.Join(svcB, "env", "prod.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	// Install distinct lookups on each sub-workspace.
	lookupA := &stubLookup{tag: "svc-a"}
	lookupB := &stubLookup{tag: "svc-b"}
	subWS(t, srv, svcA).Lookup = lookupA
	subWS(t, srv, svcB).Lookup = lookupB

	devDoc := srv.Documents().Get(devURI)
	prodDoc := srv.Documents().Get(prodURI)
	assert.Equal(t, schema.Lookup(lookupA), srv.LookupForDoc(devDoc),
		"svc-a doc routes to svc-a's lookup")
	assert.Equal(t, schema.Lookup(lookupB), srv.LookupForDoc(prodDoc),
		"svc-b doc routes to svc-b's lookup")
}

func TestSubWorkspace_NoOwnRegistry_FallsThroughToRoot(t *testing.T) {
	// Sub-workspace's .protolsp.pxf has no schema_sources block;
	// LookupForDoc falls through to the root's lookup.
	root := t.TempDir()
	svc := filepath.Join(root, "svc")
	require.NoError(t, os.MkdirAll(filepath.Join(svc, "env"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(svc, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svc, "chain.pxf"), []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
environments = [{ name = "dev" }]
layers = []
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svc, "env", "dev.pxf"),
		[]byte("@type c4test.v1.AppConfig\n"), 0o600))

	srv, _ := initializeWithWorkspace(t, root, nil)
	rootLookup := &stubLookup{tag: "root"}
	srv.Lookups().Swap(rootLookup, nil)

	devURI := uri.File(filepath.Join(svc, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	devDoc := srv.Documents().Get(devURI)
	require.NotNil(t, subWS(t, srv, svc),
		"sub-workspace state was loaded for chain even without own registry")
	assert.Nil(t, subWS(t, srv, svc).Lookup,
		"sub-workspace without schema_sources doesn't dial its own lookup")
	assert.Equal(t, schema.Lookup(rootLookup), srv.LookupForDoc(devDoc),
		"sub-workspaces without own registry fall through to root lookup")
}

func TestSubWorkspace_RebuildIndependentFromRoot(t *testing.T) {
	// Editing svc-a's .protolsp.pxf (via didChange) reloads
	// svc-a's sub-workspace — including the lookup field —
	// without touching svc-b or the root.
	root, svcA, svcB := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	prodURI := uri.File(filepath.Join(svcB, "env", "prod.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	swBBefore := subWS(t, srv, svcB)
	require.NotNil(t, swBBefore)
	addrBBefore := swBBefore.LastRebuiltConfig().Address

	// Edit svc-a's .protolsp.pxf via didChange — switching to
	// yet another registry.
	configA := uri.File(filepath.Join(svcA, ".protolsp.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(configA), 1, `@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "registry-a.invalid:443" namespace = "team-a" }
}
`)))
	const edited = `@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "registry-a-v2.invalid:443" namespace = "team-a-v2" }
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, string(configA), 2, edited)))

	swA := subWS(t, srv, svcA)
	require.NotNil(t, swA)
	// Allow async rebuild to complete the diff-track. Even if the
	// dial fails (invalid registry), the last-rebuilt config should record
	// the new effective config.
	assert.Eventually(t, func() bool {
		return swA.LastRebuiltConfig().Address == "registry-a-v2.invalid:443"
	}, 200*time.Millisecond, 5*time.Millisecond,
		"svc-a's recorded config reflects the edit")

	swBAfter := subWS(t, srv, svcB)
	require.NotNil(t, swBAfter)
	assert.Equal(t, addrBBefore, swBAfter.LastRebuiltConfig().Address,
		"svc-b's recorded config is unchanged by an edit in svc-a")
}

func TestWatchedFiles_SubWorkspaceConfigChangeReloads(t *testing.T) {
	// W5 watcher fires for a sub-workspace's .protolsp.pxf;
	// the classifier routes the event to that sub-workspace
	// and reloadSubWorkspace runs.
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWatchSupport(t, root)

	// Trigger sub-workspace discovery first.
	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	subWS(t, srv, svcA)

	// Rewrite svc-a's config on disk + fire a watcher event.
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "registry-a-v2.invalid:443" namespace = "team-a-v2" }
}
`), 0o600))
	configURI := uri.File(filepath.Join(svcA, ".protolsp.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: configURI, Type: protocol.FileChangeTypeChanged},
		})))

	// Sub-workspace's FileConfig reflects the disk update.
	swA := subWS(t, srv, svcA)
	require.NotNil(t, swA)
	require.NotNil(t, swA.FileConfig)
	assert.Equal(t, "registry-a-v2.invalid:443", swA.FileConfig.Registry.Address,
		"watched-files event triggers reload of the affected sub-workspace")
}

// TestWatchedFiles_SubWorkspaceChainSpecChangeReloads is TC2
// from the audit roadmap: subWorkspaceDirForChainPath had a
// test for the negative branch (path doesn't match any
// sub-workspace's chain) but the positive branch — chain.pxf
// inside a sub-workspace gets routed to that sub-workspace —
// was unexercised. A regression in the routing would have
// silently dropped chain edits on sub-workspaces.
func TestWatchedFiles_SubWorkspaceChainSpecChangeReloads(t *testing.T) {
	root := t.TempDir()
	svcA := filepath.Join(root, "svc-a")
	require.NoError(t, os.MkdirAll(filepath.Join(svcA, "env"), 0o755))

	// svc-a has a chameleon chain spec.
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, ".protolsp.pxf"), []byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	chainPath := filepath.Join(svcA, "chain.pxf")
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [ { name = "dev" } ]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(svcA, "env", "dev.pxf"),
		[]byte("@type c4test.v1.AppConfig\n"), 0o600))

	srv, _ := initializeWithWatchSupport(t, root)

	// didOpen on a doc inside svc-a triggers sub-workspace discovery.
	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	swA := subWS(t, srv, svcA)
	require.NotNil(t, swA, "preconditions: sub-workspace loaded")
	require.NotNil(t, swA.Chain, "preconditions: chain spec loaded")
	require.Equal(t, "dev", swA.Chain.Spec.DefaultEnvironment)

	// Rewrite the chain.pxf with a different default environment +
	// fire a watcher event. The classifier must route this to svcA's
	// reload (positive case of subWorkspaceDirForChainPath).
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "prod"
environments = [ { name = "prod" }, { name = "dev" } ]
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(chainPath), Type: protocol.FileChangeTypeChanged},
		})))

	swA = subWS(t, srv, svcA)
	require.NotNil(t, swA)
	require.NotNil(t, swA.Chain)
	assert.Equal(t, "prod", swA.Chain.Spec.DefaultEnvironment,
		"chain.pxf watcher event routes to the owning sub-workspace and reloads")
}

// TestWatchedFiles_SubWorkspaceConfigDeleteClearsFileConfig is
// TC3: the root-workspace delete path (#TestWatchedFiles_Config
// DeleteClearsFileConfig) has been covered since W5; the
// sub-workspace equivalent — deleting svc-a/.protolsp.pxf —
// was not. Without coverage, a regression could leak a stale
// sub-workspace state through a re-load that doesn't notice
// the file is gone.
func TestWatchedFiles_SubWorkspaceConfigDeleteClearsFileConfig(t *testing.T) {
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWatchSupport(t, root)

	// Discover the sub-workspace.
	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	swA := subWS(t, srv, svcA)
	require.NotNil(t, swA)
	require.NotNil(t, swA.FileConfig, "preconditions: sub-workspace config loaded")

	// Delete + fire the watcher.
	configPath := filepath.Join(svcA, ".protolsp.pxf")
	require.NoError(t, os.Remove(configPath))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(configPath), Type: protocol.FileChangeTypeDeleted},
		})))

	// Sub-workspace stays in the map (no auto-evict on watcher
	// delete — eviction is reference-counted, not file-presence-
	// driven) but its FileConfig clears so chain-aware features
	// don't keep resolving against a deleted file.
	swA = subWS(t, srv, svcA)
	require.NotNil(t, swA, "sub-workspace entry persists; only the parsed config goes nil")
	assert.Nil(t, swA.FileConfig,
		"deleted .protolsp.pxf clears the sub-workspace's FileConfig")
}

func TestSubWorkspace_Close_TearsDownAllLookups(t *testing.T) {
	// Server.Close cascades teardown to every loaded sub-workspace:
	// each sub-workspace's lookup closer runs and any in-flight
	// rebuild gets cancelled. Previously a known leak (pinned with
	// `assert.False(stub.closed)`); now plugged.
	root, svcA, svcB := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	prodURI := uri.File(filepath.Join(svcB, "env", "prod.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	// Install stub closers on each sub-workspace so we can detect
	// the cascade.
	stubA := &trackedCloser{}
	stubB := &trackedCloser{}
	srv.SwapSubWorkspaceLookup(subWS(t, srv, svcA), &stubLookup{tag: "a"}, stubA)
	srv.SwapSubWorkspaceLookup(subWS(t, srv, svcB), &stubLookup{tag: "b"}, stubB)

	require.NoError(t, srv.Close())
	assert.True(t, stubA.closed.Load(),
		"Close cascades teardown to svc-a's sub-workspace lookup")
	assert.True(t, stubB.closed.Load(),
		"Close cascades teardown to svc-b's sub-workspace lookup")
}

func TestSubWorkspace_Close_IsIdempotent(t *testing.T) {
	// Calling Close twice doesn't double-close or panic. Matters
	// for transports that call Close on both shutdown notification
	// AND connection EOF.
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)
	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	stub := &trackedCloser{}
	srv.SwapSubWorkspaceLookup(subWS(t, srv, svcA), &stubLookup{tag: "x"}, stub)

	require.NoError(t, srv.Close())
	require.NoError(t, srv.Close(),
		"a second Close is a no-op — no panic, no double-free")
}

func TestSubWorkspace_Close_CancelsInFlightRebuild(t *testing.T) {
	// A sub-workspace mid-dial when Close fires: the rebuild
	// goroutine sees ctx.Err() after schema.Open returns and skips
	// its install. Confirm the in-flight rebuild is cleared on Close so the
	// cancel func can't accidentally fire again later.
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))

	// loadSubWorkspaceState fired rebuildLookupAsyncForWorkspace,
	// which marked a rebuild in flight. Close should cancel + clear it.
	ws := subWS(t, srv, svcA)
	require.NotNil(t, ws)
	require.NoError(t, srv.Close())
	assert.False(t, ws.RebuildInFlight(),
		"Close cancels + clears the sub-workspace's in-flight rebuild")
}

// trackedCloser records whether Close was called.
type trackedCloser struct {
	closed boolFlag
}

func (c *trackedCloser) Close() error {
	c.closed.Store(true)
	return nil
}

var _ io.Closer = (*trackedCloser)(nil)

// boolFlag wraps an atomic.Bool with a tiny API so the test
// doesn't pull sync/atomic at the top level just for this.
type boolFlag struct {
	stored bool
}

func (b *boolFlag) Store(v bool) { b.stored = v }
func (b *boolFlag) Load() bool   { return b.stored }
