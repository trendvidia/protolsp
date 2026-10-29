// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/schema"
)

// lookupChangedRecorder records each LookupChanged fire's
// workspaceDir; every other push is discarded via the embedded
// NopPublisher.
type lookupChangedRecorder struct {
	NopPublisher
	mu   sync.Mutex
	dirs []string
}

func (r *lookupChangedRecorder) LookupChanged(_ context.Context, workspaceDir string) {
	r.mu.Lock()
	r.dirs = append(r.dirs, workspaceDir)
	r.mu.Unlock()
}

func (r *lookupChangedRecorder) fired() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dirs...)
}

func TestLookupChanged_FiresOnProtoFilesInstallAndTeardown(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".protolsp.pxf"),
		[]byte("@type protolsp.v1.WorkspaceConfig\n\nproto_files {\n  paths = [\"proto/\"]\n}\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "proto", "myapp", "v1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "proto", "myapp", "v1", "greeting.proto"),
		[]byte("syntax = \"proto3\";\npackage myapp.v1;\nmessage Greeting { string name = 1; }\n"), 0o600))

	w := New(nil)
	rec := &lookupChangedRecorder{}
	w.SetPublisher(rec)
	w.ws.SetWorkspaceRoot(uri.File(dir))
	w.SetBufLint(false)

	w.LoadWorkspaceFileConfig()
	w.RebuildRootProtoFilesLookup()
	assert.Equal(t, []string{""}, rec.fired(),
		"proto_files install is a root-lookup transition — Snapshot's W2 fallback changed")

	require.NoError(t, w.Lookups().Close())
	assert.Equal(t, []string{"", ""}, rec.fired(),
		"teardown fires once: the proto_files slot was installed, the registry slot's nil→nil swap is silent")
}

func TestLookupChanged_SilentWhenNothingWasInstalled(t *testing.T) {
	w := New(nil)
	rec := &lookupChangedRecorder{}
	w.SetPublisher(rec)

	require.NoError(t, w.Lookups().Close())
	w.RebuildLookupAsync(context.Background())
	assert.Empty(t, rec.fired(),
		"nil→nil swaps and the no-op rebuild diff path are not transitions")
}

func TestLookupChanged_FiresOnInstallAndConfigClearedTeardown(t *testing.T) {
	w := New(nil)
	rec := &lookupChangedRecorder{}
	w.SetPublisher(rec)

	// Direct install — the same Swap the shell's initialize-time
	// configureSchemaLookup path lands on.
	w.Lookups().Swap(schema.NewInternalLookup(), nil)
	assert.Equal(t, []string{""}, rec.fired(), "install fires for the root")

	// Simulate "registry config removed": baseline says a registry
	// was built, effective config now resolves empty → the
	// synchronous teardown branch of RebuildLookupAsync.
	w.Lookups().RecordBaseline(schema.Config{Address: "registry.example:443", Namespace: "acme"})
	w.RebuildLookupAsync(context.Background())
	assert.Equal(t, []string{"", ""}, rec.fired(), "config-cleared teardown fires")
	assert.Nil(t, w.Lookups().Snapshot())
}

func TestLookupChanged_FiresOnFailedRebuildDial(t *testing.T) {
	// Address without namespace fails schema.Open's config
	// validation before any network I/O — a deterministic stand-in
	// for an unreachable registry. The previous (nil) state is
	// kept, but the transition must still fire so a status surface
	// waiting on the rebuild knows the attempt resolved.
	w := New(nil)
	rec := &lookupChangedRecorder{}
	w.SetPublisher(rec)

	w.dialAndInstallLookup(context.Background(), schema.Config{Address: "registry.example:443"})
	assert.Equal(t, []string{""}, rec.fired(), "failed dial fires without a state change")
	assert.Nil(t, w.Lookups().Snapshot(), "failed dial installs nothing")
}

func TestLookupChanged_SubWorkspaceSwapCarriesDir(t *testing.T) {
	// Sub-workspace slots don't route through LookupManager;
	// the swap helpers fire with the workspace dir so per-workspace
	// status surfaces can refresh selectively.
	w := New(nil)
	rec := &lookupChangedRecorder{}
	w.SetPublisher(rec)
	ws := &WorkspaceState{Dir: "/repo/svc-a"}

	w.SwapSubWorkspaceLookup(ws, schema.NewInternalLookup(), nil)
	assert.Equal(t, []string{"/repo/svc-a"}, rec.fired(), "sub-workspace install fires with its dir")

	w.SwapSubWorkspaceLookup(ws, nil, nil)
	assert.Equal(t, []string{"/repo/svc-a", "/repo/svc-a"}, rec.fired(), "teardown fires")

	// A sub-workspace that never declared its own schema source
	// tears down silently — the nil→nil rule matches the root's.
	w.tearDownSubWorkspace(&WorkspaceState{Dir: "/repo/svc-b"})
	assert.Len(t, rec.fired(), 2, "nil→nil teardown of an empty sub-workspace is silent")
}

func TestLookupChanged_SubWorkspaceFailedDialCarriesDir(t *testing.T) {
	// Same validation-failure stand-in as the root test: fires with
	// the workspace dir, installs nothing.
	w := New(nil)
	rec := &lookupChangedRecorder{}
	w.SetPublisher(rec)
	ws := &WorkspaceState{Dir: "/repo/svc-a"}

	w.dialAndInstallSubWorkspaceLookup(context.Background(), ws, schema.Config{Address: "registry.example:443"})
	assert.Equal(t, []string{"/repo/svc-a"}, rec.fired(), "sub-workspace failed dial fires with its dir")
	assert.Nil(t, w.snapshotSubWorkspaceLookup(ws), "failed dial installs nothing")
}
