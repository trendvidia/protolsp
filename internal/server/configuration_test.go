// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// initializeWith runs dispatchInitialize + initialized with the given
// client capabilities and any pre-staged Call responses. Returns the
// server and fakeConn so tests can inspect state.
func initializeWith(t *testing.T, configurationSupported, dynamicRegistration bool, configResponse []any) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	if configResponse != nil {
		fc.setCallResponse(protocol.MethodWorkspaceConfiguration, configResponse)
	}

	params := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				Configuration: boolRef(configurationSupported),
				DidChangeConfiguration: &protocol.DidChangeConfigurationClientCapabilities{
					DynamicRegistration: boolRef(dynamicRegistration),
				},
			},
		},
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	// Fire `initialized` so the post-handshake pull / registration happens.
	// Handle returns immediately and the follow-up (capability registration,
	// editor-config pull, stale/deprecation notices) runs on a goroutine —
	// wait for it so assertions are deterministic.
	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	return srv, fc
}

func TestConfiguration_CapabilityFlagsCapturedAtInitialize(t *testing.T) {
	srv, _ := initializeWith(t, true, true, nil)

	assert.True(t, srv.ClientCapabilities().WorkspaceConfiguration,
		"workspace.configuration=true must be captured")
	assert.True(t, srv.ClientCapabilities().DidChangeConfigurationDynamic,
		"workspace.didChangeConfiguration.dynamicRegistration=true must be captured")
}

func TestConfiguration_PullsAfterInitializedWhenSupported(t *testing.T) {
	// Pre-stage a workspace/configuration response with full settings.
	response := []any{
		"registry.acme.example:443", // address
		"acme-billing",              // namespace
		"vault-fetched-token",       // token
		"/tmp/protolsp-cache",       // cacheDir
		nil,                         // security.inline_secrets (S4): unset → default
	}
	_, fc := initializeWith(t, true, true, response)

	calls := fc.callsByMethod(protocol.MethodWorkspaceConfiguration)
	require.Len(t, calls, 1,
		"client supports workspace/configuration → server pulls exactly once at initialized")

	// Assert the request included our expected sections.
	var params protocol.ConfigurationParams
	require.NoError(t, protocol.Unmarshal(calls[0].params, &params))
	sections := make([]string, 0, len(params.Items))
	for _, item := range params.Items {
		sections = append(sections, *item.Section)
	}
	assert.ElementsMatch(t,
		[]string{
			"protolsp.registry.address",
			"protolsp.registry.namespace",
			"protolsp.registry.token",
			"protolsp.registry.cacheDir",
			"protolsp.security.inline_secrets",
		}, sections, "all five sections are requested (registry × 4 + security × 1)")
}

func TestConfiguration_NoPullWhenClientUnsupported(t *testing.T) {
	// Client declares no workspace.configuration capability → server
	// must not send workspace/configuration. The initializationOptions
	// seed (none in this test) is the sole authority.
	_, fc := initializeWith(t, false, false, nil)

	calls := fc.callsByMethod(protocol.MethodWorkspaceConfiguration)
	assert.Empty(t, calls,
		"client lacks workspace.configuration → server doesn't pull")
}

func TestConfiguration_RegistersDynamicWhenSupported(t *testing.T) {
	_, fc := initializeWith(t, true, true, nil)

	calls := fc.callsByMethod(protocol.MethodClientRegisterCapability)
	require.Len(t, calls, 1, "dynamicRegistration=true → server registers")

	var reg protocol.RegistrationParams
	require.NoError(t, json.Unmarshal(calls[0].params, &reg))
	require.Len(t, reg.Registrations, 1)
	assert.Equal(t, protocol.MethodWorkspaceDidChangeConfiguration, reg.Registrations[0].Method,
		"registration is for workspace/didChangeConfiguration")
}

func TestConfiguration_NoRegisterWhenDynamicUnsupported(t *testing.T) {
	// configuration supported (server still pulls) but dynamicRegistration
	// not supported (server doesn't issue client/registerCapability).
	// Many clients send didChangeConfiguration unsolicited regardless.
	_, fc := initializeWith(t, true, false, nil)

	calls := fc.callsByMethod(protocol.MethodClientRegisterCapability)
	assert.Empty(t, calls,
		"dynamicRegistration=false → server skips client/registerCapability")
}

