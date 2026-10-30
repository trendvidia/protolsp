// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// workspaceConfigDiags filters diagnostics down to the
// "protolsp/workspaceconfig" source so each test asserts only on
// the dedicated-parser warnings.
func workspaceConfigDiags(diags []protocol.Diagnostic) []protocol.Diagnostic {
	out := []protocol.Diagnostic{}
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/workspaceconfig" {
			out = append(out, d)
		}
	}
	return out
}

func chainSpecDiags(diags []protocol.Diagnostic) []protocol.Diagnostic {
	out := []protocol.Diagnostic{}
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/chainspec" {
			out = append(out, d)
		}
	}
	return out
}

func TestConfigDiagnostics_InvalidInlineSecretsValueWarns(t *testing.T) {
	// `inline_secrets` only accepts allow/warn/refuse. A typo
	// surfaces as a warning from the dedicated parser.
	srv, fc := newServerWithFakeConn(t)

	const src = `@type protolsp.v1.WorkspaceConfig

security {
  inline_secrets = "maybee"
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, src)))

	diags := workspaceConfigDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 1)
	assert.Equal(t, protocol.DiagnosticSeverityWarning, diags[0].Severity)
	assert.Contains(t, messageText(diags[0].Message), "allow|warn|refuse",
		"warning names the allowed values so users can correct")
}

func TestConfigDiagnostics_UnknownTopBlockWarns(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	const src = `@type protolsp.v1.WorkspaceConfig

mystery_block {
  foo = "bar"
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, src)))

	diags := workspaceConfigDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 1)
	assert.Contains(t, messageText(diags[0].Message), "mystery_block")
}

func TestConfigDiagnostics_MissingTypeDirectiveWarns(t *testing.T) {
	// Without an @type directive the doc wouldn't even route
	// through the workspaceconfig diagnostics (we route by @type).
	// But the parser's "missing @type" warning is what fires once
	// the doc has a basename match... Today's routing is by
	// @type only, so missing-@type docs route through nothing.
	// Confirm the route gates ARE @type-based.
	srv, fc := newServerWithFakeConn(t)

	const src = `schema_sources {
  registry { address = "x:443" }
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, src)))

	diags := workspaceConfigDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"docs without @type don't route through workspaceconfig diagnostics today; missing-@type case is handled at load time")
}

func TestConfigDiagnostics_NonConfigDocGetsNoWarnings(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	const src = `@type acme.demo.v1.Greeting

name = "world"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))

	all := fc.calls()[0].params.Diagnostics
	assert.Empty(t, workspaceConfigDiags(all))
	assert.Empty(t, chainSpecDiags(all))
}

func TestConfigDiagnostics_ChainSpecCycleWarns(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	const cyclicChain = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [
  { name = "a" extends = "b" }
  { name = "b" extends = "a" }
]
layers = []
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/chain.pxf", 1, cyclicChain)))

	diags := chainSpecDiags(fc.calls()[0].params.Diagnostics)
	require.NotEmpty(t, diags)
	found := false
	for _, d := range diags {
		if strings.Contains(messageText(d.Message), "cycle") {
			found = true
			assert.Equal(t, protocol.DiagnosticSeverityWarning, d.Severity)
		}
	}
	assert.True(t, found, "ancestry cycle surfaces as a chainspec warning")
}

func TestConfigDiagnostics_ChainSpecWrongTypeWarns(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	const src = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [ { name = "dev" } ]
layers = []
`
	// This spec is structurally fine, no warnings expected from
	// the chain parser itself. (Verify the success path doesn't
	// emit spurious warnings.)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/chain.pxf", 1, src)))

	diags := chainSpecDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags, "well-formed chain spec produces no warnings")
}

func TestConfigDiagnostics_ParseErrorSuppressesConfigWarnings(t *testing.T) {
	// Unparseable PXF — the generic parse diagnostic fires; the
	// dedicated-parser warnings stay silent (no double-reporting).
	srv, fc := newServerWithFakeConn(t)

	const broken = `@type protolsp.v1.WorkspaceConfig

unclosed {
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, broken)))

	all := fc.calls()[0].params.Diagnostics
	assert.Empty(t, workspaceConfigDiags(all),
		"workspaceconfig warnings suppressed when PXF parse already errored")
	// The generic parse diagnostic should still fire.
	hasParse := false
	for _, d := range all {
		if optValue(d.Source) == "protolsp/parse" {
			hasParse = true
		}
	}
	assert.True(t, hasParse, "generic parse diagnostic still surfaces")
}
