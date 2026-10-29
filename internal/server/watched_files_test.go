// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// initializeWithWatchSupport mirrors initializeWithWorkspace but
// declares DidChangeWatchedFiles dynamic-registration support so
// W5's registration path fires. Returns the server, fakeConn, and
// the workspace dir.
func initializeWithWatchSupport(t *testing.T, workspaceDir string) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	// Tests assert post-dispatch state in the same goroutine that
	// fired the notification, so opt out of the production
	// debounce window — that ships in production via the default
	// in Server.New but would break the synchronous-assert tests.
	srv.SetWatchedFilesWindow(0)
	params := &protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{Name: "test"},
		WorkspaceFolders: []protocol.WorkspaceFolder{
			{URI: string(uri.File(workspaceDir)), Name: "root"},
		},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				DidChangeWatchedFiles: &protocol.DidChangeWatchedFilesWorkspaceClientCapabilities{
					DynamicRegistration: true,
				},
			},
		},
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

func TestWatchedFiles_RegistrationIncludesConfigAndChainWatchers(t *testing.T) {
	workspaceDir := t.TempDir()
	_, fc := initializeWithWatchSupport(t, workspaceDir)

	calls := fc.callsByMethod(protocol.MethodClientRegisterCapability)
	require.Len(t, calls, 1, "exactly one registerCapability batch on initialized")

	var reg protocol.RegistrationParams
	require.NoError(t, json.Unmarshal(calls[0].params, &reg))

	// Find the workspace/didChangeWatchedFiles registration.
	var found *protocol.Registration
	for i := range reg.Registrations {
		if reg.Registrations[i].Method == protocol.MethodWorkspaceDidChangeWatchedFiles {
			found = &reg.Registrations[i]
			break
		}
	}
	require.NotNil(t, found, "watcher registration must be included")

	// Decode RegisterOptions back into the struct to inspect watchers.
	optsBytes, err := json.Marshal(found.RegisterOptions)
	require.NoError(t, err)
	var opts protocol.DidChangeWatchedFilesRegistrationOptions
	require.NoError(t, json.Unmarshal(optsBytes, &opts))

	patterns := make([]string, 0, len(opts.Watchers))
	for _, w := range opts.Watchers {
		patterns = append(patterns, w.GlobPattern)
	}
	assert.Contains(t, patterns, "**/.protolsp.pxf",
		"workspace config file is watched workspace-wide")
	assert.Contains(t, patterns, "**/chain.pxf",
		"defensive catch-all for the conventional chain spec name")
}

func TestWatchedFiles_NoRegistrationWhenClientDoesntSupportIt(t *testing.T) {
	// initializeWithWorkspace doesn't declare DidChangeWatchedFiles
	// capability — registration should be skipped.
	workspaceDir := t.TempDir()
	_, fc := initializeWithWorkspace(t, workspaceDir, nil)

	calls := fc.callsByMethod(protocol.MethodClientRegisterCapability)
	for _, c := range calls {
		var reg protocol.RegistrationParams
		require.NoError(t, json.Unmarshal(c.params, &reg))
		for _, r := range reg.Registrations {
			assert.NotEqual(t, protocol.MethodWorkspaceDidChangeWatchedFiles, r.Method,
				"no watcher registration when client capability is absent")
		}
	}
}