func TestConfiguration_DidChangeConfigurationRePulls(t *testing.T) {
	// Initial pull returns one set of values; the change notification
	// triggers a fresh pull; the second response wins.
	first := []any{"registry.v1:443", "ns-v1", "tok-v1", "", nil}
	srv, fc := initializeWith(t, true, true, first)

	// Stage the second response and fire the change notification.
	second := []any{"registry.v2:443", "ns-v2", "tok-v2", "", nil}
	fc.setCallResponse(protocol.MethodWorkspaceConfiguration, second)

	change, err := newTestCall(jsonrpc2.NewNumberID(3),
		protocol.MethodWorkspaceDidChangeConfiguration, struct{}{})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, change))
	// The handler returns reply() before its nested workspace/
	// configuration call to avoid deadlocking jsonrpc2's single-
	// goroutine reader, so the pull happens on a goroutine. Drain
	// it here to make the assertions below deterministic.
	srv.WaitConfigChange()

	calls := fc.callsByMethod(protocol.MethodWorkspaceConfiguration)
	assert.Len(t, calls, 2,
		"didChangeConfiguration triggers a second workspace/configuration pull")

	// After the second pull, editorConfig.Registry reflects v2 values.
	assert.Equal(t, "registry.v2:443", srv.Config().EditorConfig.Registry.Address)
	assert.Equal(t, "ns-v2", srv.Config().EditorConfig.Registry.Namespace)
	assert.Equal(t, "tok-v2", srv.Config().EditorConfig.Registry.Token)
}

func TestConfiguration_NullValuesPreserveSeed(t *testing.T) {
	// Initial pull returns nulls for everything. The seed (set via
	// initializationOptions) must survive — null is "client has no
	// opinion," not "set to empty."
	srv, fc := newServerWithFakeConn(t)
	fc.setCallResponse(protocol.MethodWorkspaceConfiguration, []any{nil, nil, nil, nil, nil})

	// Initialize with a seed that should NOT be wiped by the pull.
	initParams := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				Configuration: boolRef(true),
			},
		},
		InitializationOptions: testAny(map[string]any{
			"registry": map[string]any{
				"address":   "seed.example:443",
				"namespace": "seed-ns",
			},
		}),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, initParams)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	// `initialized` triggers the pull (all nulls).
	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	assert.Equal(t, "seed.example:443", srv.Config().EditorConfig.Registry.Address,
		"null response must NOT erase the seed")
	assert.Equal(t, "seed-ns", srv.Config().EditorConfig.Registry.Namespace)
}

// TestConfiguration_PullFailurePreservesSeed is TC4 from the
// audit roadmap: when workspace/configuration returns a
// malformed (wrong-length) response, pullEditorConfig errors
// out. dispatchInitialized logs the failure but must leave the
// seed config in place — otherwise a single bad pull silently
// disables schema validation for the session.
func TestConfiguration_PullFailurePreservesSeed(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// Stage a response with the WRONG number of values. The
	// pullEditorConfig length check rejects it (5 sections
	// expected; we send 2).
	fc.setCallResponse(protocol.MethodWorkspaceConfiguration, []any{"a", "b"})

	initParams := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				Configuration: boolRef(true),
			},
		},
		InitializationOptions: testAny(map[string]any{
			"registry": map[string]any{
				"address":   "seed.example:443",
				"namespace": "seed-ns",
			},
		}),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, initParams)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	// initialized must still complete cleanly — the failed pull is
	// non-fatal.
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	assert.Equal(t, "seed.example:443", srv.Config().EditorConfig.Registry.Address,
		"pull failure must NOT clobber the initializationOptions seed")
	assert.Equal(t, "seed-ns", srv.Config().EditorConfig.Registry.Namespace)
}

// TestConfiguration_DeprecationNudge_FiresOnSeededClientWithCapability
// pins CP5's deprecation notice: a client that seeds via
// initializationOptions AND can pull workspace/configuration gets
// a one-time window/showMessage nudge to migrate.
func TestConfiguration_DeprecationNudge_FiresOnSeededClientWithCapability(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// Pull returns the seed-equivalent values (no override) so the
	// effective config is identical pre/post pull — the nudge fires
	// based on the seed channel having been used, not on whether
	// the values were ultimately replaced.
	fc.setCallResponse(protocol.MethodWorkspaceConfiguration, []any{
		"seed.example:443", "seed-ns", "", "", nil,
	})

	params := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{Configuration: boolRef(true)},
		},
		InitializationOptions: testAny(map[string]any{
			"registry": map[string]any{
				"address":   "seed.example:443",
				"namespace": "seed-ns",
			},
		}),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	msgs := fc.shownMessages()
	require.Len(t, msgs, 1,
		"seeded init-options + workspace/configuration capability → exactly one nudge")
	assert.Equal(t, protocol.MessageTypeInfo, msgs[0].Type,
		"deprecation nudge is informational, not a warning")
	assert.Contains(t, msgs[0].Message, "initializationOptions is deprecated")
	assert.Contains(t, msgs[0].Message, "workspace/configuration",
		"message points the editor at the replacement method")
}

