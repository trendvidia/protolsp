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
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// findSecuritySource returns the diagnostic whose Source is
// "protolsp/security", or nil if none was emitted.
func findSecuritySource(diags []protocol.Diagnostic) *protocol.Diagnostic {
	for i := range diags {
		if optValue(diags[i].Source) == "protolsp/security" {
			return &diags[i]
		}
	}
	return nil
}

func TestPolicy_DefaultIsWarn(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	d := findSecuritySource(notes[0].params.Diagnostics)
	require.NotNil(t, d, "default policy fires the security diagnostic")
	assert.Equal(t, protocol.DiagnosticSeverityWarning, d.Severity,
		"default severity is Warning")
}

func TestPolicy_OverrideToAllowSuppressesDiagnostic(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)

	// Push the override BEFORE opening the document so analyze sees
	// the right policy on first emit.
	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.security.inline_secrets": "allow",
		},
	})

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Nil(t, findSecuritySource(notes[0].params.Diagnostics),
		"allow policy suppresses the literal-secret diagnostic entirely")
}

func TestPolicy_OverrideToRefuseUpgradeSeverity(t *testing.T) {
	srv, fc, _ := newServerWithCredentialsType(t)

	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.security.inline_secrets": "refuse",
		},
	})

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	d := findSecuritySource(notes[0].params.Diagnostics)
	require.NotNil(t, d)
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity,
		"refuse upgrades the diagnostic to Error severity")
}

func TestPolicy_OverrideValidatedAsEnum(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	// Non-enum string → InvalidParams.
	_, errResp := setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.security.inline_secrets": "maybe",
		},
	})
	require.NotNil(t, errResp)
	assert.Equal(t, jsonrpc2.InvalidParams, errResp.Code,
		"non-enum value rejected at push time")
}

func TestPolicy_FromWorkspaceFile(t *testing.T) {
	// .protolsp.pxf at workspace root carries the policy; analyze
	// sees Tier 2 = refuse and upgrades the diagnostic.
	workspaceDir := t.TempDir()
	const cfgSrc = `@type protolsp.v1.WorkspaceConfig

security {
  inline_secrets = "refuse"
}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(cfgSrc), 0o600))

	srv, fc := initializeWithWorkspace(t, workspaceDir, nil)
	// Manually install the Credentials lookup since
	// initializeWithWorkspace doesn't.
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	d := findSecuritySource(notes[0].params.Diagnostics)
	require.NotNil(t, d, "policy from .protolsp.pxf is consulted")
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity,
		"refuse from file upgrades severity")
}

func TestPolicy_SettingsBeatsFile(t *testing.T) {
	// File says refuse; editor settings say allow. Settings win
	// (Tier 3 > Tier 2 per config-protocol).
	workspaceDir := t.TempDir()
	const cfgSrc = `@type protolsp.v1.WorkspaceConfig

security { inline_secrets = "refuse" }
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(cfgSrc), 0o600))

	srv, fc := initializeWithWorkspace(t, workspaceDir, nil)
	srv.Config().EditorConfig.InlineSecrets = "allow"
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Nil(t, findSecuritySource(notes[0].params.Diagnostics),
		"Tier 3 (settings=allow) beats Tier 2 (file=refuse)")
}

func TestPolicy_OverrideBeatsAll(t *testing.T) {
	// Tier 4 (override=refuse) beats Tier 3 (settings=allow)
	// beats Tier 2 (file=warn).
	workspaceDir := t.TempDir()
	const cfgSrc = `@type protolsp.v1.WorkspaceConfig

security { inline_secrets = "warn" }
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(cfgSrc), 0o600))

	srv, fc := initializeWithWorkspace(t, workspaceDir, nil)
	srv.Config().EditorConfig.InlineSecrets = "allow"
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)

	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.security.inline_secrets": "refuse",
		},
	})

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	d := findSecuritySource(notes[0].params.Diagnostics)
	require.NotNil(t, d)
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity,
		"Tier 4 (override=refuse) wins over Tiers 2 (file=warn) + 3 (settings=allow)")
}

func TestPolicy_EffectiveValueReturnedByGetEffectiveConfig(t *testing.T) {
	// getEffectiveConfig surfaces the resolved policy with source.
	srv, _, _ := newServerWithCredentialsType(t)
	srv.Config().EditorConfig.InlineSecrets = "refuse"

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.security.inline_secrets"},
	})
	v := got.Values["protolsp.security.inline_secrets"]
	assert.Equal(t, "refuse", v.Value)
	assert.Equal(t, SourceSettings, v.Source)
}