// TestWatchedFiles_DebounceCoalescesRapidEvents validates issue
// #110: back-to-back didChangeWatchedFiles notifications must
// merge into a single classify + reload + reanalyzeOpenDocs
// pass instead of N. The observable is `analyzeCount`, which
// runAnalyzePipeline bumps once per doc — with one open doc,
// the synchronous (window == 0) path reports N pipeline runs
// for N events; the coalesced (window > 0) path reports 1.
func TestWatchedFiles_DebounceCoalescesRapidEvents(t *testing.T) {
	workspaceDir := t.TempDir()
	configPath := filepath.Join(workspaceDir, ".protolsp.pxf")
	require.NoError(t, os.WriteFile(configPath, []byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "test-registry.example:443" }
}
`), 0o600))

	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	// Re-enable debouncing for this test (the helper opts out by
	// default so the rest of the suite can assert synchronously).
	srv.SetWatchedFilesWindow(50 * time.Millisecond)

	// Open a doc so reanalyzeOpenDocs has something to run.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, validPxf)))
	baseline := srv.AnalyzeCount()

	// Fire five events in rapid succession — all targeting the
	// workspace's .protolsp.pxf so each one would individually
	// trigger a reload + reanalyze.
	const eventCount = 5
	for i := 0; i < eventCount; i++ {
		require.NoError(t, srv.Handle(context.Background(), noopReply,
			fileEventCall(t, []*protocol.FileEvent{
				{URI: uri.File(configPath), Type: protocol.FileChangeTypeChanged},
			})))
	}

	// Within the synchronous path each Handle call would have
	// already bumped analyzeCount by 1 (so 5 by now); with the
	// debouncer no pipeline has fired yet — the timer is still
	// pending.
	assert.Equal(t, baseline, srv.AnalyzeCount(),
		"pipeline must not run inside the debounce window")

	// Wait for the timer to drain.
	assert.Eventually(t,
		func() bool { return srv.AnalyzeCount() > baseline },
		2*time.Second, 10*time.Millisecond,
		"debounced pipeline should fire after the window elapses")

	// And it must have fired exactly once: 5 events → 1 reanalyze
	// of the (single) open doc.
	assert.Equal(t, baseline+1, srv.AnalyzeCount(),
		"5 coalesced events must run the analyze pipeline exactly once")
}

// TestWatchedFiles_CreateDeleteCreateWithinDebounceReflectsFinalDisk
// pins the contract that the debounced reload re-reads the disk at
// fire time rather than latching onto any intermediate file state.
// Editors that thrash file contents during git operations (rebase,
// stash apply, etc.) emit create-delete-create sequences within
// milliseconds; the existing TestWatchedFiles_DebounceCoalesces-
// RapidEvents only asserts the timer fires once, not that it
// observes the final state.
func TestWatchedFiles_CreateDeleteCreateWithinDebounceReflectsFinalDisk(t *testing.T) {
	workspaceDir := t.TempDir()
	configPath := filepath.Join(workspaceDir, ".protolsp.pxf")

	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	require.Nil(t, srv.SnapshotFileConfig(), "precondition: no config at initialize")
	srv.SetWatchedFilesWindow(100 * time.Millisecond)

	// Three rapid events for the SAME path: create → delete → create.
	// All three land in the pending buffer before the timer fires.
	for _, evt := range []protocol.FileEvent{
		{URI: uri.File(configPath), Type: protocol.FileChangeTypeCreated},
		{URI: uri.File(configPath), Type: protocol.FileChangeTypeDeleted},
		{URI: uri.File(configPath), Type: protocol.FileChangeTypeCreated},
	} {
		require.NoError(t, srv.Handle(context.Background(), noopReply,
			fileEventCall(t, []*protocol.FileEvent{&evt})))
	}

	// Write the FINAL on-disk state — this is what the debounced
	// reload must observe, regardless of any intermediate file
	// snapshot the events might suggest.
	require.NoError(t, os.WriteFile(configPath, []byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "final-state.example:443" }
}
`), 0o600))

	// Within the window the timer hasn't fired yet — config still nil.
	assert.Nil(t, srv.SnapshotFileConfig(),
		"reload must not run inside the debounce window")

	// Wait for the timer to fire, then assert the reload picked up
	// the FINAL disk state. The contract being pinned: reads happen
	// at fire time, not at scheduling time.
	assert.Eventually(t, func() bool {
		cfg := srv.SnapshotFileConfig()
		return cfg != nil && cfg.Registry.Address == "final-state.example:443"
	}, 2*time.Second, 10*time.Millisecond,
		"debounced reload must observe the final disk state, not any intermediate snapshot")
}

// fileEventCall builds a workspace/didChangeWatchedFiles notification.
func fileEventCall(t *testing.T, events []*protocol.FileEvent) jsonrpc2.Request {
	t.Helper()
	notif, err := jsonrpc2.NewNotification(
		protocol.MethodWorkspaceDidChangeWatchedFiles,
		&protocol.DidChangeWatchedFilesParams{Changes: events})
	require.NoError(t, err)
	return notif
}

func TestWatchedFiles_ConfigChangeReloadsFileConfig(t *testing.T) {
	workspaceDir := t.TempDir()
	configPath := filepath.Join(workspaceDir, ".protolsp.pxf")
	// Start with no config file.
	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	require.Nil(t, srv.Workspaces().SnapshotFileConfig(), "no config file at initialize → fileConfig nil")

	// Write a config file with registry settings.
	require.NoError(t, os.WriteFile(configPath, []byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "from-watch.example:443" }
}
`), 0o600))

	// Fire a file-changed event.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(configPath), Type: protocol.FileChangeTypeCreated},
		})))

	require.NotNil(t, srv.Workspaces().SnapshotFileConfig(), "watch fired → fileConfig now loaded")
	assert.Equal(t, "from-watch.example:443", srv.Workspaces().SnapshotFileConfig().Registry.Address)
}

func TestWatchedFiles_ConfigDeleteClearsFileConfig(t *testing.T) {
	workspaceDir := t.TempDir()
	configPath := filepath.Join(workspaceDir, ".protolsp.pxf")
	require.NoError(t, os.WriteFile(configPath, []byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "to-be-removed:443" }
}
`), 0o600))

	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	require.NotNil(t, srv.Workspaces().SnapshotFileConfig(), "preconditions: config loaded at initialize")

	// Delete the file and fire a delete event.
	require.NoError(t, os.Remove(configPath))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(configPath), Type: protocol.FileChangeTypeDeleted},
		})))

	assert.Nil(t, srv.Workspaces().SnapshotFileConfig(),
		"config deletion clears fileConfig — server returns to initial state")
}