// TestConfiguration_DeprecationNudge_SilentWhenNoSeed pins the
// other side of the same trigger: editors that already use
// workspace/configuration from day one shouldn't see a deprecation
// nudge for a feature they're not using.
func TestConfiguration_DeprecationNudge_SilentWhenNoSeed(t *testing.T) {
	_, fc := initializeWith(t, true, true,
		[]any{"pull.example:443", "pull-ns", "", "", nil})

	for _, m := range fc.shownMessages() {
		assert.NotContains(t, m.Message, "initializationOptions",
			"no seed → no deprecation nudge")
	}
}

// TestConfiguration_DeprecationNudge_SilentWhenClientLacksCapability
// pins the migration-availability gate: a client that uses the
// seed AND can't pull workspace/configuration has no migration
// target. Nudging them would be noise.
func TestConfiguration_DeprecationNudge_SilentWhenClientLacksCapability(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	params := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		// No Workspace.Configuration capability.
		InitializationOptions: testAny(map[string]any{
			"registry": map[string]any{
				"address":   "seed.example:443",
				"namespace": "seed-ns",
			},
		}),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	for _, m := range fc.shownMessages() {
		assert.NotContains(t, m.Message, "initializationOptions",
			"no workspace/configuration capability → no deprecation nudge")
	}
}

// TestConfiguration_EmptyStringValuesPreserveSeed pins the
// configuration.go:updateString contract that empty-string values
// from the pull are treated identically to null — "client has no
// opinion, keep the seed." Without this test a future regression
// that special-cases "" as an explicit clear would silently wipe
// seeded registry values across an LSP restart, breaking schema
// validation for everyone who set the namespace through
// initializationOptions.
func TestConfiguration_EmptyStringValuesPreserveSeed(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// Pull returns empty strings for every section that the seed
	// populated. The contract: empty string == "no opinion".
	fc.setCallResponse(protocol.MethodWorkspaceConfiguration, []any{"", "", "", "", ""})

	initParams := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				Configuration: boolRef(true),
			},
		},
		InitializationOptions: testAny(map[string]any{
			"registry": map[string]any{
				"address":   "seed.example:443",
				"namespace": "seed-ns",
				"token":     "seed-token",
			},
		}),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, initParams)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	assert.Equal(t, "seed.example:443", srv.Config().EditorConfig.Registry.Address,
		"empty-string pull response must preserve seed, matching the null contract")
	assert.Equal(t, "seed-ns", srv.Config().EditorConfig.Registry.Namespace)
	assert.Equal(t, "seed-token", srv.Config().EditorConfig.Registry.Token)
}

// TestConfiguration_MixedNullAndConcreteValues_PartialOverride is
// the realistic mixed case: a pull supplies a fresh value for ONE
// section and null for the rest. The fresh value wins for that
// section; the others keep their seed. This is the everyday
// "user changed one setting" path that pxfed exercises whenever
// a single setting changes in the editor — the audit's
// "value-replacement on a per-section basis" gap.
func TestConfiguration_MixedNullAndConcreteValues_PartialOverride(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// Override only the namespace (index 1); rest null.
	fc.setCallResponse(protocol.MethodWorkspaceConfiguration, []any{nil, "override-ns", nil, nil, nil})

	initParams := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				Configuration: boolRef(true),
			},
		},
		InitializationOptions: testAny(map[string]any{
			"registry": map[string]any{
				"address":   "seed.example:443",
				"namespace": "seed-ns",
				"token":     "seed-token",
			},
		}),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, initParams)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	// Namespace is overridden; address + token survive untouched.
	assert.Equal(t, "override-ns", srv.Config().EditorConfig.Registry.Namespace,
		"concrete pull value must override the matching seed section")
	assert.Equal(t, "seed.example:443", srv.Config().EditorConfig.Registry.Address,
		"null sections in the pull must NOT touch the rest of the seed")
	assert.Equal(t, "seed-token", srv.Config().EditorConfig.Registry.Token)
}

func TestConfiguration_PullWinsOverSeed(t *testing.T) {
	// Initial pull returns concrete values for fields that were also
	// in the seed. The pull's values win.
	srv, fc := newServerWithFakeConn(t)
	fc.setCallResponse(protocol.MethodWorkspaceConfiguration, []any{
		"pull.example:443", "pull-ns", "pull-token", "", nil,
	})

	initParams := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{Configuration: boolRef(true)},
		},
		InitializationOptions: testAny(map[string]any{
			"registry": map[string]any{
				"address":   "seed.example:443",
				"namespace": "seed-ns",
			},
		}),
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, initParams)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))

	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()

	assert.Equal(t, "pull.example:443", srv.Config().EditorConfig.Registry.Address,
		"workspace/configuration return value overrides the seed")
	assert.Equal(t, "pull-ns", srv.Config().EditorConfig.Registry.Namespace)
	assert.Equal(t, "pull-token", srv.Config().EditorConfig.Registry.Token,
		"pull adds fields the seed didn't have")
}
