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

	"github.com/trendvidia/protolsp/schema"
)

// effectiveConfigReply drives MethodGetEffectiveConfig and decodes
// the result.
func effectiveConfigReply(t *testing.T, srv *Server, params GetEffectiveConfigParams) GetEffectiveConfigResult {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodGetEffectiveConfig, params)
	require.NoError(t, err)

	var got GetEffectiveConfigResult
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

func TestEffectiveConfig_AllKnownSectionsReturnedByDefault(t *testing.T) {
	// No params → response contains every known section.
	srv, _ := newServerWithFakeConn(t)
	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{})

	want := []string{
		"protolsp.registry.address",
		"protolsp.registry.namespace",
		"protolsp.registry.token",
		"protolsp.registry.cacheDir",
		"protolsp.security.inline_secrets",
	}
	keys := make([]string, 0, len(got.Values))
	for k := range got.Values {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, want, keys,
		"empty Sections → all known sections returned")
}

func TestEffectiveConfig_SettingsValuesReportSourceSettings(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{
		Address:   "registry.example:443",
		Namespace: "acme-billing",
	}

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.address", "protolsp.registry.namespace"},
	})

	addr := got.Values["protolsp.registry.address"]
	assert.Equal(t, "registry.example:443", addr.Value)
	assert.Equal(t, SourceSettings, addr.Source,
		"non-empty editorConfig values surface as Source=settings")
	assert.Equal(t, "protolsp.registry.address", addr.Origin)

	ns := got.Values["protolsp.registry.namespace"]
	assert.Equal(t, "acme-billing", ns.Value)
	assert.Equal(t, SourceSettings, ns.Source)
}

func TestEffectiveConfig_EmptySettingsFallBackToDefault(t *testing.T) {
	// No editorConfig at all → every value comes from the default
	// tier. Most sections default to empty; inline_secrets defaults
	// to "warn" per the secret-handling design. The per-section
	// defaults themselves are pinned in-package by the analysis
	// tests (TestDefaultFor_SectionDefaults); here we only assert
	// they surface through the RPC unchanged.
	srv, _ := newServerWithFakeConn(t)

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{})

	for section, v := range got.Values {
		assert.Equal(t, SourceDefault, v.Source,
			"%s with no settings/file/override → default tier", section)
		want := ""
		if section == sectionSecurityInlineSecrets {
			want = "warn"
		}
		assert.Equal(t, want, v.Value, "default for %s", section)
	}
}

func TestEffectiveConfig_TokenRedactedByDefault(t *testing.T) {
	// Settings have a token. The default response redacts it.
	srv, _ := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{Token: "super-secret"}

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.token"},
	})
	tok := got.Values["protolsp.registry.token"]
	assert.Equal(t, "<redacted>", tok.Value,
		"token value redacted when revealSecrets=false (default)")
	assert.Equal(t, SourceSettings, tok.Source,
		"provenance preserved through redaction — the source tier is still settings")
}

func TestEffectiveConfig_RevealSecretsBypassesRedaction(t *testing.T) {
	// Explicit revealSecrets=true returns the real value. Editors call
	// this after a deliberate user action ("Show token").
	srv, _ := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{Token: "super-secret"}

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections:      []string{"protolsp.registry.token"},
		RevealSecrets: true,
	})
	tok := got.Values["protolsp.registry.token"]
	assert.Equal(t, "super-secret", tok.Value,
		"revealSecrets=true returns the unredacted value")
}

func TestEffectiveConfig_EmptyTokenNotRedacted(t *testing.T) {
	// Redaction shouldn't replace an empty default with <redacted>;
	// that would mislead users into thinking a token is configured.
	srv, _ := newServerWithFakeConn(t)
	// No token set; editorConfig is zero.

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.token"},
	})
	tok := got.Values["protolsp.registry.token"]
	assert.Equal(t, "", tok.Value, "empty value passes through unredacted")
	assert.Equal(t, SourceDefault, tok.Source)
}

func TestEffectiveConfig_NonSensitiveSectionsNotRedacted(t *testing.T) {
	// Only token/password/secret sections should redact. Address etc.
	// are returned plain regardless of revealSecrets.
	srv, _ := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{
		Address: "still-visible.example:443",
	}

	got := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.address"},
	})
	addr := got.Values["protolsp.registry.address"]
	assert.Equal(t, "still-visible.example:443", addr.Value,
		"non-sensitive sections are never redacted")
}

func TestEffectiveConfig_NilParamsTolerated(t *testing.T) {
	// JSON-RPC params can be null/missing. The handler treats both as
	// "default request" (all sections, no reveal).
	srv, _ := newServerWithFakeConn(t)

	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodGetEffectiveConfig, nil)
	require.NoError(t, err)
	var got GetEffectiveConfigResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, _ := json.Marshal(result)
		_ = json.Unmarshal(b, &got)
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	assert.NotEmpty(t, got.Values, "null params returns all sections")
}