func TestWatchedFiles_ChainSpecChangeReloadsChain(t *testing.T) {
	workspaceDir := t.TempDir()
	chainPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = []
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))

	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "preconditions: chain loaded at initialize")
	assert.Equal(t, "dev", srv.Workspaces().SnapshotChain().Spec.DefaultEnvironment)

	// Rewrite the chain spec with a different default env.
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "prod"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = []
`), 0o600))

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(chainPath), Type: protocol.FileChangeTypeChanged},
		})))

	require.NotNil(t, srv.Workspaces().SnapshotChain())
	assert.Equal(t, "prod", srv.Workspaces().SnapshotChain().Spec.DefaultEnvironment,
		"chain spec was reloaded; default_environment reflects the on-disk update")
}

func TestWatchedFiles_UnrelatedPxfEventIgnored(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
schema_sources {
  registry { address = "stable.example:443" }
}
`), 0o600))

	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	before := srv.Workspaces().SnapshotFileConfig()

	// Fire an event for some unrelated .pxf file.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(filepath.Join(workspaceDir, "random.pxf")), Type: protocol.FileChangeTypeChanged},
		})))

	assert.Same(t, before, srv.Workspaces().SnapshotFileConfig(),
		"unrelated PXF events don't trigger a fileConfig reload")
}

func TestWatchedFiles_ConfigReloadRefreshesLayerInfoOnOpenDocs(t *testing.T) {
	// Open a doc that's NOT a chain layer initially (no chain
	// configured). Then add a chain config + chain spec, fire the
	// watch, and verify the doc's LayerInfo is refreshed.
	workspaceDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, "env", "dev.pxf"),
		[]byte(`@type c4test.v1.AppConfig`+"\n"), 0o600))

	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	// Open env/dev.pxf — should classify as standalone since no chain.
	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type c4test.v1.AppConfig`+"\n")))
	require.NotNil(t, srv.Documents().Get(devURI).LayerInfo)
	assert.False(t, srv.Documents().Get(devURI).LayerInfo.IsLayer,
		"preconditions: no chain → file is standalone")

	// Now write the chain config + spec, fire watch on both.
	chainPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	configPath := filepath.Join(workspaceDir, ".protolsp.pxf")
	require.NoError(t, os.WriteFile(configPath, []byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(configPath), Type: protocol.FileChangeTypeCreated},
		})))

	// After the reload, env/dev.pxf should classify as a PER_ENV layer.
	doc := srv.Documents().Get(devURI)
	require.NotNil(t, doc.LayerInfo)
	assert.True(t, doc.LayerInfo.IsLayer,
		"reloaded chain → previously standalone doc is now a layer")
	assert.Equal(t, "dev", doc.LayerInfo.Environment)
	assert.Equal(t, "PER_ENV", doc.LayerInfo.Strategy)
}

func TestWatchedFiles_BadParamsDoesNotPanic(t *testing.T) {
	srv, _ := initializeWithWatchSupport(t, t.TempDir())

	// Malformed JSON in params — handler should log + continue.
	notif, err := jsonrpc2.NewNotification(
		protocol.MethodWorkspaceDidChangeWatchedFiles, nil)
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, notif),
		"nil params is tolerated (rare client bug); no panic, no error reply")
}

func TestWatchedFiles_NonFileSchemeURIIgnored(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
schema_sources { registry { address = "x:443" } }
`), 0o600))
	srv, _ := initializeWithWatchSupport(t, workspaceDir)
	before := srv.Workspaces().SnapshotFileConfig()

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.URI("vscode-vfs://github/foo/.protolsp.pxf"), Type: protocol.FileChangeTypeChanged},
		})))

	assert.Same(t, before, srv.Workspaces().SnapshotFileConfig(),
		"non-file:// URIs are dropped — we can't read them off local disk")
}
