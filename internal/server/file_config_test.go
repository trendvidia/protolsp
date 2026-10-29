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

	"github.com/trendvidia/protolsp/analysis"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const wsConfigRegistryOnly = `@type protolsp.v1.WorkspaceConfig

schema_sources {
  registry {
    address    = "file-registry.example:443"
    namespace  = "file-namespace"
    cache_dir  = "/var/cache/from-file"
  }
}
`

// initializeWithWorkspace runs initialize + initialized with a
// concrete workspace root pointing at the given tmp dir. Sets up
// the same workspace+capabilities scaffold initializeWith provides.
func initializeWithWorkspace(t *testing.T, workspaceDir string, initOpts map[string]any) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)

	params := &protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{Name: "test"},
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{URI: string(uri.File(workspaceDir)), Name: "root"},
		},
		InitializationOptions: initOpts,
	}
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))

	initCall, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	return srv, fc
}

func TestFileConfig_LoadedFromWorkspaceRoot(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigRegistryOnly), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	require.NotNil(t, srv.Workspaces().SnapshotFileConfig(), ".protolsp.pxf at workspace root must be loaded")
	assert.Equal(t, "file-registry.example:443", srv.Workspaces().SnapshotFileConfig().Registry.Address)
	assert.Equal(t, "file-namespace", srv.Workspaces().SnapshotFileConfig().Registry.Namespace)
}

func TestFileConfig_AbsentFileIsSilent(t *testing.T) {
	// No .protolsp.pxf at the workspace root → fileConfig stays nil.
	// Existing initOptions behavior is unchanged.
	workspaceDir := t.TempDir()

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	assert.Nil(t, srv.Workspaces().SnapshotFileConfig(), "missing .protolsp.pxf is not an error")
}

func TestFileConfig_FileValuesSurfaceAsTier2InGetEffectiveConfig(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigRegistryOnly), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.address", "protolsp.registry.namespace"},
	})
	addr := got.Values["protolsp.registry.address"]
	assert.Equal(t, "file-registry.example:443", addr.Value)
	assert.Equal(t, SourceFile, addr.Source,
		"values from .protolsp.pxf surface with Source=file")

	ns := got.Values["protolsp.registry.namespace"]
	assert.Equal(t, "file-namespace", ns.Value)
	assert.Equal(t, SourceFile, ns.Source)
}

func TestFileConfig_SettingsBeatFile(t *testing.T) {
	// File and initializationOptions both set address. Tier 3 wins.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigRegistryOnly), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, map[string]any{
		"registry": map[string]any{
			"address":   "settings-wins.example:443",
			"namespace": "settings-ns",
		},
	})

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.address", "protolsp.registry.namespace"},
	})
	addr := got.Values["protolsp.registry.address"]
	assert.Equal(t, "settings-wins.example:443", addr.Value)
	assert.Equal(t, SourceSettings, addr.Source,
		"settings beat file when both set")

	ns := got.Values["protolsp.registry.namespace"]
	assert.Equal(t, "settings-ns", ns.Value)
	assert.Equal(t, SourceSettings, ns.Source)
}

func TestFileConfig_FileFillsGapsLeftBySettings(t *testing.T) {
	// Settings sets address; file sets cache_dir. Effective config
	// gets address from settings, cache_dir from file.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigRegistryOnly), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, map[string]any{
		"registry": map[string]any{
			"address": "settings-only.example:443",
		},
	})

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{
			"protolsp.registry.address",
			"protolsp.registry.cacheDir",
		},
	})
	addr := got.Values["protolsp.registry.address"]
	assert.Equal(t, "settings-only.example:443", addr.Value)
	assert.Equal(t, SourceSettings, addr.Source)

	cache := got.Values["protolsp.registry.cacheDir"]
	assert.Equal(t, "/var/cache/from-file", cache.Value)
	assert.Equal(t, SourceFile, cache.Source,
		"file fills the gap where settings is silent")
}

func TestFileConfig_OverrideBeatsBothFileAndSettings(t *testing.T) {
	// Set address in file. Set address in settings. Push an override.
	// Override wins.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(wsConfigRegistryOnly), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, map[string]any{
		"registry": map[string]any{"address": "settings.example:443"},
	})

	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.registry.address": "override.example:443",
		},
	})

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.address"},
	})
	addr := got.Values["protolsp.registry.address"]
	assert.Equal(t, "override.example:443", addr.Value)
	assert.Equal(t, SourceOverride, addr.Source,
		"Tier 4 override beats Tier 3 settings beats Tier 2 file")
}

func TestFileConfig_MalformedFileIsLoggedButNonFatal(t *testing.T) {
	// Unparseable PXF — fileConfig stays nil. Server continues to
	// work with editor settings only.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte("not @ valid { pxf"), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, map[string]any{
		"registry": map[string]any{"address": "still-works:443"},
	})

	assert.Nil(t, srv.Workspaces().SnapshotFileConfig(), "unparseable file leaves fileConfig nil")
	assert.Equal(t, "still-works:443", srv.Config().EditorConfig.Registry.Address,
		"editor settings still apply when file is broken")
}

func TestFileConfig_NoWorkspaceRootIsSilent(t *testing.T) {
	// Initialize without WorkspaceFolders or RootURI → no path to
	// look at. fileConfig stays nil; no warnings.
	srv, fc := newServerWithFakeConn(t)

	params := &protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{Name: "test"},
	}
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, call))

	assert.Nil(t, srv.Workspaces().SnapshotFileConfig())
	assert.Empty(t, fc.shownMessages(), "no workspace root + no file → silent")
}

func TestFileConfig_EmptyFileIsHandledCleanly(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"), []byte(""), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	// Empty file means HasContent=false → fileConfig stays nil.
	assert.Nil(t, srv.Workspaces().SnapshotFileConfig(),
		"empty .protolsp.pxf is treated as no file (HasContent=false)")
}

// Make sure the existing CP2 testing helper finds what it needs
// inside this file's tests (it uses json.Marshal on result, etc.).
var _ = json.Marshal

// subWS fetches a loaded sub-workspace through the sanctioned
// accessor, failing the test when the dir isn't loaded.
func subWS(t *testing.T, srv *Server, dir string) *analysis.WorkspaceState {
	t.Helper()
	ws, ok := srv.SubWorkspaceByDir(dir)
	require.True(t, ok, "sub-workspace %s not loaded", dir)
	return ws
}
