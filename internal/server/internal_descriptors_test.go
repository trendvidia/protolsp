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

// These tests pin the end-to-end UX win from shipping internal
// descriptors: opening the LSP's own config files (`.protolsp.pxf`,
// `chain.pxf`) works without any user-side registry. Previously
// every such open produced a spurious "unknown type" diagnostic
// because the user's registry has no reason to publish
// protolsp.v1.WorkspaceConfig or chameleon.v1.ChainSpec.

func TestInternalDescriptors_NoRegistryNoSpuriousUnknownType(t *testing.T) {
	// Server with NO user lookup. Open a .protolsp.pxf-shaped doc.
	// The @type resolves via the internal lookup; validate runs;
	// no "unknown type" diagnostic.
	srv, fc := newServerWithFakeConn(t)
	// Intentionally no srv.lookup assignment.

	const src = `@type protolsp.v1.WorkspaceConfig

registry { address = "x:443" }
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	for _, d := range notes[0].params.Diagnostics {
		assert.NotContains(t, d.Message, "unknown type",
			"internal lookup resolves protolsp.v1.WorkspaceConfig — no spurious unknown-type diagnostic")
	}
}

func TestInternalDescriptors_NoRegistryNoSpuriousUnknownTypeForChainSpec(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// No srv.lookup.

	const src = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"
environments = [ { name = "dev" } ]
layers = []
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/chain.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	for _, d := range notes[0].params.Diagnostics {
		assert.NotContains(t, d.Message, "unknown type",
			"internal lookup resolves chameleon.v1.ChainSpec without a registry")
	}
}

func TestInternalDescriptors_UnknownFieldOnWorkspaceConfigFiresWithoutRegistry(t *testing.T) {
	// Confirm the OTHER direction: when the @type DOES resolve via
	// internal lookup, normal field-level validation runs. A typo'd
	// field on protolsp.v1.WorkspaceConfig produces an unknown-field
	// diagnostic even without a user registry — proving the descriptor
	// is fully wired, not just a stub.
	srv, fc := newServerWithFakeConn(t)
	// No srv.lookup.

	const src = `@type protolsp.v1.WorkspaceConfig

mystery_typo = "value"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	var unknownField *protocol.Diagnostic
	for i := range notes[0].params.Diagnostics {
		d := &notes[0].params.Diagnostics[i]
		if strings.Contains(d.Message, "mystery_typo") {
			unknownField = d
			break
		}
	}
	require.NotNil(t, unknownField,
		"unknown field on internal type still emits a diagnostic")
	assert.Equal(t, "protolsp/schema", unknownField.Source)
}

func TestInternalDescriptors_PreservesNoLookupSilenceForUserDomainTypes(t *testing.T) {
	// The previous "no lookup → no validation" behavior is preserved
	// for user-domain types. Opening a doc whose @type isn't in
	// any lookup (user's or internal) on a server with no user
	// lookup is silent — the user hasn't opted into schema validation.
	srv, fc := newServerWithFakeConn(t)
	// No srv.lookup.

	const src = `@type acme.demo.v1.Greeting

name = "world"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, notes[0].params.Diagnostics,
		"no registry + non-internal @type → silent (matches pre-internal behavior)")
}

func TestInternalDescriptors_UnknownTypeStillFiresWhenUserRegistryConfigured(t *testing.T) {
	// Sanity check: when the user HAS configured a lookup, the
	// "unknown type" diagnostic still fires for @types that neither
	// lookup resolves. The internal fallback doesn't suppress
	// legitimate unknown-type errors.
	srv, fc, _ := newServerWithLookup(t)

	const src = `@type acme.totally.NotAThing

x = 1
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	found := false
	for _, d := range notes[0].params.Diagnostics {
		if strings.Contains(d.Message, "unknown type") {
			found = true
		}
	}
	assert.True(t, found,
		"user has a registry → unknown @type still flags an error")
}

func TestInternalDescriptors_HoverOnWorkspaceConfigField(t *testing.T) {
	// Hovering over a field name inside .protolsp.pxf surfaces the
	// internal descriptor's metadata (kind, defining file). No
	// registry required.
	srv, _ := newServerWithFakeConn(t)
	// No srv.lookup.

	const src = `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "config/chain.pxf"
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, src)))

	// Cursor on `chain_spec` (line 3 of the doc — 0-indexed line 3
	// of the source; the body is on line 2 of the body, line 3
	// counting the @type + blank line).
	md := hoverAt(t, srv, "file:///work/.protolsp.pxf", 3, 2)
	require.NotEmpty(t, md,
		"hover should render on internal-type fields without a user registry")
	assert.Contains(t, md, "chain_spec",
		"hover surfaces the field name from the internal descriptor")
}

func TestInternalDescriptors_CompletionOffersInternalTypesOnAtType(t *testing.T) {
	// On an @type line with no user registry, completion still
	// suggests the internal FQNs so users can complete to
	// protolsp.v1.WorkspaceConfig.
	srv, _ := newServerWithFakeConn(t)
	// No srv.lookup.

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/.protolsp.pxf", 1, "@type \n")))

	list := completionFromHandle(t, srv, "file:///work/.protolsp.pxf", 0, 6)
	require.NotNil(t, list)
	labels := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		labels = append(labels, it.Label)
	}
	assert.Contains(t, labels, "protolsp.v1.WorkspaceConfig")
	assert.Contains(t, labels, "chameleon.v1.ChainSpec")
}
